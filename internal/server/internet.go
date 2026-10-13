package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"homeport/internal/certs"
	"homeport/internal/router"
	"homeport/internal/state"
)

// MARK: Internet access
//
// The Internet access page checks each link from the internet to this server (the public address, the router and its
// port forwarding, the free name, the certificate), says plainly which one is missing, and can open the ports on the
// router itself (UPnP).

func (s *Server) internetRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/internet", guard(s.internetStatus))
	mux.HandleFunc("POST /api/internet/router", guard(s.internetRouter))
}

type internetStatus struct {
	PublicIP    string           `json:"publicIP"`
	Router      routerView       `json:"router"`
	Shared      bool             `json:"shared"` // the connection's address is shared: nothing from outside can come in
	RouterPorts bool             `json:"routerPorts"`
	Forwards    []router.Forward `json:"forwards"`
	Name        string           `json:"name"` // the internet name apps go under ("" when there's none)
	NameIPs     []string         `json:"nameIPs"`
	NamePoints  bool             `json:"namePoints"`
	Cert        certs.Status     `json:"cert"`
	Apps        []appAddress     `json:"apps"`
	Steps       []string         `json:"steps"` // what to do next, in order
	Ready       bool             `json:"ready"`
}

type routerView struct {
	Found   bool   `json:"found"`
	Name    string `json:"name,omitempty"`
	Local   string `json:"local,omitempty"`
	Outside string `json:"outside,omitempty"`
	Error   string `json:"error,omitempty"`
}

type appAddress struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Domain   string       `json:"domain"`
	Internet bool         `json:"internet"`
	Cert     certs.Status `json:"cert"`
}

func (s *Server) internetStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.readInternet(r.Context()))
}

// readInternet checks every link between the internet and this server.
func (s *Server) readInternet(ctx context.Context) internetStatus {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	var st internetStatus
	var list []state.App
	s.Store.Read(func(d *state.Data) {
		st.Name, st.RouterPorts, list = d.PublicBase(), d.Settings().RouterPorts, d.Apps()
	})
	addr := s.HTTPSAddr
	if addr == "" {
		addr = "127.0.0.1:443"
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if s.DynDNS != nil {
			st.PublicIP = s.DynDNS.PublicIPv4(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		rt, err := router.Find(ctx)
		if err != nil {
			st.Router.Error = err.Error()
			return
		}
		st.Router = routerView{Found: true, Name: rt.Name, Local: rt.Local}
		st.Router.Outside, _ = rt.OutsideAddress(ctx)
		for _, p := range []int{80, 443} {
			st.Forwards = append(st.Forwards, rt.Where(ctx, p))
		}
	}()
	go func() {
		defer wg.Done()
		if st.Name == "" {
			return
		}
		ips, _ := net.DefaultResolver.LookupIPAddr(ctx, st.Name)
		for _, ip := range ips {
			if ip.IP.To4() != nil {
				st.NameIPs = append(st.NameIPs, ip.IP.String())
			}
		}
		st.Cert = certs.Live(ctx, addr, st.Name, nil)
	}()
	for _, a := range list {
		st.Apps = append(st.Apps, appAddress{ID: a.ID, Name: a.Name, Domain: a.Domain, Internet: !state.PrivateName(a.Domain)})
	}
	for i := range st.Apps {
		if st.Apps[i].Internet {
			wg.Add(1)
			go func(x *appAddress) { defer wg.Done(); x.Cert = certs.Live(ctx, addr, x.Domain, nil) }(&st.Apps[i])
		}
	}
	wg.Wait()
	st.NamePoints = st.PublicIP != "" && slices.Contains(st.NameIPs, st.PublicIP)
	st.Shared = router.Shared(st.Router.Outside) || (st.Router.Outside != "" && st.PublicIP != "" && st.Router.Outside != st.PublicIP)
	st.Steps, st.Ready = internetSteps(st)
	if st.Apps == nil {
		st.Apps = []appAddress{}
	}
	return st
}

// internetSteps says what's still missing between the internet and this server, first thing first.
func internetSteps(st internetStatus) ([]string, bool) {
	var steps []string
	forwarded := len(st.Forwards) == 2 && st.Forwards[0].Mine && st.Forwards[1].Mine
	switch {
	case st.PublicIP == "":
		steps = append(steps, "This server can't reach the internet right now (or can't tell its address on it).")
	case st.Shared:
		steps = append(steps, "This internet connection shares its address with other customers (or this router is behind another one), so nothing from the internet can reach in through it. If it's a backup connection, it works again when the main one is back.")
	}
	if st.Name == "" {
		steps = append(steps, "Get a free internet name (DuckDNS or deSEC): it's the address apps get on the internet.")
	} else if !st.NamePoints && st.PublicIP != "" {
		steps = append(steps, st.Name+" doesn't point at this home's address ("+st.PublicIP+") yet; Homeport updates it within a few minutes.")
	}
	var elsewhere []string
	for _, f := range st.Forwards {
		if f.To != "" && !f.Mine {
			elsewhere = append(elsewhere, fmt.Sprintf("%d to %s", f.Port, f.To))
		}
	}
	if !forwarded {
		if len(elsewhere) > 0 {
			steps = append(steps, "The router already forwards "+strings.Join(elsewhere, " and ")+" (another device). If that device doesn't need them, remove that forwarding in the router's app, then let Homeport open them here.")
		} else if st.Router.Found {
			steps = append(steps, "Let Homeport open ports 80 and 443 on the router ("+st.Router.Name+").")
		} else {
			steps = append(steps, "Forward ports 80 and 443 (TCP) on the router to this server ("+cmpOr(st.Router.Local, "its address")+"): the router didn't answer automatic requests (UPnP).")
		}
	}
	if st.Name != "" && !st.Cert.Trusted && len(steps) == 0 {
		steps = append(steps, "The certificate for "+st.Name+" isn't there yet: it comes within a minute of everything above working.")
	}
	return steps, len(steps) == 0
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// internetRouter opens (or closes) ports 80 and 443 on the router, and keeps them open (Homeport asks again every
// half hour, and when this machine's address changes).
func (s *Server) internetRouter(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Open bool `json:"open"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rt, err := router.Find(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	var errs []string
	for _, p := range []int{80, 443} {
		if in.Open {
			err = rt.Open(ctx, p)
		} else {
			err = rt.Close(ctx, p)
		}
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	_ = s.Store.Update(func(d *state.Data) error { d.Settings().RouterPorts = in.Open && len(errs) == 0; return nil })
	if len(errs) > 0 {
		writeError(w, http.StatusBadGateway, errors.New(strings.Join(errs, "; ")))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// KeepRouterPorts asks the router again, every half hour, to forward 80 and 443 here while that's turned on: router
// forwardings run out (Lease), and the router may have restarted or this machine's address changed.
func (s *Server) KeepRouterPorts(ctx context.Context) {
	t := time.NewTicker(30 * time.Minute)
	defer t.Stop()
	for {
		var on bool
		s.Store.Read(func(d *state.Data) { on = d.Settings().RouterPorts })
		if on {
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			if rt, err := router.Find(c); err != nil {
				log.Printf("Router ports: %v", err)
			} else {
				for _, p := range []int{80, 443} {
					if err := rt.Open(c, p); err != nil {
						log.Printf("Router ports: %v", err)
					}
				}
			}
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
