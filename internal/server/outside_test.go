package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestOutsideAppAPI(t *testing.T) {
	r := newRig(t)
	admin := r.browser()
	admin.do("POST", "https://home.test/login", url.Values{"name": {"alice"}, "password": {"correct horse"}}, nil)
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
		req.Host = "home.test"
		req.Header.Set("X-Homeport", "1")
		req.AddCookie(&http.Cookie{Name: cookieName, Value: admin.cookies["home.test"]})
		w := httptest.NewRecorder()
		r.h.ServeHTTP(w, req)
		return w
	}
	evil := "id: evil\nname: Evil\ncompose: |\n  services:\n    app:\n      image: x/y:1\n      privileged: true\n      ports: [\"127.0.0.1:{{.Port}}:80\"]\n"
	w := call("POST", "/api/apps/check", map[string]string{"manifest": evil})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "privileged") {
		t.Fatalf("check: %d %s", w.Code, w.Body)
	}
	src := map[string]string{"url": "https://github.com/o/evil", "commit": strings.Repeat("b", 40)}
	if w := call("POST", "/api/apps", map[string]any{"manifest": evil, "source": src, "domain": "evil.example.test"}); w.Code != http.StatusBadRequest {
		t.Errorf("installing a refused manifest must fail up front: %d %s", w.Code, w.Body)
	}
	if w := call("POST", "/api/apps/draft", map[string]string{"url": "https://gitlab.com/o/a"}); w.Code != http.StatusBadRequest {
		t.Errorf("only GitHub addresses: %d", w.Code)
	}
}
