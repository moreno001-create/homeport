package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"homeport/internal/state"
)

func TestCertificateChoice(t *testing.T) {
	r := newRig(t)
	r.srv.CertDir = t.TempDir()
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
	cert := func() state.CertMode {
		var m state.CertMode
		r.srv.Store.Read(func(d *state.Data) { x, _ := d.Domain("example.test"); m = x.Cert })
		return m
	}

	if w := call("PUT", "/api/domains/example.test/certificate", map[string]string{"mode": "letsencrypt"}); w.Code != http.StatusNoContent || cert() != state.CertLetsEncrypt {
		t.Fatalf("choosing Let's Encrypt: %d %s", w.Code, w.Body)
	}
	if w := call("PUT", "/api/domains/example.test/certificate", map[string]string{"mode": "bogus"}); w.Code != http.StatusBadRequest {
		t.Errorf("unknown mode: %d", w.Code)
	}
	if w := call("PUT", "/api/domains/nope.test/certificate", map[string]string{"mode": "internal"}); w.Code != http.StatusNotFound {
		t.Errorf("unknown domain: %d", w.Code)
	}

	// An uploaded certificate must be for the domain.
	c, k := testCert(t, "other.test")
	if w := call("PUT", "/api/domains/example.test/certificate", map[string]string{"mode": "custom", "cert": c, "key": k}); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not example.test") {
		t.Errorf("certificate for another domain: %d %s", w.Code, w.Body)
	}
	c, k = testCert(t, "example.test")
	if w := call("PUT", "/api/domains/example.test/certificate", map[string]string{"mode": "custom", "cert": c, "key": k}); w.Code != http.StatusNoContent || cert() != state.CertCustom {
		t.Fatalf("uploading: %d %s", w.Code, w.Body)
	}
	saved := filepath.Join(r.srv.CertDir, "example.test", "key.pem")
	if b, _ := os.ReadFile(saved); string(b) != k {
		t.Fatal("key not saved where Caddy reads it")
	}

	// Caddy refusing the change puts the old choice back.
	r.srv.ApplyProxy = func(context.Context) error { return errors.New("Caddy rejected it") }
	if w := call("PUT", "/api/domains/example.test/certificate", map[string]string{"mode": "internal"}); w.Code != http.StatusInternalServerError || cert() != state.CertCustom {
		t.Errorf("a refused change should be undone: %d, now %q", w.Code, cert())
	}
	r.srv.ApplyProxy = func(context.Context) error { return nil }

	// Back to the server's setting: the uploaded files go.
	if w := call("PUT", "/api/domains/example.test/certificate", map[string]string{"mode": ""}); w.Code != http.StatusNoContent || cert() != "" {
		t.Fatalf("back to the server's setting: %d", w.Code)
	}
	if _, err := os.Stat(saved); !os.IsNotExist(err) {
		t.Error("uploaded key left behind")
	}
	if w := call("GET", "/api/domains", nil); !strings.Contains(w.Body.String(), `"name":"example.test","main":false,"added":"0001-01-01T00:00:00Z","cert":"internal","ownCert":false`) {
		t.Errorf("listing should show the server's setting: %s", w.Body)
	}
}

func testCert(t *testing.T, name string) (certPEM, keyPEM string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(k)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}))
}
