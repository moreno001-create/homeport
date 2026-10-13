package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"homeport/internal/sysload"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"homeport/internal/apps"
	"homeport/internal/auth"
	"homeport/internal/catalog"
	"homeport/internal/certs"
	"homeport/internal/dyndns"
	"homeport/internal/source"
	"homeport/internal/state"
)

type actorKey struct{}

func actor(r *http.Request) state.User {
	u, _ := r.Context().Value(actorKey{}).(state.User)
	return u
}

// browserAdmin lets through signed-in admins on the main domain. Changes also need the X-Homeport header, which another
// site's page can't send, so a malicious page can't make an admin's browser change things.
func (s *Server) browserAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isMainHost(host(r)) {
			http.NotFound(w, r)
			return
		}
		u, _, ok := s.sessionUser(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errors.New("sign in first"))
			return
		}
		if !u.Admin {
			writeError(w, http.StatusForbidden, errors.New("only admins can do this"))
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Homeport") != "1" {
			writeError(w, http.StatusForbidden, errors.New("missing X-Homeport header"))
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, u)))
	}
}

func (s *Server) api(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/overview", guard(s.overview))
	mux.HandleFunc("PUT /api/settings", guard(s.putSettings))
	mux.HandleFunc("GET /api/system", guard(s.systemSpecs))
	mux.HandleFunc("GET /api/users", guard(s.listUsers))
	mux.HandleFunc("POST /api/users", guard(s.addUser))
	mux.HandleFunc("PATCH /api/users/{name}", guard(s.changeUser))
	mux.HandleFunc("DELETE /api/users/{name}", guard(s.deleteUser))
	mux.HandleFunc("GET /api/domains", guard(s.listDomains))
	mux.HandleFunc("POST /api/domains", guard(s.addDomain))
	mux.HandleFunc("DELETE /api/domains/{name}", guard(s.deleteDomain))
	mux.HandleFunc("PUT /api/domains/{name}/certificate", guard(s.setCertificate))
	mux.HandleFunc("POST /api/domains/free", guard(s.freeDomain))
	mux.HandleFunc("GET /api/certificates", guard(s.listCertificates))
	mux.HandleFunc("GET /api/catalog", guard(s.listCatalog))
	mux.HandleFunc("GET /api/apps", guard(s.listApps))
	mux.HandleFunc("POST /api/apps", guard(s.installApp))
	mux.HandleFunc("POST /api/apps/draft", guard(s.draftApp))
	mux.HandleFunc("POST /api/apps/check", guard(s.checkApp))
	mux.HandleFunc("POST /api/apps/{id}/update", guard(s.updateApp))
	mux.HandleFunc("POST /api/apps/{id}/address", guard(s.changeAddress))
	mux.HandleFunc("POST /api/apps/{id}/internet", guard(s.setInternet))
	mux.HandleFunc("GET /api/addresses", guard(s.addressBases))
	mux.HandleFunc("POST /api/apps/{id}/start", guard(s.appAction("start")))
	mux.HandleFunc("POST /api/apps/{id}/stop", guard(s.appAction("stop")))
	mux.HandleFunc("POST /api/apps/{id}/restart", guard(s.appAction("restart")))
	mux.HandleFunc("DELETE /api/apps/{id}", guard(s.removeApp))
	mux.HandleFunc("PUT /api/apps/{id}/access", guard(s.setAccess))
	mux.HandleFunc("GET /api/apps/{id}/secrets", guard(s.appSecrets))
	mux.HandleFunc("GET /api/apps/{id}/notes", guard(s.appNotes))
	mux.HandleFunc("PUT /api/apps/{id}/notes", guard(s.setAppNotes))
	mux.HandleFunc("GET /api/jobs/{id}", guard(s.getJob))
	s.serviceRoutes(mux, guard)
	s.firewallRoutes(mux, guard)
	s.backupRoutes(mux, guard)
	s.internetRoutes(mux, guard)
	mux.HandleFunc("GET /api/approvals", guard(s.listApprovals))
	mux.HandleFunc("POST /api/approvals/{id}/{decision}", guard(s.decideApproval))
	mux.HandleFunc("GET /api/audit", guard(s.auditLog))
	mux.HandleFunc("GET /api/memory", guard(s.memory))
	s.chatRoutes(mux, guard)
	s.modelRoutes(mux, guard)
	s.selfCareRoutes(mux, guard)
}

// MARK: AI helpers

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	client := "mcp"
	if c := headerSafe(r.Header.Get("X-Homeport-Client")); c != "" {
		if len(c) > 40 {
			c = c[:40]
		}
		client = "mcp:" + c
	}
	out := s.Assist.HandleMCP(r.Context(), client, body)
	if out == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Assist.Approvals.Pending())
}

func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request) {
	d := r.PathValue("decision")
	if d != "approve" && d != "deny" {
		writeError(w, http.StatusNotFound, errors.New("approve or deny"))
		return
	}
	if err := s.Assist.Approvals.Decide(r.PathValue("id"), d == "approve", actor(r).Name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) auditLog(w http.ResponseWriter, r *http.Request) {
	n := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		n = v
	}
	entries, err := s.Assist.Audit.Recent(n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (s *Server) memory(w http.ResponseWriter, r *http.Request) {
	text, err := s.Assist.Memory.Read()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"memory": text})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad request: %w", err)
	}
	return nil
}

// MARK: Overview and settings

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	var out struct {
		Version  string         `json:"version"`
		Settings state.Settings `json:"settings"`
		Users    int            `json:"users"`
		Domains  int            `json:"domains"`
		Apps     int            `json:"apps"`
		Me       string         `json:"me"`
	}
	s.Store.Read(func(d *state.Data) {
		out.Settings = *d.Settings()
		out.Users, out.Domains, out.Apps = len(d.Users()), len(d.Domains()), len(d.Apps())
	})
	out.Version, out.Me = s.Version, actor(r).Name
	writeJSON(w, http.StatusOK, out)
}

// systemSpecs is what this machine is (OS, processor, memory, disk, graphics, uptime, versions), and how busy it is.
func (s *Server) systemSpecs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{"specs": sysload.ReadSpecs(ctx, filepath.Dir(s.Apps.Root)), "load": s.load.Read(ctx),
		"addresses": s.localAddrs()})
}

func (s *Server) localAddrs() []string {
	if s.LocalAddrs == nil {
		return []string{}
	}
	return s.LocalAddrs()
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MainDomain      *string        `json:"mainDomain"`
		ACMEEmail       *string        `json:"acmeEmail"`
		TLS             *state.TLSMode `json:"tls"`
		AssistantShell  *bool          `json:"assistantShell"`
		ApprovalMinutes *int           `json:"approvalMinutes"`
		Terminal        *bool          `json:"terminal"`
		DailyCheck      *bool          `json:"dailyCheck"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if in.TLS != nil && *in.TLS != state.TLSLetsEncrypt && *in.TLS != state.TLSInternal {
		writeError(w, http.StatusBadRequest, errors.New(`tls is "letsencrypt" or "internal"`))
		return
	}
	if in.ACMEEmail != nil && *in.ACMEEmail != "" && !strings.Contains(*in.ACMEEmail, "@") {
		writeError(w, http.StatusBadRequest, errors.New("that isn't an email address"))
		return
	}
	if in.MainDomain != nil {
		*in.MainDomain = strings.ToLower(strings.TrimSpace(*in.MainDomain))
		if err := auth.ValidDomain(*in.MainDomain); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if in.Terminal != nil {
		if s.SetTerminal == nil {
			writeError(w, http.StatusBadRequest, errors.New("the web terminal is only on Linux servers"))
			return
		}
		if err := s.SetTerminal(r.Context(), *in.Terminal); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	err := s.Store.Update(func(d *state.Data) error {
		st := d.Settings()
		if in.Terminal != nil {
			st.Terminal = *in.Terminal
		}
		if in.DailyCheck != nil {
			st.NoDailyCheck = !*in.DailyCheck
		}
		if in.MainDomain != nil && *in.MainDomain != st.MainDomain {
			// Apps that sign in with Homeport were configured with the old address (their OIDC issuer).
			for _, a := range d.Apps() {
				if a.OIDC != nil {
					return fmt.Errorf("%s signs in with Homeport at https://%s; remove it before changing the main domain, then install it again", a.Name, st.MainDomain)
				}
			}
			if other, taken := d.AppOn(*in.MainDomain); taken {
				return fmt.Errorf("%s already serves %s", *in.MainDomain, other.Name)
			}
			st.MainDomain = *in.MainDomain
			if !d.HasDomain(st.MainDomain) {
				d.AddDomain(state.Domain{Name: st.MainDomain, Added: time.Now()})
			}
		}
		if in.ACMEEmail != nil {
			st.ACMEEmail = *in.ACMEEmail
		}
		if in.TLS != nil {
			st.TLS = *in.TLS
		}
		if in.AssistantShell != nil {
			st.AssistantShell = *in.AssistantShell
		}
		if in.ApprovalMinutes != nil {
			if *in.ApprovalMinutes < 1 || *in.ApprovalMinutes > 240 {
				return errors.New("approvals can wait 1 to 240 minutes")
			}
			st.ApprovalMinutes = *in.ApprovalMinutes
		}
		return nil
	})
	if err == nil {
		err = s.ApplyProxy(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.overview(w, r)
}

// initServer sets up a fresh server: main domain, certificates and the first admin (root socket only).
func (s *Server) initServer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MainDomain string        `json:"mainDomain"`
		ACMEEmail  string        `json:"acmeEmail"`
		TLS        state.TLSMode `json:"tls"`
		Admin      newUser       `json:"admin"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	in.MainDomain = strings.ToLower(strings.TrimSpace(in.MainDomain))
	if err := auth.ValidDomain(in.MainDomain); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if in.TLS == "" {
		in.TLS = state.TLSLetsEncrypt
	}
	in.Admin.Admin = true
	user, err := in.Admin.build()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	err = s.Store.Update(func(d *state.Data) error {
		if d.Settings().MainDomain != "" {
			return fmt.Errorf("this server is already set up for %s", d.Settings().MainDomain)
		}
		*d.Settings() = state.Settings{MainDomain: in.MainDomain, ACMEEmail: in.ACMEEmail, TLS: in.TLS}
		if !d.HasDomain(in.MainDomain) {
			d.AddDomain(state.Domain{Name: in.MainDomain, Added: time.Now()})
		}
		if _, exists := d.User(user.Name); exists {
			return fmt.Errorf("there's already a user called %s", user.Name)
		}
		d.AddUser(user)
		return nil
	})
	if err == nil {
		err = s.ApplyProxy(r.Context())
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"portal": "https://" + in.MainDomain + "/", "admin": "https://" + in.MainDomain + "/admin"})
}

// MARK: Users

type publicUser struct {
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName"`
	Email       string    `json:"email,omitempty"`
	Groups      []string  `json:"groups"`
	Admin       bool      `json:"admin"`
	Created     time.Time `json:"created"`
}

func public(u state.User) publicUser {
	g := u.Groups
	if g == nil {
		g = []string{}
	}
	return publicUser{u.Name, u.DisplayName, u.Email, g, u.Admin, u.Created}
}

type newUser struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Email       string   `json:"email"`
	Password    string   `json:"password"`
	Groups      []string `json:"groups"`
	Admin       bool     `json:"admin"`
}

func (n newUser) build() (state.User, error) {
	n.Name = strings.ToLower(strings.TrimSpace(n.Name))
	if err := auth.ValidUser(n.Name); err != nil {
		return state.User{}, err
	}
	if err := auth.ValidPassword(n.Password); err != nil {
		return state.User{}, err
	}
	groups, err := cleanGroups(n.Groups)
	if err != nil {
		return state.User{}, err
	}
	hash, err := auth.HashPassword(n.Password)
	if err != nil {
		return state.User{}, err
	}
	if strings.TrimSpace(n.DisplayName) == "" {
		n.DisplayName = n.Name
	}
	return state.User{Name: n.Name, DisplayName: strings.TrimSpace(n.DisplayName), Email: strings.TrimSpace(n.Email),
		Password: hash, Groups: groups, Admin: n.Admin, Created: time.Now()}, nil
}

func cleanGroups(in []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, g := range in {
		g = strings.ToLower(strings.TrimSpace(g))
		if g == "" || seen[g] {
			continue
		}
		if g == "all" || g == "admins" {
			return nil, fmt.Errorf("%q is built in (every user, or every admin); pick another group name", g)
		}
		if err := auth.ValidGroup(g); err != nil {
			return nil, err
		}
		seen[g] = true
		out = append(out, g)
	}
	sort.Strings(out)
	return out, nil
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	var out []publicUser
	s.Store.Read(func(d *state.Data) {
		for _, u := range d.Users() {
			out = append(out, public(u))
		}
	})
	if out == nil {
		out = []publicUser{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) addUser(w http.ResponseWriter, r *http.Request) {
	var in newUser
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := in.build()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	err = s.Store.Update(func(d *state.Data) error {
		if _, exists := d.User(u.Name); exists {
			return fmt.Errorf("there's already a user called %s", u.Name)
		}
		d.AddUser(u)
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, public(u))
}

func (s *Server) changeUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in struct {
		DisplayName *string   `json:"displayName"`
		Email       *string   `json:"email"`
		Password    *string   `json:"password"`
		Groups      *[]string `json:"groups"`
		Admin       *bool     `json:"admin"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var hash string
	if in.Password != nil {
		if err := auth.ValidPassword(*in.Password); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		hash, _ = auth.HashPassword(*in.Password)
	}
	var groups []string
	if in.Groups != nil {
		var err error
		if groups, err = cleanGroups(*in.Groups); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	var out publicUser
	err := s.Store.Update(func(d *state.Data) error {
		u, ok := d.User(name)
		if !ok {
			return state.ErrNotFound
		}
		if in.Admin != nil && !*in.Admin && u.Admin && countAdmins(d) == 1 {
			return errors.New("this is the only admin; make someone else an admin first")
		}
		if in.DisplayName != nil {
			u.DisplayName = strings.TrimSpace(*in.DisplayName)
		}
		if in.Email != nil {
			u.Email = strings.TrimSpace(*in.Email)
		}
		if in.Groups != nil {
			u.Groups = groups
		}
		if in.Admin != nil {
			u.Admin = *in.Admin
		}
		if hash != "" {
			u.Password = hash
			d.DeleteSessionsOf(name) // a new password signs them out everywhere
		}
		out = public(*u)
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func countAdmins(d *state.Data) int {
	n := 0
	for _, u := range d.Users() {
		if u.Admin {
			n++
		}
	}
	return n
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.Store.Update(func(d *state.Data) error {
		u, ok := d.User(name)
		if !ok {
			return state.ErrNotFound
		}
		if u.Admin && countAdmins(d) == 1 {
			return errors.New("this is the only admin; make someone else an admin first")
		}
		d.DeleteUser(name)
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// MARK: Domains

func (s *Server) listDomains(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Name   string         `json:"name"`
		Main   bool           `json:"main"`
		App    string         `json:"app,omitempty"`
		Added  time.Time      `json:"added"`
		Cert   state.CertMode `json:"cert"` // where its certificate comes from (the server's setting if it has none)
		Own    bool           `json:"ownCert"`
		DynDNS *state.DynDNS  `json:"dyndns,omitempty"`
	}
	out := []row{}
	s.Store.Read(func(d *state.Data) {
		for _, x := range d.Domains() {
			rw := row{Name: x.Name, Main: x.Name == d.Settings().MainDomain, Added: x.Added, Cert: effectiveCert(*d.Settings(), x),
				Own: x.Cert != "", DynDNS: x.DynDNS}
			if a, ok := d.AppOn(x.Name); ok {
				rw.App = a.Name
			}
			out = append(out, rw)
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) addDomain(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := strings.ToLower(strings.TrimSpace(in.Name))
	if err := auth.ValidDomain(name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	err := s.Store.Update(func(d *state.Data) error {
		if d.HasDomain(name) {
			return fmt.Errorf("%s is already added", name)
		}
		d.AddDomain(state.Domain{Name: name, Added: time.Now()})
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": name})
}

func (s *Server) deleteDomain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := s.Store.Update(func(d *state.Data) error {
		if name == d.Settings().MainDomain {
			return errors.New("that's the main domain (portal and admin panel)")
		}
		if a, ok := d.AppOn(name); ok {
			return fmt.Errorf("%s is serving %s; remove the app first", name, a.Name)
		}
		if !d.DeleteDomain(name) {
			return state.ErrNotFound
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.CertDir != "" {
		certs.Remove(s.CertDir, name)
	}
	if s.DynDNS != nil {
		s.DynDNS.Forget(name)
	}
	w.WriteHeader(http.StatusNoContent)
}

// effectiveCert is where a domain's certificate comes from.
func effectiveCert(st state.Settings, d state.Domain) state.CertMode {
	return state.CertFor(st, d.Name, d.Cert)
}

// setCertificate picks one domain's certificate: "letsencrypt", "internal", "custom" (with the PEM certificate chain
// and key) or "" (follow the server's setting).
func (s *Server) setCertificate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in struct {
		Mode state.CertMode `json:"mode"`
		Cert string         `json:"cert"`
		Key  string         `json:"key"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	switch in.Mode {
	case "", state.CertLetsEncrypt, state.CertInternal:
	case state.CertCustom:
		if s.CertDir == "" {
			writeError(w, http.StatusBadRequest, errors.New("uploaded certificates aren't set up on this server"))
			return
		}
		if _, err := certs.Check([]byte(in.Cert), []byte(in.Key), name, time.Now()); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	default:
		writeError(w, http.StatusBadRequest, errors.New(`the certificate is "letsencrypt", "internal", "custom" or "" (the server's setting)`))
		return
	}
	var found bool
	s.Store.Read(func(d *state.Data) { _, found = d.Domain(name) })
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("%s isn't one of the domains", name))
		return
	}
	if in.Mode == state.CertLetsEncrypt && state.PrivateName(name) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("Let's Encrypt can't certify %s: it's a name for your home network, not the internet. It uses Caddy's own certificates (browsers warn once)", name))
		return
	}
	if in.Mode == state.CertCustom {
		if err := certs.Save(s.CertDir, name, []byte(in.Cert), []byte(in.Key)); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	var before state.CertMode
	err := s.Store.Update(func(d *state.Data) error {
		x, ok := d.Domain(name)
		if !ok {
			return state.ErrNotFound
		}
		before, x.Cert = x.Cert, in.Mode
		return nil
	})
	if err == nil {
		if err = s.ApplyProxy(r.Context()); err != nil {
			// Caddy didn't take it: put the old choice back so the next change doesn't trip on it.
			s.Store.Update(func(d *state.Data) error {
				if x, ok := d.Domain(name); ok {
					x.Cert = before
				}
				return nil
			})
			s.ApplyProxy(r.Context())
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if in.Mode != state.CertCustom && s.CertDir != "" {
		certs.Remove(s.CertDir, name)
	}
	w.WriteHeader(http.StatusNoContent)
}

// listCertificates looks at the certificate each served domain (the main one and the apps') really has, the way a
// browser would, and where its DNS points.
func (s *Server) listCertificates(w http.ResponseWriter, r *http.Request) {
	type row struct {
		Name string         `json:"name"`
		Cert state.CertMode `json:"cert"`
		DNS  []string       `json:"dns"` // the addresses its name resolves to
		Live certs.Status   `json:"live"`
	}
	var out []row
	s.Store.Read(func(d *state.Data) {
		st := *d.Settings()
		served := map[string]bool{st.MainDomain: true}
		for _, a := range d.Apps() {
			served[a.Domain] = true
		}
		for _, x := range d.Domains() {
			if served[x.Name] {
				out = append(out, row{Name: x.Name, Cert: effectiveCert(st, x)})
			}
		}
	})
	addr := s.HTTPSAddr
	if addr == "" {
		addr = "127.0.0.1:443"
	}
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func(x *row) {
			defer wg.Done()
			x.Live = certs.Live(r.Context(), addr, x.Name, nil)
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			x.DNS, _ = net.DefaultResolver.LookupHost(ctx, x.Name)
			if x.DNS == nil {
				x.DNS = []string{}
			}
		}(&out[i])
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if out == nil {
		out = []row{}
	}
	writeJSON(w, http.StatusOK, out)
}

// freeDomain sets up a free domain from deSEC (name.dedyn.io) or DuckDNS (name.duckdns.org) with the token from the
// admin's account there, and keeps it pointed at this server.
func (s *Server) freeDomain(w http.ResponseWriter, r *http.Request) {
	if s.DynDNS == nil {
		writeError(w, http.StatusBadRequest, errors.New("free domains aren't set up on this server"))
		return
	}
	var in struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
		Token    string `json:"token"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name, err := dyndns.FullName(in.Provider, in.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.DynDNS.Register(r.Context(), in.Provider, name, in.Token); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": name})
}

// MARK: Apps

func (s *Server) listCatalog(w http.ResponseWriter, r *http.Request) {
	all, err := catalog.All()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	type entry struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Category    string `json:"category"`
		Website     string `json:"website"`
		SignIn      string `json:"signIn"`
	}
	out := make([]entry, 0, len(all))
	for _, a := range all {
		out = append(out, entry{a.ID, a.Name, a.Description, a.Category, a.Website, string(a.SignIn)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	type row struct {
		ID        string        `json:"id"`
		Catalog   string        `json:"catalog"`
		Name      string        `json:"name"`
		Domain    string        `json:"domain"`
		Access    state.Access  `json:"access"`
		Status    string        `json:"status"`
		Installed time.Time     `json:"installed"`
		Updated   *time.Time    `json:"updated,omitempty"`
		HasSecret bool          `json:"hasSecrets"`
		Source    *state.Source `json:"source,omitempty"`
		Internet  bool          `json:"internet"` // on a public name (not a home-network one)
	}
	var list []state.App
	s.Store.Read(func(d *state.Data) { list = d.Apps() })
	out := make([]row, len(list))
	var wg sync.WaitGroup
	for i, a := range list {
		out[i] = row{ID: a.ID, Catalog: a.Catalog, Name: a.Name, Domain: a.Domain, Access: a.Access, Installed: a.Installed, HasSecret: len(a.Secrets) > 0, Source: a.Source,
			Internet: !state.PrivateName(a.Domain)}
		if !a.Updated.IsZero() {
			t := a.Updated
			out[i].Updated = &t
		}
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			out[i].Status = s.Apps.Status(ctx, id)
		}(i, a.ID)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) installApp(w http.ResponseWriter, r *http.Request) {
	var in apps.InstallRequest
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	who := actor(r)
	installer := who
	if who.Name == "root" { // from the command line: the first admin owns it
		s.Store.Read(func(d *state.Data) {
			for _, u := range d.Users() {
				if u.Admin {
					installer = u
					break
				}
			}
		})
	}
	what := in.Catalog
	if in.Manifest != "" {
		// Refused before it starts, so the admin sees why straight away.
		if _, err := apps.Outside(in.Manifest); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if in.Source == nil {
			writeError(w, http.StatusBadRequest, errors.New("an app from outside the catalog needs its source"))
			return
		}
		what = in.Source.URL
	}
	job := s.jobs.start("install "+what, func(ctx context.Context, j *job) error {
		_, err := s.Apps.Install(ctx, in, installer, j)
		return err
	})
	writeJSON(w, http.StatusAccepted, job.view())
}

// draftApp reads an app from a GitHub repository and prepares its manifest: the repository's own homeport.yaml, or
// one a model writes from its compose file. Nothing is installed; the admin reviews it first. The job's result is a
// draftResult.
func (s *Server) draftApp(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
		Use string `json:"use"` // "claude", "local" or "" (Claude if there's a key)
		// AI asks a model even when the repository has its own manifest.
		AI bool `json:"ai"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	repo, err := source.ParseURL(in.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	j := s.jobs.start("read "+repo.URL(), func(ctx context.Context, j *job) error {
		fmt.Fprintf(j, "Reading %s…\n", repo.URL())
		files, err := source.Fetch(ctx, repo)
		if err != nil {
			return err
		}
		fmt.Fprintf(j, "Commit %.12s.\n", files.Commit)
		res := draftResult{Source: state.Source{URL: repo.URL(), Commit: files.Commit}, Compose: files.ComposeName}
		for n := range files.Extra {
			res.Files = append(res.Files, n)
		}
		sort.Strings(res.Files)
		if files.Manifest != "" && !in.AI {
			fmt.Fprintln(j, "It has its own homeport.yaml.")
			res.Manifest, res.Source.By, res.Review = files.Manifest, "repository", review(files.Manifest)
		} else {
			if s.Chat == nil {
				return errors.New("there's no assistant on this server to write the manifest")
			}
			fmt.Fprintf(j, "It has %s and no homeport.yaml, so the assistant writes one.\n", cmp.Or(files.ComposeName, "a homeport.yaml"))
			d, err := s.Chat.DraftApp(ctx, files, in.Use, func(line string) { fmt.Fprintln(j, line) })
			if err != nil {
				return err
			}
			res.Manifest, res.Source.By, res.Review = d.Manifest, d.By, d.Review
		}
		if res.Review.OK() {
			fmt.Fprintln(j, "It passed Homeport's checks. Review it before installing.")
		} else {
			fmt.Fprintln(j, "It doesn't pass Homeport's checks yet: see the problems below.")
		}
		j.setResult(res)
		return nil
	})
	writeJSON(w, http.StatusAccepted, j.view())
}

type draftResult struct {
	Source   state.Source   `json:"source"`
	Manifest string         `json:"manifest"`
	Review   catalog.Review `json:"review"`
	Compose  string         `json:"compose,omitempty"` // which compose file it was made from
	Files    []string       `json:"files"`             // the other files read
}

// review checks a manifest, with a parse error as its problem.
func review(manifest string) catalog.Review {
	entry, err := catalog.Parse(manifest)
	if err != nil {
		return catalog.Review{Problems: []string{err.Error()}}
	}
	return entry.Check()
}

// checkApp checks a manifest an admin edited.
func (s *Server) checkApp(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Manifest string `json:"manifest"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, review(in.Manifest))
}

// addressBases are the names apps go under: on the internet (a free name, or a public main domain) and at home.
func (s *Server) addressBases(w http.ResponseWriter, r *http.Request) {
	var public, home string
	s.Store.Read(func(d *state.Data) { public, home = d.PublicBase(), d.HomeBase() })
	writeJSON(w, http.StatusOK, map[string]string{"public": public, "home": home})
}

// changeAddress moves an app to another address (a job: the app restarts with it).
func (s *Server) changeAddress(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Domain string `json:"domain"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	j := s.jobs.start("address "+id, func(ctx context.Context, j *job) error { return s.Apps.ChangeAddress(ctx, id, in.Domain, j) })
	writeJSON(w, http.StatusAccepted, j.view())
}

// setInternet moves an app onto the internet (app.<public base>) or back home (app.<home base>), keeping its name.
func (s *Server) setInternet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		On bool `json:"on"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	var current, base string
	s.Store.Read(func(d *state.Data) {
		if a, ok := d.App(id); ok {
			current = a.Domain
		}
		base = d.HomeBase()
		if in.On {
			base = d.PublicBase()
		}
	})
	switch {
	case current == "":
		writeError(w, http.StatusNotFound, state.ErrNotFound)
		return
	case base == "" && in.On:
		writeError(w, http.StatusBadRequest, errors.New("there's no internet name yet: get a free one under Internet access (DuckDNS or deSEC) first"))
		return
	case base == "":
		writeError(w, http.StatusBadRequest, errors.New("there's no home name (like homeport.local) to move it to: add one under Domains"))
		return
	}
	label, _, _ := strings.Cut(current, ".")
	domain := label + "." + base
	j := s.jobs.start("address "+id, func(ctx context.Context, j *job) error { return s.Apps.ChangeAddress(ctx, id, domain, j) })
	writeJSON(w, http.StatusAccepted, j.view())
}

// appAction starts, stops or restarts a whole app.
func (s *Server) appAction(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()
		if err := s.Apps.AppAction(ctx, r.PathValue("id"), action); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) updateApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job := s.jobs.start("update "+id, func(ctx context.Context, j *job) error { return s.Apps.Update(ctx, id, j) })
	writeJSON(w, http.StatusAccepted, job.view())
}

func (s *Server) removeApp(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	purge := r.URL.Query().Get("purge") == "1"
	job := s.jobs.start("remove "+id, func(ctx context.Context, j *job) error { return s.Apps.Remove(ctx, id, purge, j) })
	writeJSON(w, http.StatusAccepted, job.view())
}

func (s *Server) setAccess(w http.ResponseWriter, r *http.Request) {
	var in state.Access
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Apps.SetAccess(r.Context(), r.PathValue("id"), in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, in)
}

func (s *Server) appSecrets(w http.ResponseWriter, r *http.Request) {
	var out map[string]string
	found := false
	s.Store.Read(func(d *state.Data) {
		if a, ok := d.App(r.PathValue("id")); ok {
			found = true
			out = map[string]string{}
			for k, v := range a.Secrets {
				out[k] = v
			}
		}
	})
	if !found {
		writeError(w, http.StatusNotFound, state.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// appNotes is everything to look back at about an installed app: its setup notes, the passwords and keys Homeport made
// for it, and the admin's own notes.
func (s *Server) appNotes(w http.ResponseWriter, r *http.Request) {
	var app state.App
	found := false
	s.Store.Read(func(d *state.Data) {
		if a, ok := d.App(r.PathValue("id")); ok {
			app, found = *a, true
		}
	})
	if !found {
		writeError(w, http.StatusNotFound, state.ErrNotFound)
		return
	}
	secrets := map[string]string{}
	for k, v := range app.Secrets {
		secrets[k] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": app.Name, "domain": app.Domain, "setup": s.Apps.Notes(app),
		"secrets": secrets, "notes": app.Notes, "data": s.Apps.Dir(app.ID) + "/data"})
}

func (s *Server) setAppNotes(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Notes string `json:"notes"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(in.Notes) > 20000 {
		writeError(w, http.StatusBadRequest, errors.New("notes can be at most 20,000 characters"))
		return
	}
	err := s.Store.Update(func(d *state.Data) error {
		a, ok := d.App(r.PathValue("id"))
		if !ok {
			return state.ErrNotFound
		}
		a.Notes = strings.TrimRight(in.Notes, " \n")
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.jobs.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("no such job (they're kept until the service restarts)"))
		return
	}
	writeJSON(w, http.StatusOK, j.view())
}

// MARK: Jobs

// A job is a slow change (install, update, removal) running in the background, with its log.
type job struct {
	id, what string
	mu       sync.Mutex
	log      bytes.Buffer
	done     bool
	err      error
	started  time.Time
	result   any // what the job made, for jobs that make something (a draft)
}

func (j *job) setResult(v any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.result = v
}

func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.log.Len() < 2<<20 { // keep the first 2 MB
		j.log.Write(p)
	}
	return len(p), nil
}

type jobView struct {
	ID      string    `json:"id"`
	What    string    `json:"what"`
	Status  string    `json:"status"` // running, done, failed
	Error   string    `json:"error,omitempty"`
	Log     string    `json:"log"`
	Started time.Time `json:"started"`
	Result  any       `json:"result,omitempty"`
}

func (j *job) view() jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := jobView{ID: j.id, What: j.what, Status: "running", Log: j.log.String(), Started: j.started, Result: j.result}
	if j.done {
		v.Status = "done"
		if j.err != nil {
			v.Status, v.Error = "failed", j.err.Error()
		}
	}
	return v
}

type jobs struct {
	mu   sync.Mutex
	all  map[string]*job
	keep []string
}

func (js *jobs) start(what string, run func(ctx context.Context, j *job) error) *job {
	j := &job{id: auth.NewToken()[:12], what: what, started: time.Now()}
	js.mu.Lock()
	if js.all == nil {
		js.all = map[string]*job{}
	}
	js.all[j.id] = j
	js.keep = append(js.keep, j.id)
	if len(js.keep) > 50 { // remember the last 50
		delete(js.all, js.keep[0])
		js.keep = js.keep[1:]
	}
	js.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		err := run(ctx, j)
		j.mu.Lock()
		j.done, j.err = true, err
		j.mu.Unlock()
	}()
	return j
}

func (js *jobs) get(id string) (*job, bool) {
	js.mu.Lock()
	defer js.mu.Unlock()
	j, ok := js.all[id]
	return j, ok
}
