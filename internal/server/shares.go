package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"homeport/internal/firewall"
	"homeport/internal/shares"
	"homeport/internal/state"
	"homeport/internal/storage"
)

// MARK: Shares
//
// The Shares page: folders shared on the home network (SMB), the accounts that sign in to them, and who may read or
// write each one. Shares live in state.json (Sharing); Samba's settings for them are written from there.

func (s *Server) sharesRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/shares", guard(s.listShares))
	mux.HandleFunc("POST /api/shares/install", guard(s.installSamba))
	mux.HandleFunc("POST /api/shares/service", guard(s.sharingService))
	mux.HandleFunc("POST /api/shares", guard(s.addShare))
	mux.HandleFunc("PUT /api/shares/{name}", guard(s.changeShare))
	mux.HandleFunc("DELETE /api/shares/{name}", guard(s.removeShare))
	mux.HandleFunc("POST /api/shares/accounts", guard(s.addShareAccount))
	mux.HandleFunc("POST /api/shares/accounts/{name}/password", guard(s.shareAccountPassword))
	mux.HandleFunc("POST /api/shares/accounts/{name}/enable", guard(s.shareAccountEnable))
	mux.HandleFunc("DELETE /api/shares/accounts/{name}", guard(s.removeShareAccount))
}

func (s *Server) sharing() (list []state.Share, ours []string) {
	s.Store.Read(func(d *state.Data) {
		sh := d.Sharing()
		for _, x := range sh.Shares {
			x.Access = cloneMap(x.Access)
			list = append(list, x)
		}
		ours = append(ours, sh.Accounts...)
	})
	return list, ours
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *Server) listShares(w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil {
		writeError(w, http.StatusBadRequest, errors.New("not available here"))
		return
	}
	list, ours := s.sharing()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	st := s.Shares.Status(ctx, ours)
	// Where new folders can go: /srv/shares on the system drive, <drive>/shares on the others.
	type place struct {
		Label string `json:"label"`
		Base  string `json:"base"`
		Free  int64  `json:"free"`
	}
	places := []place{}
	if st.Supported {
		drives, _ := storage.Drives(ctx, s.stateDir())
		for _, d := range drives {
			if !d.Mounted || d.Mount == "" || (!d.Usable && !d.System) {
				continue
			}
			base := filepath.Join(d.Mount, "shares")
			label := cmp.Or(d.Label, d.Model, d.Device) + " (" + d.Mount + ")"
			if d.System {
				base, label = "/srv/shares", "The system drive"
			}
			places = append(places, place{Label: label, Base: base, Free: d.Free})
		}
	}
	if list == nil {
		list = []state.Share{}
	}
	// How to reach it: this machine's own .local name (Avahi announces it) and its addresses.
	addrs := []string{}
	if h, err := os.Hostname(); err == nil && h != "" {
		addrs = append(addrs, strings.TrimSuffix(h, ".local")+".local")
	}
	addrs = append(addrs, s.localAddrs()...)
	writeJSON(w, http.StatusOK, map[string]any{"status": st, "shares": list, "places": places, "addresses": addrs})
}

func (s *Server) installSamba(w http.ResponseWriter, r *http.Request) {
	if s.Shares == nil {
		writeError(w, http.StatusBadRequest, errors.New("not available here"))
		return
	}
	j := s.jobs.start("install file sharing", func(ctx context.Context, j *job) error {
		fmt.Fprintln(j, "Installing Samba (file sharing)…")
		if err := s.Shares.Install(ctx, j); err != nil {
			return err
		}
		list, _ := s.sharing()
		return s.Shares.Apply(ctx, list)
	})
	writeJSON(w, http.StatusAccepted, j.view())
}

func (s *Server) sharingService(w http.ResponseWriter, r *http.Request) {
	var in struct {
		On bool `json:"on"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Shares.Service(r.Context(), in.On); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	log.Printf("Shares: %s turned file sharing %s", actor(r).Name, map[bool]string{true: "on", false: "off"}[in.On])
	w.WriteHeader(http.StatusNoContent)
}

type shareIn struct {
	Name    string            `json:"name"`
	Path    string            `json:"path"`
	Guests  bool              `json:"guests"`
	Off     bool              `json:"off"`
	Access  map[string]string `json:"access"`
	Comment string            `json:"comment"`
}

// saveShares stores the list and writes Samba's settings; on failure the old list stays.
func (s *Server) saveShares(ctx context.Context, list []state.Share) error {
	if err := s.Shares.Apply(ctx, list); err != nil {
		old, _ := s.sharing()
		_ = s.Shares.Apply(ctx, old)
		return err
	}
	return s.Store.Update(func(d *state.Data) error {
		d.Sharing().Shares = list
		return nil
	})
}

func (s *Server) addShare(w http.ResponseWriter, r *http.Request) {
	var in shareIn
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	list, ours := s.sharing()
	sh := state.Share{Name: strings.TrimSpace(in.Name), Path: strings.TrimSpace(in.Path), Guests: in.Guests, Access: in.Access, Comment: in.Comment}
	if sh.Access == nil {
		sh.Access = map[string]string{}
	}
	if err := s.Shares.CheckShare(sh, list, s.Shares.Status(r.Context(), ours).Accounts); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	owner, err := s.Shares.Prepare(r.Context(), sh.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sh.Owner = owner
	if err := s.saveShares(r.Context(), append(list, sh)); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.openSMB(r.Context())
	log.Printf("Shares: %s shared %s as %s", actor(r).Name, sh.Path, sh.Name)
	writeJSON(w, http.StatusCreated, sh)
}

// openSMB lets the home network reach file sharing (port 445) when the firewall is on.
func (s *Server) openSMB(ctx context.Context) {
	if s.Firewall == nil {
		return
	}
	st := s.Firewall.Status(ctx)
	if !st.Active {
		return
	}
	for _, n := range []string{"192.168.0.0/16", "10.0.0.0/8", "172.16.0.0/12"} {
		if slices.ContainsFunc(st.Rules, func(x firewall.Rule) bool { return x.Port == "445" && x.From == n }) {
			continue
		}
		if err := s.Firewall.OpenPrivate(ctx, "445", "tcp", n, "file sharing (SMB)"); err != nil {
			log.Printf("Shares: couldn't open the firewall for file sharing: %v", err)
		}
	}
}

func (s *Server) changeShare(w http.ResponseWriter, r *http.Request) {
	var in shareIn
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("name")
	list, ours := s.sharing()
	i := slices.IndexFunc(list, func(x state.Share) bool { return x.Name == name })
	if i < 0 {
		writeError(w, http.StatusNotFound, fmt.Errorf("no share called %s", name))
		return
	}
	sh := list[i]
	sh.Guests, sh.Off, sh.Comment = in.Guests, in.Off, in.Comment
	if in.Access != nil {
		sh.Access = in.Access
	}
	rest := slices.Delete(slices.Clone(list), i, i+1)
	if err := s.Shares.CheckShare(sh, rest, s.Shares.Status(r.Context(), ours).Accounts); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	list[i] = sh
	if err := s.saveShares(r.Context(), list); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	log.Printf("Shares: %s changed %s", actor(r).Name, name)
	writeJSON(w, http.StatusOK, sh)
}

// removeShare stops sharing a folder. Its files stay where they are.
func (s *Server) removeShare(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	list, _ := s.sharing()
	i := slices.IndexFunc(list, func(x state.Share) bool { return x.Name == name })
	if i < 0 {
		writeError(w, http.StatusNotFound, fmt.Errorf("no share called %s", name))
		return
	}
	path := list[i].Path
	s.Shares.Close(r.Context(), name)
	if err := s.saveShares(r.Context(), slices.Delete(list, i, i+1)); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	log.Printf("Shares: %s stopped sharing %s (%s); its files stay", actor(r).Name, name, path)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) addShareAccount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	in.Name = strings.ToLower(strings.TrimSpace(in.Name))
	made, err := s.Shares.AddAccount(r.Context(), in.Name, in.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if made {
		_ = s.Store.Update(func(d *state.Data) error {
			if sh := d.Sharing(); !slices.Contains(sh.Accounts, in.Name) {
				sh.Accounts = append(sh.Accounts, in.Name)
			}
			return nil
		})
	}
	log.Printf("Shares: %s added the account %s", actor(r).Name, in.Name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) shareAccountPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.knownAccount(r); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := s.Shares.SetPassword(r.Context(), r.PathValue("name"), in.Password); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	log.Printf("Shares: %s changed the password of %s", actor(r).Name, r.PathValue("name"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) shareAccountEnable(w http.ResponseWriter, r *http.Request) {
	var in struct {
		On bool `json:"on"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.knownAccount(r); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err := s.Shares.Enable(r.Context(), r.PathValue("name"), in.On); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeShareAccount removes a sign-in (and it from every share's access list).
func (s *Server) removeShareAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.knownAccount(r); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	list, ours := s.sharing()
	for i := range list {
		delete(list[i].Access, name)
	}
	if err := s.saveShares(r.Context(), list); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Shares.RemoveAccount(r.Context(), name, slices.Contains(ours, name)); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	_ = s.Store.Update(func(d *state.Data) error {
		sh := d.Sharing()
		sh.Accounts = slices.DeleteFunc(sh.Accounts, func(x string) bool { return x == name })
		return nil
	})
	log.Printf("Shares: %s removed the account %s", actor(r).Name, name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) knownAccount(r *http.Request) error {
	_, ours := s.sharing()
	name := r.PathValue("name")
	if !slices.ContainsFunc(s.Shares.Status(r.Context(), ours).Accounts, func(a shares.Account) bool { return a.Name == name }) {
		return fmt.Errorf("no account called %s", name)
	}
	return nil
}

// RefreshShares writes Samba's settings for the shares again when Homeport starts, so a new version's fixes (folders
// opened for passing through, say) reach shares made before it.
func (s *Server) RefreshShares(ctx context.Context) {
	if s.Shares == nil {
		return
	}
	list, _ := s.sharing()
	if len(list) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := s.Shares.Apply(ctx, list); err != nil {
		log.Printf("Shares: couldn't write Samba's settings again: %v", err)
	}
}
