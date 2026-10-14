package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
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
	Name        string           `json:"name"`               // the internet name apps go under ("" when there's none)
	HomeName    string           `json:"homeName,omitempty"` // the home name (apps under it go on the internet where they are)
	Port        int              `json:"port"`               // the port the internet reaches apps on (443, or 8443 beside another server)
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
	online := map[string]bool{}
	s.Store.Read(func(d *state.Data) {
		st.Name, st.RouterPorts, list = d.PublicBase(), d.Settings().RouterPorts, d.Apps()
		st.HomeName, st.Port = d.HomeName(), cmp.Or(d.Settings().InternetPort, 443)
		for _, a := range list {
			online[a.ID] = d.OnInternet(a)
		}
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
		for _, p := range s.routerPorts() {
			st.Forwards = append(st.Forwards, rt.Where(ctx, p.Outside))
		}
	}()
	go func() {
		defer wg.Done()
		if st.HomeName != "" {
			// The home name's records are Homeport's own (deSEC); its certificate covers every app under it.
			st.Name, st.NamePoints = st.HomeName, true
			st.Cert = certs.Live(ctx, addr, st.HomeName, nil)
			return
		}
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
		st.Apps = append(st.Apps, appAddress{ID: a.ID, Name: a.Name, Domain: a.Domain, Internet: online[a.ID]})
	}
	for i := range st.Apps {
		if st.Apps[i].Internet || st.HomeName != "" { // under a home name every app has a real certificate
			wg.Add(1)
			go func(x *appAddress) { defer wg.Done(); x.Cert = certs.Live(ctx, addr, x.Domain, nil) }(&st.Apps[i])
		}
	}
	wg.Wait()
	if st.HomeName == "" {
		st.NamePoints = st.PublicIP != "" && slices.Contains(st.NameIPs, st.PublicIP)
	}
	st.Shared = router.Shared(st.Router.Outside) || (st.Router.Outside != "" && st.PublicIP != "" && st.Router.Outside != st.PublicIP)
	st.Steps, st.Ready = internetSteps(st)
	if st.Apps == nil {
		st.Apps = []appAddress{}
	}
	return st
}

// internetSteps says what's still missing between the internet and this server, first thing first.
func internetSteps(st internetStatus) ([]string, bool) {
	steps := []string{} // never null: the page reads its length
	forwarded := allMine(st.Forwards)
	ports := "ports 80 and 443"
	if st.HomeName != "" {
		ports = "port " + strconv.Itoa(st.Port)
	}
	switch {
	case st.PublicIP == "":
		steps = append(steps, "This server can't reach the internet right now (or can't tell its address on it).")
	case st.Shared:
		steps = append(steps, "This internet connection shares its address with other customers (or this router is behind another one), so nothing from the internet can reach in through it. If it's a backup connection, it works again when the main one is back.")
	}
	if st.HomeName != "" {
		// Apps go on the internet under the home name, each switched on below: nothing to set up for the name.
	} else if st.Name == "" {
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
		switch {
		case len(elsewhere) > 0 && st.HomeName != "" && st.Port == 443:
			steps = append(steps, "The router already forwards "+strings.Join(elsewhere, " and ")+" (another device, another server perhaps). Either use another port beside it (8443: addresses then end in :8443), or remove that forwarding in the router's app and let Homeport open 443 here.")
		case len(elsewhere) > 0:
			steps = append(steps, "The router already forwards "+strings.Join(elsewhere, " and ")+" (another device). If that device doesn't need them, remove that forwarding in the router's app, then let Homeport open them here.")
		case st.Router.Found:
			steps = append(steps, "Let Homeport open "+ports+" on the router ("+st.Router.Name+").")
		default:
			steps = append(steps, "Forward "+ports+" (TCP) on the router to this server ("+cmpOr(st.Router.Local, "its address")+"): the router didn't answer automatic requests (UPnP).")
		}
	}
	if st.Name != "" && !st.Cert.Trusted && len(steps) == 0 {
		steps = append(steps, "The certificate for "+st.Name+" isn't there yet: it comes within a minute of everything above working.")
	}
	return steps, len(steps) == 0
}

// allMine says whether the router sends every needed port here.
func allMine(fw []router.Forward) bool {
	if len(fw) == 0 {
		return false
	}
	for _, f := range fw {
		if !f.Mine {
			return false
		}
	}
	return true
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// portPair is a router forwarding: outside port → this server's port.
type portPair struct{ Outside, Inside int }

// routerPorts are the forwardings the internet needs to reach this server: 80 and 443 for internet names (Let's
// Encrypt checks them over port 80); for a home name only 443 (its certificate comes through DNS), or another
// outside port (8443) sent to 443 when another machine has the router's 443.
func (s *Server) routerPorts() []portPair {
	var home string
	var port int
	s.Store.Read(func(d *state.Data) { home, port = d.HomeName(), d.Settings().InternetPort })
	switch {
	case home != "" && port != 0 && port != 443:
		return []portPair{{port, 443}}
	case home != "":
		return []portPair{{443, 443}}
	}
	return []portPair{{80, 80}, {443, 443}}
}

func portsText(pairs []portPair) string {
	var out []string
	for _, p := range pairs {
		out = append(out, strconv.Itoa(p.Outside))
	}
	if len(out) == 1 {
		return "port " + out[0]
	}
	return "ports " + strings.Join(out, " and ")
}

// openRouterPorts asks the router for the forwardings.
func openRouterPorts(ctx context.Context, rt *router.Router, pairs []portPair) error {
	var errs []string
	for _, p := range pairs {
		if err := rt.OpenAs(ctx, p.Outside, p.Inside); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// internetRouter opens (or closes) ports 80 and 443 on the router, and keeps them open (Homeport asks again every
// half hour, and when this machine's address changes).
func (s *Server) internetRouter(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Open bool `json:"open"`
		// Port is another outside port to use (8443), beside another machine that has the router's 443; home names
		// only (their certificates don't need 80 or 443 from outside).
		Port int `json:"port"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if in.Port != 0 && (in.Port < 1024 || in.Port > 65535) && in.Port != 443 {
		writeError(w, http.StatusBadRequest, errors.New("choose a port from 1024 to 65535 (8443, say)"))
		return
	}
	if in.Open && in.Port != 0 {
		if s.HomeName() == "" {
			writeError(w, http.StatusBadRequest, errors.New("another port only works with a home name (its certificates come through DNS); set one up under Domains"))
			return
		}
		_ = s.Store.Update(func(d *state.Data) error { d.Settings().InternetPort = in.Port; return nil })
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rt, err := router.Find(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	pairs := s.routerPorts()
	if in.Open {
		err = openRouterPorts(ctx, rt, pairs)
	} else {
		var errs []string
		for _, p := range pairs {
			if err := rt.Close(ctx, p.Outside); err != nil {
				errs = append(errs, err.Error())
			}
		}
		if len(errs) > 0 {
			err = errors.New(strings.Join(errs, "; "))
		}
	}
	_ = s.Store.Update(func(d *state.Data) error {
		d.Settings().RouterPorts = in.Open && err == nil
		if !in.Open {
			d.Settings().InternetPort = 0
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
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
				if err := openRouterPorts(c, rt, s.routerPorts()); err != nil {
					log.Printf("Router ports: %v", err)
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
