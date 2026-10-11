package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"homeport/internal/apps"
	"homeport/internal/auth"
	"homeport/internal/state"
)

// fakeRunner stands in for docker, caddy and systemctl.
type fakeRunner struct{ calls []string }

func (f *fakeRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return `{"State":"running"}`, nil
}

func (f *fakeRunner) Stream(ctx context.Context, dir string, w io.Writer, name string, args ...string) error {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return nil
}

type rig struct {
	t      *testing.T
	srv    *Server
	h      http.Handler
	runner *fakeRunner
}

func newRig(t *testing.T) *rig {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	hash := func(p string) string { h, _ := auth.HashPassword(p); return h }
	_ = store.Update(func(d *state.Data) error {
		*d.Settings() = state.Settings{MainDomain: "home.test", TLS: state.TLSInternal}
		d.AddDomain(state.Domain{Name: "home.test"})
		d.AddDomain(state.Domain{Name: "example.test"})
		d.AddUser(state.User{Name: "alice", DisplayName: "Alice", Password: hash("correct horse"), Admin: true})
		d.AddUser(state.User{Name: "bob", DisplayName: "Bob", Password: hash("battery staple"), Groups: []string{"family"}})
		return nil
	})
	fr := &fakeRunner{}
	m := &apps.Manager{Store: store, Root: filepath.Join(dir, "apps"), Runner: fr, Apply: func(context.Context) error { return nil }}
	s := &Server{Store: store, Apps: m, Version: "test", ApplyProxy: func(context.Context) error { return nil }}
	return &rig{t: t, srv: s, h: s.Handler(), runner: fr}
}

// browser keeps one cookie jar per domain, like a real browser.
type browser struct {
	r       *rig
	cookies map[string]string
}

func (r *rig) browser() *browser { return &browser{r: r, cookies: map[string]string{}} }

func (b *browser) do(method, rawURL string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	u, _ := url.Parse(rawURL)
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, u.RequestURI(), body)
	req.Host = u.Host
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c, ok := b.cookies[u.Host]; ok {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: c})
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	b.r.h.ServeHTTP(w, req)
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			if c.MaxAge < 0 {
				delete(b.cookies, u.Host)
			} else {
				b.cookies[u.Host] = c.Value
			}
		}
	}
	return w
}

// verify is what Caddy's forward_auth sends for a request to an app.
func (b *browser) verify(app, domain, path string) *httptest.ResponseRecorder {
	return b.do("GET", "https://"+domain+"/auth/verify?app="+app, nil, map[string]string{"X-Forwarded-Method": "GET", "X-Forwarded-Uri": path})
}

func (r *rig) install(catalogID, domain string, access *state.Access) state.App {
	app, err := r.srv.Apps.Install(context.Background(), apps.InstallRequest{Catalog: catalogID, Domain: domain, Access: access}, state.User{Name: "alice", Admin: true}, io.Discard)
	if err != nil {
		r.t.Fatal(err)
	}
	return app
}

func TestSignInCarriesOverToAnAppOnAnotherDomain(t *testing.T) {
	r := newRig(t)
	app := r.install("whoami", "who.example.test", &state.Access{Groups: []string{"family"}})
	bob := r.browser()

	// Not signed in: the app's check sends the browser to the portal.
	w := bob.verify(app.ID, app.Domain, "/page?x=1")
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "https://home.test/sso/continue?return=") {
		t.Fatalf("want a redirect to sign in, got %d %s", w.Code, w.Header().Get("Location"))
	}
	cont := w.Header().Get("Location")
	// The portal wants a sign-in first, and comes back to continue.
	w = bob.do("GET", cont, nil, nil)
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "/login?return=") {
		t.Fatalf("want the login page, got %d %s", w.Code, w.Header().Get("Location"))
	}
	ret, _ := url.QueryUnescape(strings.TrimPrefix(w.Header().Get("Location"), "/login?return="))
	w = bob.do("POST", "https://home.test/login", url.Values{"name": {"bob"}, "password": {"battery staple"}, "return": {ret}}, nil)
	if w.Code != http.StatusFound || w.Header().Get("Location") != ret {
		t.Fatalf("signing in should go back to %s, got %d %s", ret, w.Code, w.Header().Get("Location"))
	}
	// Continue hands the sign-in to the app's domain with a one-time code.
	w = bob.do("GET", "https://home.test"+ret, nil, nil)
	cb := w.Header().Get("Location")
	if !strings.HasPrefix(cb, "https://who.example.test/.homeport/callback?code=") {
		t.Fatalf("want the hand-off to the app's domain, got %s", cb)
	}
	w = bob.do("GET", cb, nil, nil)
	if w.Header().Get("Location") != "https://who.example.test/page?x=1" {
		t.Fatalf("after the hand-off, want the page they asked for, got %s", w.Header().Get("Location"))
	}
	// The code works once only.
	if w2 := r.browser().do("GET", cb, nil, nil); w2.Code != http.StatusBadRequest {
		t.Fatalf("a used code must not work again, got %d", w2.Code)
	}
	// Now the check lets Bob in, and tells the app who he is.
	w = bob.verify(app.ID, app.Domain, "/page")
	if w.Code != http.StatusOK || w.Header().Get("Remote-User") != "bob" || w.Header().Get("Remote-Groups") != "family" {
		t.Fatalf("want 200 as bob, got %d %v", w.Code, w.Header())
	}
	// Signing out of the portal ends the app's session too.
	bob.do("POST", "https://home.test/logout", nil, nil)
	if w := bob.verify(app.ID, app.Domain, "/page"); w.Code == http.StatusOK {
		t.Fatal("signing out of the portal must sign out of the apps too")
	}
}

func TestAppsOnlyOpenForTheirGroups(t *testing.T) {
	r := newRig(t)
	app := r.install("whoami", "who.example.test", &state.Access{Groups: []string{"work"}})
	alice := r.browser()
	alice.do("POST", "https://home.test/login", url.Values{"name": {"alice"}, "password": {"correct horse"}}, nil)
	signIn(t, alice, app)
	// Alice is an admin but not in "work".
	if w := alice.verify(app.ID, app.Domain, "/"); w.Code != http.StatusForbidden {
		t.Fatalf("want 403 for someone outside the app's groups, got %d", w.Code)
	}
	// Another app's session doesn't count here, and a session for one domain doesn't work on another.
	other := r.install("whoami", "who2.example.test", &state.Access{Groups: []string{"all"}})
	alice.cookies[other.Domain] = alice.cookies[app.Domain]
	if w := alice.verify(other.ID, other.Domain, "/"); w.Code == http.StatusOK {
		t.Fatal("a session cookie from one domain must not work on another")
	}
}

func signIn(t *testing.T, b *browser, app state.App) {
	t.Helper()
	w := b.do("GET", "https://home.test/sso/continue?return="+url.QueryEscape("https://"+app.Domain+"/"), nil, nil)
	b.do("GET", w.Header().Get("Location"), nil, nil)
}

func TestWrongPasswordsAndRedirectsAreRefused(t *testing.T) {
	r := newRig(t)
	b := r.browser()
	if w := b.do("POST", "https://home.test/login", url.Values{"name": {"alice"}, "password": {"nope nope nope"}}, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("a wrong password: want 401, got %d", w.Code)
	}
	b.do("POST", "https://home.test/login", url.Values{"name": {"alice"}, "password": {"correct horse"}}, nil)
	// Sign-in mustn't be usable to send people to another site.
	for _, ret := range []string{"https://evil.test/", "http://who.example.test/", "//evil.test", "javascript:alert(1)"} {
		if w := b.do("GET", "https://home.test/sso/continue?return="+url.QueryEscape(ret), nil, nil); w.Code != http.StatusBadRequest {
			t.Errorf("return=%s: want 400, got %d %s", ret, w.Code, w.Header().Get("Location"))
		}
	}
	w := b.do("POST", "https://home.test/login", url.Values{"name": {"alice"}, "password": {"correct horse"}, "return": {"//evil.test/"}}, nil)
	if w.Header().Get("Location") != "/" {
		t.Errorf("a login return of //evil.test must go to /, got %s", w.Header().Get("Location"))
	}
	// Too many wrong passwords from one address are slowed down.
	m := r.browser()
	var last int
	for i := 0; i < maxBadLogins+1; i++ {
		last = m.do("POST", "https://home.test/login", url.Values{"name": {"bob"}, "password": {"wrong wrong"}}, map[string]string{"X-Forwarded-For": "203.0.113.9"}).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("want 429 after many wrong passwords, got %d", last)
	}
}

func TestAdminAPI(t *testing.T) {
	r := newRig(t)
	admin, user := r.browser(), r.browser()
	admin.do("POST", "https://home.test/login", url.Values{"name": {"alice"}, "password": {"correct horse"}}, nil)
	user.do("POST", "https://home.test/login", url.Values{"name": {"bob"}, "password": {"battery staple"}}, nil)
	if w := user.do("GET", "https://home.test/api/users", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("a non-admin: want 403, got %d", w.Code)
	}
	if w := admin.do("GET", "https://home.test/api/users", nil, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"bob"`) {
		t.Fatalf("an admin lists users: got %d %s", w.Code, w.Body)
	}
	// Changes need the X-Homeport header (another site's page can't send it).
	if w := admin.do("DELETE", "https://home.test/api/users/bob", nil, nil); w.Code != http.StatusForbidden {
		t.Fatalf("a change without X-Homeport: want 403, got %d", w.Code)
	}
	if w := admin.do("DELETE", "https://home.test/api/users/alice", nil, map[string]string{"X-Homeport": "1"}); w.Code != http.StatusBadRequest {
		t.Fatalf("deleting the only admin must be refused, got %d", w.Code)
	}
	if w := admin.do("DELETE", "https://home.test/api/users/bob", nil, map[string]string{"X-Homeport": "1"}); w.Code != http.StatusNoContent {
		t.Fatalf("deleting bob: got %d %s", w.Code, w.Body)
	}
	// The API isn't served on app domains.
	if w := admin.do("GET", "https://who.example.test/api/users", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("the API on another domain: want 404, got %d", w.Code)
	}
}

func TestInstallAndRemove(t *testing.T) {
	r := newRig(t)
	if _, err := r.srv.Apps.Install(context.Background(), apps.InstallRequest{Catalog: "whoami", Domain: "home.test"}, state.User{Name: "alice"}, io.Discard); err == nil {
		t.Fatal("installing on the main domain must be refused")
	}
	if _, err := r.srv.Apps.Install(context.Background(), apps.InstallRequest{Catalog: "whoami", Domain: "who.other.test"}, state.User{Name: "alice"}, io.Discard); err == nil {
		t.Fatal("installing on a domain that isn't listed must be refused")
	}
	a := r.install("vaultwarden", "vault.example.test", nil)
	b := r.install("vaultwarden", "vault2.example.test", nil)
	if a.ID != "vaultwarden" || b.ID != "vaultwarden-2" || a.Port == b.Port {
		t.Fatalf("two installs need their own ids and ports: %+v %+v", a, b)
	}
	if !a.Access.Public || len(a.Secrets["admin_token"]) < 20 {
		t.Fatalf("an app with its own accounts is public by default, with a generated admin token: %+v", a)
	}
	if _, err := r.srv.Apps.Install(context.Background(), apps.InstallRequest{Catalog: "whoami", Domain: "vault.example.test"}, state.User{Name: "alice"}, io.Discard); err == nil {
		t.Fatal("two apps on one domain must be refused")
	}
	if err := r.srv.Apps.Remove(context.Background(), a.ID, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	var left int
	r.srv.Store.Read(func(d *state.Data) { left = len(d.Apps()) })
	if left != 1 {
		t.Fatalf("want 1 app left, got %d", left)
	}
	joined := strings.Join(r.runner.calls, "\n")
	for _, want := range []string{"compose -p hp-vaultwarden -f compose.yaml pull", "up --detach", "down --remove-orphans --volumes"} {
		if !strings.Contains(joined, want) {
			t.Errorf("docker wasn't asked to %q:\n%s", want, joined)
		}
	}
}
