package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"homeport/internal/router"
	"homeport/internal/state"
)

func (r *rig) local(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.srv.LocalHandler().ServeHTTP(w, req)
	return w
}

func (r *rig) waitJob(t *testing.T, w *httptest.ResponseRecorder) jobView {
	t.Helper()
	var j jobView
	_ = json.Unmarshal(w.Body.Bytes(), &j)
	for range 200 {
		_ = json.Unmarshal(r.local(t, "GET", "/api/jobs/"+j.ID, "").Body.Bytes(), &j)
		if j.Status != "running" {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job didn't finish")
	return j
}

func TestOnAndOffTheInternet(t *testing.T) {
	r := newRig(t)
	_ = r.srv.Store.Update(func(d *state.Data) error {
		d.Settings().MainDomain = "homeport.local"
		d.AddDomain(state.Domain{Name: "homeport.local"})
		return nil
	})
	app := r.install("whoami", "who.homeport.local", &state.Access{Public: true})
	// No internet name yet: the switch says what's missing.
	if w := r.local(t, "POST", "/api/apps/"+app.ID+"/internet", `{"on":true}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "free") {
		t.Fatalf("without an internet name: %d %s", w.Code, w.Body)
	}
	_ = r.srv.Store.Update(func(d *state.Data) error {
		d.AddDomain(state.Domain{Name: "wh91.duckdns.org", DynDNS: &state.DynDNS{Provider: "duckdns"}})
		return nil
	})
	if j := r.waitJob(t, r.local(t, "POST", "/api/apps/"+app.ID+"/internet", `{"on":true}`)); j.Status != "done" {
		t.Fatalf("on the internet: %s %s", j.Error, j.Log)
	}
	var domain string
	var oldListed bool
	r.srv.Store.Read(func(d *state.Data) {
		a, _ := d.App(app.ID)
		domain = a.Domain
		oldListed = d.HasDomain("who.homeport.local")
	})
	if domain != "who.wh91.duckdns.org" || oldListed {
		t.Fatalf("now at %s; old name still listed: %v", domain, oldListed)
	}
	if j := r.waitJob(t, r.local(t, "POST", "/api/apps/"+app.ID+"/internet", `{"on":false}`)); j.Status != "done" {
		t.Fatalf("back home: %s", j.Error)
	}
	r.srv.Store.Read(func(d *state.Data) { a, _ := d.App(app.ID); domain = a.Domain })
	if domain != "who.homeport.local" {
		t.Fatalf("back home at %s", domain)
	}
	// Taken names and the main domain are refused.
	if err := r.srv.Apps.ChangeAddress(context.Background(), app.ID, "homeport.local", nil); err == nil {
		t.Error("the main domain can't be an app's")
	}
	if err := r.srv.Apps.ChangeAddress(context.Background(), app.ID, "x.unknown.org", nil); err == nil {
		t.Error("an address under no known domain")
	}
}

func TestInternetSteps(t *testing.T) {
	steps, ready := internetSteps(internetStatus{PublicIP: "47.1.2.3", Name: "wh91.duckdns.org", NameIPs: []string{"47.1.2.3"}, NamePoints: true,
		Router: routerView{Found: true, Name: "eero", Local: "192.168.4.37"}})
	if ready || len(steps) != 1 || !strings.Contains(steps[0], "open ports 80 and 443") {
		t.Fatalf("only the router ports missing: %v", steps)
	}
	steps, _ = internetSteps(internetStatus{PublicIP: "47.1.2.3", Router: routerView{Found: true}, Forwards: []router.Forward{{Port: 80, To: "192.168.6.237"}, {Port: 443, To: "192.168.6.237"}}})
	if !strings.Contains(strings.Join(steps, " "), "already forwards 80 to 192.168.6.237 and 443 to 192.168.6.237") {
		t.Fatalf("ports another device has: %v", steps)
	}
	steps, _ = internetSteps(internetStatus{PublicIP: "172.56.1.2", Shared: true})
	if !strings.Contains(steps[0], "shares its address") || !strings.Contains(strings.Join(steps, " "), "free internet name") {
		t.Fatalf("shared connection, no name: %v", steps)
	}
}
