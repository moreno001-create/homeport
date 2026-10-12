package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"homeport/internal/state"
)

func (b *browser) api(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "home.test"
	req.Header.Set("X-Homeport", "1")
	req.Header.Set("Content-Type", "application/json")
	if c, ok := b.cookies["home.test"]; ok {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: c})
	}
	w := httptest.NewRecorder()
	b.r.h.ServeHTTP(w, req)
	return w
}

func TestAppNotes(t *testing.T) {
	r := newRig(t)
	app := r.install("wikijs", "wiki.example.test", &state.Access{Groups: []string{"all"}})
	alice := r.browser()
	alice.do("POST", "https://home.test/login", url.Values{"name": {"alice"}, "password": {"correct horse"}}, nil)

	var n struct {
		Setup, Notes, Domain string
		Secrets              map[string]string
	}
	w := alice.api(t, "GET", "/api/apps/"+app.ID+"/notes", "")
	_ = json.Unmarshal(w.Body.Bytes(), &n)
	if w.Code != http.StatusOK || !strings.Contains(n.Setup, "https://wiki.example.test") || strings.Contains(n.Setup, "{{") {
		t.Fatalf("setup notes should have the app's address filled in: %d %q", w.Code, n.Setup)
	}
	if n.Secrets["db_password"] == "" || n.Notes != "" {
		t.Fatalf("secrets %v, notes %q", n.Secrets, n.Notes)
	}
	if w := alice.api(t, "PUT", "/api/apps/"+app.ID+"/notes", `{"notes":"Wiki admin: alice@example.test / hunter22\n\n"}`); w.Code != http.StatusNoContent {
		t.Fatalf("saving notes: %d %s", w.Code, w.Body)
	}
	_ = json.Unmarshal(alice.api(t, "GET", "/api/apps/"+app.ID+"/notes", "").Body.Bytes(), &n)
	if n.Notes != "Wiki admin: alice@example.test / hunter22" {
		t.Fatalf("saved notes: %q", n.Notes)
	}
	// Not for people who aren't admins, and not in the app list.
	bob := r.browser()
	bob.do("POST", "https://home.test/login", url.Values{"name": {"bob"}, "password": {"battery staple"}}, nil)
	if w := bob.api(t, "GET", "/api/apps/"+app.ID+"/notes", ""); w.Code != http.StatusForbidden {
		t.Errorf("a non-admin reading notes: %d", w.Code)
	}
	if strings.Contains(alice.api(t, "GET", "/api/apps", "").Body.String(), "hunter22") {
		t.Error("the app list must not carry the notes")
	}
}
