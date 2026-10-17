package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPortalLook(t *testing.T) {
	r := newRig(t)
	b := r.browser()
	b.do("POST", "https://home.test/login", url.Values{"name": {"bob"}, "password": {"battery staple"}, "return": {"/"}}, nil)
	send := func(method, path, ctype string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://home.test"+path, bytes.NewReader(body))
		req.Header.Set("X-Homeport", "1")
		req.Header.Set("Content-Type", ctype)
		for _, c := range b.cookies {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: c})
		}
		w := httptest.NewRecorder()
		r.h.ServeHTTP(w, req)
		return w
	}
	if w := send("PUT", "/api/me/portal", "application/json", []byte(`{"background":"ocean","hide":["news","weather","news"]}`)); w.Code != http.StatusNoContent {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	page := b.do("GET", "https://home.test/", nil, nil).Body.String()
	if !strings.Contains(page, `<body class="page bg-ocean">`) || strings.Contains(page, `id="news"`) || strings.Contains(page, `id="weather"`) ||
		!strings.Contains(page, `id="links"`) || !strings.Contains(page, "no-weather") {
		t.Fatalf("the page follows the look:\n%s", page)
	}
	for _, bad := range []string{`{"background":"rainbow"}`, `{"hide":["everything"]}`, `{"background":"picture"}`} {
		if w := send("PUT", "/api/me/portal", "application/json", []byte(bad)); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, w.Code)
		}
	}
	if w := send("PUT", "/api/me/background", "image/svg+xml", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)); w.Code != http.StatusBadRequest {
		t.Fatalf("not a photo: %d", w.Code)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	if w := send("PUT", "/api/me/background", "image/png", png); w.Code != http.StatusNoContent {
		t.Fatalf("picture: %d %s", w.Code, w.Body)
	}
	if w := send("PUT", "/api/me/portal", "application/json", []byte(`{"background":"picture"}`)); w.Code != http.StatusNoContent {
		t.Fatalf("picture background: %d %s", w.Code, w.Body)
	}
	if w := b.do("GET", "https://home.test/api/me/background", nil, nil); w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("their picture: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	alice := r.browser()
	alice.do("POST", "https://home.test/login", url.Values{"name": {"alice"}, "password": {"correct horse"}, "return": {"/"}}, nil)
	if w := alice.do("GET", "https://home.test/api/me/background", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("someone else's picture isn't served: %d", w.Code)
	}
	if !strings.Contains(alice.do("GET", "https://home.test/", nil, nil).Body.String(), `id="news"`) {
		t.Fatal("another person's portal is untouched")
	}
}
