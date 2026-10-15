package server

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"homeport/internal/acmedns"
	"homeport/internal/certs"
	"homeport/internal/dyndns"
	"homeport/internal/state"
)

// MARK: Home name
//
// A home name is a free name from deSEC (home.dedyn.io), or a name in a domain of your own on deSEC or Cloudflare
// (home.example.com), that points at this server's address on the home network.
// Every device at home finds it with ordinary DNS (phones, Android, other subnets, the server itself), nothing
// outside can reach it, and Homeport gets it a real certificate from Let's Encrypt for it and every name under it,
// proving it owns the name through its DNS: no ports to open. So every app is trusted out of the box, and signing
// in works everywhere. Apps under it can still be put on the internet one by one (their name alone then points at
// the home's public address).

func (s *Server) homeNameRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /api/domains/home", guard(s.addHomeName))
	mux.HandleFunc("POST /api/internet/everything", guard(s.everythingOnline))
}

// renewBefore is how long before a certificate runs out a new one is got.
const renewBefore = 30 * 24 * time.Hour

func (s *Server) acmeClient(ctx context.Context, name string) *acmedns.Client {
	return &acmedns.Client{Dir: filepath.Join(s.stateDir(), "acme"), Email: s.settings().ACMEEmail, Nameservers: s.DynDNS.Nameservers(ctx, name)}
}

// addHomeName sets up a home name (a job): the name on deSEC pointed at this server's home address, its certificate,
// and (main) the main domain moved to it, when nothing stops that.
func (s *Server) addHomeName(w http.ResponseWriter, r *http.Request) {
	if s.DynDNS == nil {
		writeError(w, http.StatusBadRequest, errors.New("free names aren't set up on this server"))
		return
	}
	var in struct {
		Provider string `json:"provider"` // desec (default) or cloudflare
		Name     string `json:"name"`
		Token    string `json:"token"`
		Main     bool   `json:"main"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	provider := cmpOr(in.Provider, "desec")
	name, err := homeNameFor(provider, in.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.HomeName() != "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("this server already has a home name (%s)", s.HomeName()))
		return
	}
	token := in.Token
	j := s.jobs.start("home name "+name, func(ctx context.Context, j *job) error {
		return s.SetUpHomeName(ctx, provider, name, token, in.Main, j)
	})
	writeJSON(w, http.StatusAccepted, j.view())
}

// everythingOnline puts everything under the home name on the internet (or back home only), as a job: the name and
// every name under it point at the home's public address, so the portal, admin panel, AI page and every app are reached
// from anywhere (with the internet port in their addresses when it isn't 443).
func (s *Server) everythingOnline(w http.ResponseWriter, r *http.Request) {
	var in struct {
		On bool `json:"on"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var home, oidc string
	var port int
	s.Store.Read(func(d *state.Data) {
		home, port = d.HomeName(), d.Settings().InternetPort
		for _, a := range d.Apps() {
			if a.OIDC != nil && d.UnderHome(a.Domain) {
				oidc = a.Name
			}
		}
	})
	switch {
	case home == "":
		writeError(w, http.StatusBadRequest, errors.New("set up a home name first (Domains)"))
		return
	case oidc != "" && port != 0 && port != 443:
		// Its sign-in address would change (the port joins it), and the app was set up with the old one.
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s signs in with Homeport, and its sign-in address would change: remove it first, or free port 443 for this server", oidc))
		return
	}
	j := s.jobs.start("internet everything", func(ctx context.Context, j *job) error {
		if err := s.Store.Update(func(d *state.Data) error { d.Settings().HomeOnInternet = in.On; return nil }); err != nil {
			return err
		}
		if in.On {
			fmt.Fprintf(j, "Pointing %s and every name under it at this home's public address…\n", home)
		} else {
			fmt.Fprintf(j, "Pointing %s and every name under it back at this server's home address…\n", home)
		}
		if s.DynDNS != nil {
			if err := s.DynDNS.Sync(ctx, true); err != nil {
				return err
			}
		}
		var syncErr string
		s.Store.Read(func(d *state.Data) {
			if x, ok := d.Domain(home); ok && x.DynDNS != nil {
				syncErr = x.DynDNS.Error
			}
		})
		if syncErr != "" {
			return errors.New(syncErr)
		}
		if s.ApplyProxy != nil {
			if err := s.ApplyProxy(ctx); err != nil {
				return err
			}
		}
		addr := s.address(home)
		if in.On {
			fmt.Fprintf(j, "Done: everything is on the internet. Open https://%s/ from anywhere (devices may take a few minutes to see the change).\n", addr)
			if addr != home {
				fmt.Fprintf(j, "Every address ends in :%d now, at home too: bookmarks without it reach the device that has the router's 443.\n", port)
			}
		} else {
			fmt.Fprintf(j, "Done: everything is home-only again, at https://%s/.\n", home)
		}
		return nil
	})
	writeJSON(w, http.StatusAccepted, j.view())
}

// HomeName is the home name, or "".
func (s *Server) HomeName() string {
	var h string
	s.Store.Read(func(d *state.Data) { h = d.HomeName() })
	return h
}

// homeNameFor checks a home name for its DNS service: on deSEC, "myhome" is myhome.dedyn.io; on Cloudflare it's a
// name in your own domain there.
func homeNameFor(provider, typed string) (string, error) {
	if dyndns.HomeProviders[provider] == "" {
		return "", fmt.Errorf(`the DNS service is "desec" or "cloudflare"`)
	}
	name, err := dyndns.HomeName(typed)
	if err == nil && provider == "cloudflare" && (!strings.Contains(strings.TrimSpace(typed), ".") || strings.HasSuffix(name, ".dedyn.io")) {
		return "", errors.New("on Cloudflare, type a name in your domain there: home.example.com, or example.com")
	}
	return name, err
}

// SetUpHomeName registers name on deSEC or Cloudflare as the home name, gets its certificate, and (main) makes it the
// main domain.
func (s *Server) SetUpHomeName(ctx context.Context, provider, name, token string, main bool, log io.Writer) error {
	fmt.Fprintf(log, "Setting up %s on %s, pointed at this server's home address…\n", name, dyndns.HomeProviders[provider])
	if err := s.DynDNS.RegisterHome(ctx, provider, name, token); err != nil {
		return err
	}
	if err := s.renewHomeCert(ctx, name, log); err != nil {
		fmt.Fprintf(log, "The certificate isn't there yet (%v). Homeport tries again every few hours; meanwhile %s works with Caddy's own certificate.\n", err, name)
	}
	if main {
		if err := s.makeMain(name); err != nil {
			fmt.Fprintf(log, "The main address stays %s: %v\n", s.settings().MainDomain, err)
		} else {
			fmt.Fprintf(log, "The portal and admin panel are at https://%s/ now.\n", name)
		}
	}
	if s.ApplyProxy != nil {
		if err := s.ApplyProxy(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintf(log, "Done: %s and every name under it (apps go at name.%s) reach this server at home.\n", name, name)
	return nil
}

// makeMain moves the main domain to name, when no app signs in with Homeport at the old one (their sign-in is set up
// with it).
func (s *Server) makeMain(name string) error {
	return s.Store.Update(func(d *state.Data) error {
		st := d.Settings()
		if st.MainDomain == name {
			return nil
		}
		for _, a := range d.Apps() {
			if a.OIDC != nil {
				return fmt.Errorf("%s signs in with Homeport at https://%s", a.Name, st.MainDomain)
			}
		}
		if other, taken := d.AppOn(name); taken {
			return fmt.Errorf("%s already serves %s", other.Name, name)
		}
		st.MainDomain = name
		return nil
	})
}

// renewHomeCert gets a new certificate for the home name, and notes how it went.
func (s *Server) renewHomeCert(ctx context.Context, name string, log io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	cert, key, until, err := s.acmeClient(ctx, name).Obtain(ctx, name, s.DynDNS, log)
	if err == nil {
		err = certs.Save(s.CertDir, name, cert, key)
	}
	_ = s.Store.Update(func(d *state.Data) error {
		if x, ok := d.Domain(name); ok && x.DynDNS != nil {
			if err != nil {
				x.DynDNS.CertError = err.Error()
			} else {
				x.DynDNS.CertError, x.DynDNS.CertUntil = "", until
			}
		}
		return nil
	})
	return err
}

// KeepHomeCerts renews the home name's certificate a month before it runs out (checked twice a day; a failed attempt
// is tried again then).
func (s *Server) KeepHomeCerts(ctx context.Context) {
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 12 * time.Hour
		name := s.HomeName()
		if name == "" || s.DynDNS == nil {
			continue
		}
		if until := certUntil(s.CertDir, name); time.Until(until) > renewBefore {
			continue
		}
		var b strings.Builder
		if err := s.renewHomeCert(ctx, name, &b); err != nil {
			log.Printf("Home name %s: no new certificate yet: %v", name, err)
			continue
		}
		log.Printf("Home name %s: new certificate", name)
		if s.ApplyProxy != nil {
			if err := s.ApplyProxy(ctx); err != nil {
				log.Printf("Caddy configuration: %v", err)
			}
		}
	}
}

// certUntil is when a saved certificate runs out (zero when there's none).
func certUntil(dir, domain string) time.Time {
	path, _ := certs.Paths(dir, domain)
	b, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return time.Time{}
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return time.Time{}
	}
	return c.NotAfter
}
