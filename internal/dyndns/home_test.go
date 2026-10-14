package dyndns

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"homeport/internal/state"
)

// desecRecords is deSEC's API with its records kept: "subname TYPE" → records (in zone "", the first domain, or
// "zone subname TYPE" for others).
type desecRecords struct {
	mu      sync.Mutex
	records map[string][]string
	updates int             // calls to the dynDNS update service (home names don't use it)
	account map[string]bool // domains in the account
	others  map[string]bool // domains in other accounts
	first   string          // the zone whose records are kept without its name
}

func (f *desecRecords) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/ip4" {
		io.WriteString(w, "203.0.113.7")
		return
	}
	if r.URL.Path == "/update" {
		f.updates++
	}
	if r.URL.Path == "/ip6" || r.URL.Path == "/update" {
		w.WriteHeader(404)
		return
	}
	if r.Header.Get("Authorization") != "Token good" {
		w.WriteHeader(401)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	zone := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(path, "rrsets/"), "/domains/"), "/")
	key := func(sub, typ string) string {
		if zone == f.first || f.first == "" {
			f.first = zone
			return sub + " " + typ
		}
		return zone + " " + sub + " " + typ
	}
	type rrset struct {
		Subname string   `json:"subname"`
		Type    string   `json:"type"`
		TTL     int      `json:"ttl"`
		Records []string `json:"records"`
	}
	switch {
	case path == "/domains/" && r.Method == "GET":
		var out []map[string]string
		for d := range f.account {
			out = append(out, map[string]string{"name": d})
		}
		json.NewEncoder(w).Encode(out)
	case path == "/domains/" && r.Method == "POST":
		var in struct{ Name string }
		json.NewDecoder(r.Body).Decode(&in)
		switch {
		case !strings.Contains(in.Name, ".") || in.Name == "co.uk":
			w.WriteHeader(400)
			io.WriteString(w, `{"name":["This domain name conflicts with an existing domain, or is a public suffix."]}`)
		case f.others[in.Name]:
			w.WriteHeader(409)
			io.WriteString(w, `{"detail":"This domain is already registered."}`)
		default:
			if f.account == nil {
				f.account = map[string]bool{}
			}
			f.account[in.Name] = true
			w.WriteHeader(201)
		}
	case strings.HasSuffix(path, "/rrsets/") && r.Method == "POST":
		var in rrset
		json.NewDecoder(r.Body).Decode(&in)
		f.records[key(in.Subname, in.Type)] = in.Records
		w.WriteHeader(201)
	case strings.HasSuffix(path, "/rrsets/") && r.Method == "PATCH":
		var in []rrset
		json.NewDecoder(r.Body).Decode(&in)
		for _, x := range in {
			if x.TTL < 60 {
				w.WriteHeader(400)
				return
			}
			if len(x.Records) == 0 {
				delete(f.records, key(x.Subname, x.Type))
			} else {
				f.records[key(x.Subname, x.Type)] = x.Records
			}
		}
		w.WriteHeader(200)
	case strings.HasSuffix(path, "/rrsets/") && r.Method == "GET":
		var out []rrset
		for k, v := range f.records {
			sub, typ, _ := strings.Cut(k, " ")
			if typ == r.URL.Query().Get("type") {
				out = append(out, rrset{Subname: sub, Type: typ, Records: v})
			}
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == "GET":
		io.WriteString(w, `{"name":"`+zone+`","minimum_ttl":60}`)
	default:
		w.WriteHeader(404)
	}
}

func (f *desecRecords) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k, v := range f.records {
		out = append(out, k+"="+strings.Join(v, ","))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func homeSetup(t *testing.T) (*Updater, *desecRecords) {
	t.Helper()
	f := &desecRecords{records: map[string][]string{}, account: map[string]bool{}, others: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	DesecAPI, DesecUpdate = srv.URL+"/api/v1", srv.URL+"/update"
	CheckIPv4URL, CheckIPv6URL = srv.URL+"/ip4", srv.URL+"/ip6"
	dir := t.TempDir()
	store, _ := state.Open(filepath.Join(dir, "state.json"))
	u := &Updater{Store: store, Dir: filepath.Join(dir, "dyndns"), Client: srv.Client(), LocalIPv4: func() string { return "192.168.7.3" }}
	return u, f
}

// A home name points at the home address (and nothing at the public one), with the wildcard for apps; apps put on
// the internet get their own record at the public address, removed again when they come off.
func TestHomeName(t *testing.T) {
	u, f := homeSetup(t)
	ctx := context.Background()
	if err := u.RegisterHome(ctx, "desec", "home.dedyn.io", "bad"); err == nil {
		t.Fatal("a refused token should fail")
	}
	if err := u.RegisterHome(ctx, "desec", "home.dedyn.io", "good"); err != nil {
		t.Fatal(err)
	}
	if got := f.all(); got != " A=192.168.7.3 * CNAME=home.dedyn.io." {
		t.Errorf("records: %q", got)
	}
	if f.updates != 0 {
		t.Error("home names are set through deSEC's API, not the update service")
	}
	d, _ := domain(u, "home.dedyn.io")
	if !d.DynDNS.Home || d.Cert != state.CertDNS || d.DynDNS.IPv4 != "192.168.7.3" || d.DynDNS.Error != "" {
		t.Errorf("domain: %+v %+v", d, d.DynDNS)
	}
	u.Store.Update(func(v *state.Data) error {
		v.AddApp(state.App{ID: "photos", Domain: "photos.home.dedyn.io", Internet: true})
		v.AddApp(state.App{ID: "notes", Domain: "notes.home.dedyn.io"})
		return nil
	})
	if err := u.Sync(ctx, false); err != nil {
		t.Fatal(err)
	}
	if got := f.all(); !strings.Contains(got, "photos A=203.0.113.7") || strings.Contains(got, "notes A") {
		t.Errorf("internet app's record: %q", got)
	}
	u.Store.Update(func(v *state.Data) error { a, _ := v.App("photos"); a.Internet = false; return nil })
	_ = u.Sync(ctx, false)
	if got := f.all(); strings.Contains(got, "photos") {
		t.Errorf("the record should be gone: %q", got)
	}
	// Proof for a certificate: TXT records at a name under it, quoted, then removed.
	if err := u.SetTXT(ctx, "_acme-challenge.home.dedyn.io", []string{"abc", "def"}); err != nil {
		t.Fatal(err)
	}
	if got := f.all(); !strings.Contains(got, `_acme-challenge TXT="abc","def"`) {
		t.Errorf("TXT: %q", got)
	}
	_ = u.SetTXT(ctx, "_acme-challenge.home.dedyn.io", nil)
	if got := f.all(); strings.Contains(got, "TXT") {
		t.Errorf("TXT left: %q", got)
	}
	if err := u.SetTXT(ctx, "_acme-challenge.other.dedyn.io", []string{"x"}); err == nil {
		t.Error("a name not under a free domain")
	}
}

func TestHomeNameHelpers(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	store.Update(func(d *state.Data) error {
		d.Settings().MainDomain = "home.dedyn.io"
		d.AddDomain(state.Domain{Name: "home.dedyn.io", DynDNS: &state.DynDNS{Provider: "desec", Home: true}})
		d.AddDomain(state.Domain{Name: "homeport.local"})
		return nil
	})
	store.Read(func(d *state.Data) {
		if d.HomeName() != "home.dedyn.io" || d.HomeBase() != "home.dedyn.io" || d.PublicBase() != "" {
			t.Errorf("home %q, home base %q, public base %q", d.HomeName(), d.HomeBase(), d.PublicBase())
		}
		for _, c := range []struct {
			app  state.App
			want bool
		}{
			{state.App{Domain: "photos.home.dedyn.io"}, false},
			{state.App{Domain: "photos.home.dedyn.io", Internet: true}, true},
			{state.App{Domain: "cloud.example.org"}, true},
			{state.App{Domain: "cloud.homeport.local"}, false},
		} {
			if got := d.OnInternet(c.app); got != c.want {
				t.Errorf("%s (internet %v): %v", c.app.Domain, c.app.Internet, got)
			}
		}
	})
}

// A domain of your own: the home name can be a part of it (home.example.com), whose deSEC domain is found in the
// account or added (the shortest part deSEC takes), and it must use deSEC's nameservers. Only the home name's records
// change; the rest of the domain is left alone.
func TestHomeNameOnYourOwnDomain(t *testing.T) {
	for _, c := range []struct{ typed, want string }{
		{"MyHome", "myhome.dedyn.io"}, {"myhome.dedyn.io", "myhome.dedyn.io"}, {"Home.Example.com.", "home.example.com"}, {"example.com", "example.com"},
	} {
		if got, err := HomeName(c.typed); err != nil || got != c.want {
			t.Errorf("HomeName(%q) = %q, %v", c.typed, got, err)
		}
	}
	if _, err := HomeName("not a domain.com"); err == nil {
		t.Error("spaces aren't a domain")
	}

	u, f := homeSetup(t)
	ctx := context.Background()
	delegated := map[string]bool{"example.co.uk": true}
	lookupNS = func(_ context.Context, zone string) ([]*net.NS, error) {
		if delegated[zone] {
			return []*net.NS{{Host: "ns1.desec.io."}, {Host: "ns2.desec.org."}}, nil
		}
		return []*net.NS{{Host: "ns1.registrar.example."}}, nil
	}
	t.Cleanup(func() { lookupNS = net.DefaultResolver.LookupNS })

	// Not using deSEC's nameservers yet: added to the account, but refused, with what to do.
	err := u.RegisterHome(ctx, "desec", "home.example.org", "good")
	if err == nil || !strings.Contains(err.Error(), "ns1.desec.io") || !strings.Contains(err.Error(), "ns1.registrar.example") {
		t.Fatalf("err = %v", err)
	}
	if !f.account["example.org"] {
		t.Error("example.org should have been added to the account (the shortest part deSEC takes)")
	}

	// co.uk is a public suffix: example.co.uk is the domain.
	f.records["www A"] = []string{"198.51.100.1"} // the domain's own website stays
	f.first = "example.co.uk"
	if err := u.RegisterHome(ctx, "desec", "home.example.co.uk", "good"); err != nil {
		t.Fatal(err)
	}
	d, _ := domain(u, "home.example.co.uk")
	if d.DynDNS.Zone != "example.co.uk" {
		t.Errorf("zone = %q", d.DynDNS.Zone)
	}
	if got := f.all(); got != "*.home CNAME=home.example.co.uk. home A=192.168.7.3 www A=198.51.100.1" {
		t.Errorf("records: %q", got)
	}
	u.Store.Update(func(v *state.Data) error {
		v.AddApp(state.App{ID: "photos", Domain: "photos.home.example.co.uk", Internet: true})
		return nil
	})
	_ = u.Sync(ctx, false)
	if got := f.all(); !strings.Contains(got, "photos.home A=203.0.113.7") || !strings.Contains(got, "www A=198.51.100.1") {
		t.Errorf("records: %q", got)
	}
	if err := u.SetTXT(ctx, "_acme-challenge.home.example.co.uk", []string{"abc"}); err != nil {
		t.Fatal(err)
	}
	if got := f.all(); !strings.Contains(got, `_acme-challenge.home TXT="abc"`) {
		t.Errorf("TXT: %q", got)
	}

	// Someone else's domain.
	f.others["taken.net"] = true
	if err := u.RegisterHome(ctx, "desec", "home.taken.net", "good"); err == nil || !strings.Contains(err.Error(), "someone else") {
		t.Errorf("err = %v", err)
	}
}
