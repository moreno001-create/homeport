package dyndns

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"homeport/internal/state"
)

// fake is deSEC, DuckDNS and the address check in one test server.
type fake struct {
	mu      sync.Mutex
	ip4     string
	calls   []string
	domains map[string]bool // deSEC domains in the account
	taken   map[string]bool // deSEC domains in someone else's account
	rrsets  []string
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	desecAuth := r.Header.Get("Authorization") == "Token good"
	switch {
	case r.URL.Path == "/ip4":
		io.WriteString(w, f.ip4)
	case r.URL.Path == "/ip6":
		io.WriteString(w, "2001:db8::7")
	case r.URL.Path == "/api/v1/domains/" && r.Method == "POST":
		if !desecAuth {
			w.WriteHeader(401)
			return
		}
		var in struct{ Name string }
		json.NewDecoder(r.Body).Decode(&in)
		if f.domains[in.Name] || f.taken[in.Name] {
			w.WriteHeader(400)
			io.WriteString(w, `{"name":["This domain name conflicts with an existing domain, or is already in use."]}`)
			return
		}
		f.domains[in.Name] = true
		w.WriteHeader(201)
	case strings.HasPrefix(r.URL.Path, "/api/v1/domains/") && strings.HasSuffix(r.URL.Path, "/rrsets/"):
		var in struct {
			Subname, Type string
			Records       []string
		}
		json.NewDecoder(r.Body).Decode(&in)
		f.rrsets = append(f.rrsets, in.Subname+" "+in.Type+" "+strings.Join(in.Records, ","))
		w.WriteHeader(201)
	case strings.HasPrefix(r.URL.Path, "/api/v1/domains/") && r.Method == "GET":
		name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/domains/"), "/")
		if desecAuth && f.domains[name] {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(404)
		}
	case r.URL.Path == "/update":
		user, pass, _ := r.BasicAuth()
		if pass != "good" || user != r.URL.Query().Get("hostname") {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, "good")
	case r.URL.Path == "/duck":
		if r.URL.Query().Get("token") != "good" {
			io.WriteString(w, "KO")
			return
		}
		io.WriteString(w, "OK")
	default:
		http.NotFound(w, r)
	}
}

func setup(t *testing.T) (*Updater, *fake) {
	t.Helper()
	f := &fake{ip4: "203.0.113.7", domains: map[string]bool{}, taken: map[string]bool{"gone.dedyn.io": true}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	DesecAPI, DesecUpdate, DuckDNSURL = srv.URL+"/api/v1", srv.URL+"/update", srv.URL+"/duck"
	CheckIPv4URL, CheckIPv6URL = srv.URL+"/ip4", srv.URL+"/ip6"
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Updater{Store: store, Dir: filepath.Join(dir, "dyndns"), Client: srv.Client()}, f
}

func domain(u *Updater, name string) (state.Domain, bool) {
	var d state.Domain
	var ok bool
	u.Store.Read(func(v *state.Data) {
		var p *state.Domain
		if p, ok = v.Domain(name); ok {
			d = *p
		}
	})
	return d, ok
}

func TestFullName(t *testing.T) {
	for in, want := range map[string]string{"MyHome": "myhome.dedyn.io", "myhome.dedyn.io": "myhome.dedyn.io", " a-1 ": "a-1.dedyn.io"} {
		if got, err := FullName("desec", in); err != nil || got != want {
			t.Errorf("FullName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "a.b", "-a", "a_b", strings.Repeat("a", 64)} {
		if _, err := FullName("desec", bad); err == nil {
			t.Errorf("FullName(%q) should fail", bad)
		}
	}
	if got, _ := FullName("duckdns", "home"); got != "home.duckdns.org" {
		t.Errorf("duckdns: %q", got)
	}
	if _, err := FullName("noip", "home"); err == nil {
		t.Error("unknown provider should fail")
	}
}

func TestRegisterDesec(t *testing.T) {
	u, f := setup(t)
	ctx := context.Background()
	if err := u.Register(ctx, "desec", "home.dedyn.io", "good"); err != nil {
		t.Fatal(err)
	}
	d, ok := domain(u, "home.dedyn.io")
	if !ok || d.DynDNS == nil || d.DynDNS.IPv4 != "203.0.113.7" || d.DynDNS.IPv6 != "2001:db8::7" || d.DynDNS.Error != "" {
		t.Fatalf("domain not set up: %+v", d.DynDNS)
	}
	if len(f.rrsets) != 1 || f.rrsets[0] != "* CNAME home.dedyn.io." {
		t.Errorf("want a wildcard for app subdomains, got %v", f.rrsets)
	}
	if fi, err := os.Stat(filepath.Join(u.Dir, "home.dedyn.io.token")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("token file: %v %v", fi, err)
	}
	if err := u.Register(ctx, "desec", "home.dedyn.io", "good"); err == nil {
		t.Error("adding it twice should fail")
	}
}

func TestRegisterRefused(t *testing.T) {
	u, _ := setup(t)
	ctx := context.Background()
	if err := u.Register(ctx, "desec", "home.dedyn.io", "bad"); err == nil || !strings.Contains(err.Error(), "refused the token") {
		t.Errorf("bad token: %v", err)
	}
	if err := u.Register(ctx, "desec", "gone.dedyn.io", "good"); err == nil || !strings.Contains(err.Error(), "taken") {
		t.Errorf("someone else's name: %v", err)
	}
	if err := u.Register(ctx, "duckdns", "home.duckdns.org", "bad"); err == nil {
		t.Error("DuckDNS refusing should fail")
	}
	// Nothing half-made is kept.
	for _, n := range []string{"home.dedyn.io", "gone.dedyn.io", "home.duckdns.org"} {
		if _, ok := domain(u, n); ok {
			t.Errorf("%s kept after a failure", n)
		}
		if _, err := os.Stat(filepath.Join(u.Dir, n+".token")); err == nil {
			t.Errorf("%s token kept after a failure", n)
		}
	}
}

func TestSyncOnlyWhenTheAddressChanges(t *testing.T) {
	u, f := setup(t)
	ctx := context.Background()
	if err := u.Register(ctx, "duckdns", "home.duckdns.org", "good"); err != nil {
		t.Fatal(err)
	}
	updates := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, c := range f.calls {
			if strings.HasPrefix(c, "GET /duck?") {
				n++
			}
		}
		return n
	}
	if n := updates(); n != 1 {
		t.Fatalf("%d updates after setting up, want 1", n)
	}
	u.Sync(ctx, false)
	if n := updates(); n != 1 {
		t.Errorf("same address: %d updates, want still 1", n)
	}
	f.mu.Lock()
	f.ip4 = "198.51.100.9"
	f.mu.Unlock()
	u.Sync(ctx, false)
	d, _ := domain(u, "home.duckdns.org")
	if n := updates(); n != 2 || d.DynDNS.IPv4 != "198.51.100.9" {
		t.Errorf("new address: %d updates, recorded %s", n, d.DynDNS.IPv4)
	}
	// A token that stops working is shown on the domain, and tried again next time.
	os.WriteFile(filepath.Join(u.Dir, "home.duckdns.org.token"), []byte("bad\n"), 0o600)
	f.mu.Lock()
	f.ip4 = "198.51.100.10"
	f.mu.Unlock()
	u.Sync(ctx, false)
	if d, _ := domain(u, "home.duckdns.org"); d.DynDNS.Error == "" || d.DynDNS.IPv4 != "198.51.100.9" {
		t.Errorf("failed update should be recorded and keep the old address: %+v", d.DynDNS)
	}
	u.Forget("home.duckdns.org")
	if _, err := os.Stat(filepath.Join(u.Dir, "home.duckdns.org.token")); err == nil {
		t.Error("Forget left the token")
	}
}
