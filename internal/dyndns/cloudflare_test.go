package dyndns

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"homeport/internal/state"
)

// fakeCloudflare is Cloudflare's API with zones and records kept.
type fakeCloudflare struct {
	mu      sync.Mutex
	zones   []cfZone
	records map[string]cfRecord // by id
	next    int
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, `{"success":true,"errors":[],"result":%s}`, b)
	}
	switch {
	case r.URL.Path == "/ip4":
		io.WriteString(w, "203.0.113.7")
		return
	case r.URL.Path == "/ip6":
		w.WriteHeader(404)
		return
	case r.Header.Get("Authorization") != "Bearer good":
		w.WriteHeader(403)
		io.WriteString(w, `{"success":false,"errors":[{"code":9109,"message":"Invalid access token"}]}`)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/cf")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/zones" && r.Method == "GET":
		ok(f.zones)
	case len(parts) == 2 && parts[0] == "zones" && r.Method == "GET":
		for _, z := range f.zones {
			if z.ID == parts[1] {
				ok(z)
				return
			}
		}
		w.WriteHeader(404)
	case len(parts) == 3 && parts[2] == "dns_records" && r.Method == "GET":
		out := []cfRecord{}
		for _, rec := range f.records {
			if rec.Type == r.URL.Query().Get("type") && rec.Name == r.URL.Query().Get("name") {
				out = append(out, rec)
			}
		}
		ok(out)
	case len(parts) == 3 && parts[2] == "dns_records" && r.Method == "POST":
		var rec cfRecord
		json.NewDecoder(r.Body).Decode(&rec)
		if rec.Proxied || rec.TTL < 60 {
			w.WriteHeader(400)
			io.WriteString(w, `{"success":false,"errors":[{"message":"bad record"}]}`)
			return
		}
		f.next++
		rec.ID = fmt.Sprint(f.next)
		f.records[rec.ID] = rec
		ok(rec)
	case len(parts) == 4 && r.Method == "DELETE":
		delete(f.records, parts[3])
		ok(map[string]string{"id": parts[3]})
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeCloudflare) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.records {
		out = append(out, r.Name+" "+r.Type+"="+r.Content)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// A home name in a domain on Cloudflare: the domain found among the token's, its records set (the rest left alone),
// apps on the internet, proofs for the certificate (several at once), and the domain's own nameservers to watch.
func TestHomeNameOnCloudflare(t *testing.T) {
	f := &fakeCloudflare{records: map[string]cfRecord{"w": {ID: "w", Type: "A", Name: "www.example.com", Content: "198.51.100.1", TTL: 60}},
		zones: []cfZone{
			{ID: "z1", Name: "example.com", Status: "active", NameServers: []string{"ada.ns.cloudflare.com", "bob.ns.cloudflare.com"}},
			{ID: "z2", Name: "pending.net", Status: "pending", NameServers: []string{"cid.ns.cloudflare.com", "dee.ns.cloudflare.com"}},
		}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	oldCF := CloudflareAPI
	CloudflareAPI, CheckIPv4URL, CheckIPv6URL = srv.URL+"/cf", srv.URL+"/ip4", srv.URL+"/ip6"
	t.Cleanup(func() { CloudflareAPI = oldCF })
	dir := t.TempDir()
	store, _ := state.Open(filepath.Join(dir, "state.json"))
	u := &Updater{Store: store, Dir: filepath.Join(dir, "dyndns"), Client: srv.Client(), LocalIPv4: func() string { return "192.168.7.3" }}
	ctx := context.Background()

	if err := u.RegisterHome(ctx, "cloudflare", "home.example.com", "bad"); err == nil || !strings.Contains(err.Error(), "Edit zone DNS") {
		t.Errorf("bad token: %v", err)
	}
	if err := u.RegisterHome(ctx, "cloudflare", "home.other.org", "good"); err == nil || !strings.Contains(err.Error(), "add the domain to Cloudflare") {
		t.Errorf("a domain the token can't see: %v", err)
	}
	if err := u.RegisterHome(ctx, "cloudflare", "home.pending.net", "good"); err == nil || !strings.Contains(err.Error(), "cid.ns.cloudflare.com") {
		t.Errorf("a domain not using Cloudflare's nameservers yet: %v", err)
	}
	if err := u.RegisterHome(ctx, "cloudflare", "home.example.com", "good"); err != nil {
		t.Fatal(err)
	}
	if got := f.all(); got != "*.home.example.com A=192.168.7.3 home.example.com A=192.168.7.3 www.example.com A=198.51.100.1" {
		t.Errorf("records: %q", got)
	}
	d, _ := domain(u, "home.example.com")
	if d.DynDNS.ZoneID != "z1" || d.DynDNS.Zone != "example.com" || !d.DynDNS.Home || d.Cert != state.CertDNS {
		t.Errorf("domain: %+v", d.DynDNS)
	}
	// Synced again: nothing doubles.
	_ = u.Sync(ctx, true)
	if n := strings.Count(f.all(), "home.example.com A="); n != 2 {
		t.Errorf("%d A records after a second sync: %s", n, f.all())
	}

	u.Store.Update(func(v *state.Data) error {
		v.AddApp(state.App{ID: "photos", Domain: "photos.home.example.com", Internet: true})
		return nil
	})
	_ = u.Sync(ctx, false)
	if got := f.all(); !strings.Contains(got, "photos.home.example.com A=203.0.113.7") {
		t.Errorf("internet app: %q", got)
	}
	u.Store.Update(func(v *state.Data) error { a, _ := v.App("photos"); a.Internet = false; return nil })
	_ = u.Sync(ctx, false)
	if got := f.all(); strings.Contains(got, "photos") || !strings.Contains(got, "www.example.com") {
		t.Errorf("after taking it off: %q", got)
	}

	if err := u.SetTXT(ctx, "_acme-challenge.home.example.com", []string{"abc", "def"}); err != nil {
		t.Fatal(err)
	}
	if got := f.all(); !strings.Contains(got, "_acme-challenge.home.example.com TXT=abc _acme-challenge.home.example.com TXT=def") {
		t.Errorf("TXT: %q", got)
	}
	_ = u.SetTXT(ctx, "_acme-challenge.home.example.com", nil)
	if strings.Contains(f.all(), "TXT") {
		t.Errorf("TXT left: %q", f.all())
	}
	if ns := u.Nameservers(ctx, "home.example.com"); strings.Join(ns, " ") != "ada.ns.cloudflare.com:53 bob.ns.cloudflare.com:53" {
		t.Errorf("nameservers: %v", ns)
	}

	// Everything on the internet: the name and *.name point at the public address; no records per app.
	u.Store.Update(func(v *state.Data) error {
		v.Settings().HomeOnInternet = true
		a, _ := v.App("photos")
		a.Internet = true
		return nil
	})
	_ = u.Sync(ctx, false)
	if got := f.all(); !strings.Contains(got, "*.home.example.com A=203.0.113.7 home.example.com A=203.0.113.7") || strings.Contains(got, "photos") {
		t.Errorf("everything on: %q", got)
	}
	u.Store.Update(func(v *state.Data) error { v.Settings().HomeOnInternet = false; return nil })
	_ = u.Sync(ctx, false)
	if got := f.all(); !strings.Contains(got, "home.example.com A=192.168.7.3") || !strings.Contains(got, "photos.home.example.com A=203.0.113.7") {
		t.Errorf("back home (photos stays on the internet): %q", got)
	}
}
