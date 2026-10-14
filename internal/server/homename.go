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
// A home name is a free name from deSEC (home.dedyn.io), or a name in a domain of your own that uses deSEC's
// nameservers (home.example.com), that points at this server's address on the home network.
// Every device at home finds it with ordinary DNS (phones, Android, other subnets, the server itself), nothing
// outside can reach it, and Homeport gets it a real certificate from Let's Encrypt for it and every name under it,
// proving it owns the name through its DNS: no ports to open. So every app is trusted out of the box, and signing
// in works everywhere. Apps under it can still be put on the internet one by one (their name alone then points at
// the home's public address).

func (s *Server) homeNameRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /api/domains/home", guard(s.addHomeName))
}

// renewBefore is how long before a certificate runs out a new one is got.
const renewBefore = 30 * 24 * time.Hour

func (s *Server) acmeClient() *acmedns.Client {
	return &acmedns.Client{Dir: filepath.Join(s.stateDir(), "acme"), Email: s.settings().ACMEEmail, Nameservers: acmedns.DesecNameservers}
}

// addHomeName sets up a home name (a job): the name on deSEC pointed at this server's home address, its certificate,
// and (main) the main domain moved to it, when nothing stops that.
func (s *Server) addHomeName(w http.ResponseWriter, r *http.Request) {
	if s.DynDNS == nil {
		writeError(w, http.StatusBadRequest, errors.New("free names aren't set up on this server"))
		return
	}
	var in struct {
		Name  string `json:"name"`
		Token string `json:"token"`
		Main  bool   `json:"main"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name, err := dyndns.HomeName(in.Name)
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
		return s.SetUpHomeName(ctx, name, token, in.Main, j)
	})
	writeJSON(w, http.StatusAccepted, j.view())
}

// HomeName is the home name, or "".
func (s *Server) HomeName() string {
	var h string
	s.Store.Read(func(d *state.Data) { h = d.HomeName() })
	return h
}

// SetUpHomeName registers name on deSEC as the home name, gets its certificate, and (main) makes it the main domain.
func (s *Server) SetUpHomeName(ctx context.Context, name, token string, main bool, log io.Writer) error {
	fmt.Fprintf(log, "Setting up %s on deSEC, pointed at this server's home address…\n", name)
	if err := s.DynDNS.RegisterHome(ctx, name, token); err != nil {
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
	cert, key, until, err := s.acmeClient().Obtain(ctx, name, s.DynDNS, log)
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
