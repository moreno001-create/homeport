package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestChangeMyPassword(t *testing.T) {
	r := newRig(t)
	login := func(pass string) *browser {
		b := r.browser()
		b.do("POST", "https://home.test/login", url.Values{"name": {"bob"}, "password": {pass}, "return": {"/"}}, nil)
		return b
	}
	here, phone := login("battery staple"), login("battery staple")
	change := func(b *browser, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "https://home.test/api/me/password", bytes.NewReader([]byte(body)))
		req.Header.Set("X-Homeport", "1")
		req.Header.Set("Content-Type", "application/json")
		for _, c := range b.cookies {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: c})
		}
		w := httptest.NewRecorder()
		r.h.ServeHTTP(w, req)
		return w
	}
	if !strings.Contains(here.do("GET", "https://home.test/", nil, nil).Body.String(), `id="password"`) {
		t.Fatal("the portal has no Password button")
	}
	if w := change(here, `{"current":"wrong one","new":"a brand new password"}`); w.Code != http.StatusForbidden {
		t.Errorf("wrong current password: %d", w.Code)
	}
	if w := change(here, `{"current":"battery staple","new":"short"}`); w.Code != http.StatusBadRequest {
		t.Errorf("too short: %d", w.Code)
	}
	if w := change(here, `{"current":"battery staple","new":"battery staple"}`); w.Code != http.StatusBadRequest {
		t.Errorf("the same password: %d", w.Code)
	}
	if w := change(here, `{"current":"battery staple","new":"a brand new password"}`); w.Code != http.StatusOK {
		t.Fatalf("changing it: %d %s", w.Code, w.Body)
	}
	// this browser stays signed in; the other one is signed out
	if w := here.do("GET", "https://home.test/api/me/portal", nil, nil); w.Code != http.StatusOK {
		t.Errorf("this browser was signed out: %d", w.Code)
	}
	if w := phone.do("GET", "https://home.test/api/me/portal", nil, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("the other browser is still signed in: %d", w.Code)
	}
	if w := login("battery staple").do("GET", "https://home.test/api/me/portal", nil, nil); w.Code != http.StatusUnauthorized {
		t.Errorf("the old password still works: %d", w.Code)
	}
	if w := login("a brand new password").do("GET", "https://home.test/api/me/portal", nil, nil); w.Code != http.StatusOK {
		t.Errorf("the new password doesn't work: %d", w.Code)
	}
	// someone guessing at an unlocked computer gets locked out like at the sign-in page
	var last int
	for i := 0; i < 12; i++ {
		last = change(here, `{"current":"guess","new":"another new password"}`).Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("guessing isn't slowed down: %d", last)
	}
}
