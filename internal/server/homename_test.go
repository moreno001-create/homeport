package server

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"homeport/internal/state"
)

// Under a home name, the internet switch keeps the app's address (only its DNS changes), and people outside sign in
// at signin.<home name>, on the internet port.
func TestHomeNameInternetAndSignIn(t *testing.T) {
	r := newRig(t)
	_ = r.srv.Store.Update(func(d *state.Data) error {
		d.Settings().MainDomain = "home.dedyn.io"
		d.Settings().TLS = state.TLSLetsEncrypt
		d.AddDomain(state.Domain{Name: "home.dedyn.io", Cert: state.CertDNS, DynDNS: &state.DynDNS{Provider: "desec", Home: true}})
		return nil
	})
	app := r.install("mealie", "mealie.home.dedyn.io", &state.Access{Groups: []string{"all"}})

	w := r.local(t, "GET", "/api/apps", "")
	if !strings.Contains(w.Body.String(), `"internet":false`) {
		t.Fatalf("an app under the home name is home-only at first: %s", w.Body.String())
	}
	j := r.waitJob(t, r.local(t, "POST", "/api/apps/"+app.ID+"/internet", `{"on":true}`))
	if j.Status != "done" {
		t.Fatalf("job: %+v", j)
	}
	var got state.App
	r.srv.Store.Read(func(d *state.Data) { a, _ := d.App(app.ID); got = *a })
	if !got.Internet || got.Domain != "mealie.home.dedyn.io" {
		t.Errorf("app after the switch: %+v", got)
	}

	// Signing in from outside, with the internet on port 8443.
	_ = r.srv.Store.Update(func(d *state.Data) error { d.Settings().InternetPort = 8443; return nil })
	b := r.browser()
	res := b.do("GET", "https://mealie.home.dedyn.io:8443/auth/verify?app="+app.ID, nil,
		map[string]string{"X-Forwarded-Method": "GET", "X-Forwarded-Uri": "/recipes", "X-Forwarded-Host": "mealie.home.dedyn.io:8443"})
	loc := res.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://signin.home.dedyn.io:8443/sso/continue?return=") {
		t.Fatalf("sign-in went to %q", loc)
	}
	u, _ := url.Parse(loc)
	if back := u.Query().Get("return"); back != "https://mealie.home.dedyn.io:8443/recipes" {
		t.Errorf("back to %q", back)
	}
	if !r.srv.isMainHost("signin.home.dedyn.io") {
		t.Error("the sign-in name should be the portal")
	}
	if _, ok := r.srv.safeReturn("https://mealie.home.dedyn.io:8443/recipes"); !ok {
		t.Error("going back to the app on its port should be allowed")
	}

	// Links to it carry the port: the Apps page and the portal.
	if w := r.local(t, "GET", "/api/apps", ""); !strings.Contains(w.Body.String(), `"address":"mealie.home.dedyn.io:8443"`) {
		t.Errorf("apps: %s", w.Body.String())
	}
	var links string
	r.srv.Store.Read(func(d *state.Data) { a, _ := d.App(app.ID); links = d.AppAddress(*a) })
	if links != "mealie.home.dedyn.io:8443" {
		t.Errorf("address %q", links)
	}

	// Home-only apps still sign in on the main domain.
	_ = r.srv.Store.Update(func(d *state.Data) error { a, _ := d.App(app.ID); a.Internet = false; return nil })
	res = b.do("GET", "https://mealie.home.dedyn.io/auth/verify?app="+app.ID, nil, map[string]string{"X-Forwarded-Method": "GET", "X-Forwarded-Uri": "/"})
	if loc := res.Header().Get("Location"); !strings.HasPrefix(loc, "https://home.dedyn.io/sso/continue") {
		t.Errorf("home-only app's sign-in went to %q", loc)
	}

	// Everything on the internet: refused while an app signs in with Homeport (its sign-in address would change)…
	if w := r.local(t, "POST", "/api/internet/everything", `{"on":true}`); !strings.Contains(w.Body.String(), "signs in with Homeport") {
		t.Errorf("should refuse with an OIDC app: %s", w.Body.String())
	}
	_ = r.srv.Store.Update(func(d *state.Data) error { a, _ := d.App(app.ID); a.OIDC = nil; return nil })
	// …otherwise every app and the main domain are reached on the port, and sign-in goes there.
	j = r.waitJob(t, r.local(t, "POST", "/api/internet/everything", `{"on":true}`))
	if j.Status != "done" {
		t.Fatalf("everything: %+v", j)
	}
	w = r.local(t, "GET", "/api/apps", "")
	if !strings.Contains(w.Body.String(), `"address":"mealie.home.dedyn.io:8443"`) || !strings.Contains(w.Body.String(), `"internet":true`) {
		t.Errorf("apps with everything on: %s", w.Body.String())
	}
	res = b.do("GET", "https://mealie.home.dedyn.io:8443/auth/verify?app="+app.ID, nil,
		map[string]string{"X-Forwarded-Method": "GET", "X-Forwarded-Uri": "/", "X-Forwarded-Host": "mealie.home.dedyn.io:8443"})
	if loc := res.Header().Get("Location"); !strings.HasPrefix(loc, "https://home.dedyn.io:8443/sso/continue") {
		t.Errorf("sign-in with everything on went to %q", loc)
	}
	if w := r.local(t, "GET", "/api/overview", ""); !strings.Contains(w.Body.String(), `"address":"home.dedyn.io:8443"`) || !strings.Contains(w.Body.String(), `"aiAddress":"ai.home.dedyn.io:8443"`) {
		t.Errorf("overview: %s", w.Body.String())
	}
	if iss := r.srv.issuer(); iss != "https://home.dedyn.io:8443" {
		t.Errorf("issuer %q", iss)
	}
	// The AI page sends people to sign in, and back to it on its port.
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "ai.home.dedyn.io:8443"
	rec := httptest.NewRecorder()
	r.srv.aiPage(rec, req)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, url.QueryEscape("https://ai.home.dedyn.io:8443/")) || !strings.HasPrefix(loc, "https://home.dedyn.io:8443/") {
		t.Errorf("AI page sign-in: %q", loc)
	}

	// And back.
	r.waitJob(t, r.local(t, "POST", "/api/internet/everything", `{"on":false}`))
	if w := r.local(t, "GET", "/api/overview", ""); !strings.Contains(w.Body.String(), `"address":"home.dedyn.io"`) {
		t.Errorf("overview after: %s", w.Body.String())
	}
}
