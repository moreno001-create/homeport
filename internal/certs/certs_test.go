package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func selfSigned(t *testing.T, names []string, from, until time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]},
		DNSNames: names, NotBefore: from, NotAfter: until}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd})
}

func TestCheck(t *testing.T) {
	now := time.Now()
	c, k := selfSigned(t, []string{"*.example.com", "example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	if info, err := Check(c, k, "cloud.example.com", now); err != nil || !info.NotAfter.After(now) {
		t.Fatalf("a wildcard covering the domain should pass: %v", err)
	}
	if _, err := Check(c, k, "other.test", now); err == nil || !strings.Contains(err.Error(), "not other.test") {
		t.Errorf("wrong domain: %v", err)
	}
	if _, err := Check(c, k, "example.com", now.Add(100*24*time.Hour)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired: %v", err)
	}
	_, k2 := selfSigned(t, []string{"example.com"}, now.Add(-time.Hour), now.Add(time.Hour))
	if _, err := Check(c, k2, "example.com", now); err == nil {
		t.Error("a key from another certificate should fail")
	}
}

func TestSaveAndRemove(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "a.example.com", []byte("C"), []byte("K")); err != nil {
		t.Fatal(err)
	}
	cert, key := Paths(dir, "a.example.com")
	if b, _ := os.ReadFile(cert); string(b) != "C" {
		t.Error("certificate not saved")
	}
	if fi, _ := os.Stat(key); fi.Mode().Perm() != 0o640 {
		t.Errorf("key mode %v, want 0640", fi.Mode().Perm())
	}
	if err := Save(dir, "../evil", nil, nil); err == nil {
		t.Error("a domain with a slash must be refused")
	}
	if err := Remove(dir, "a.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cert); !os.IsNotExist(err) {
		t.Error("certificate still there")
	}
}

func TestLive(t *testing.T) {
	now := time.Now()
	c, k := selfSigned(t, []string{"site.test"}, now.Add(-time.Hour), now.Add(24*time.Hour))
	pair, err := tls.X509KeyPair(c, k)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { conn.(*tls.Conn).Handshake(); conn.Close() }()
		}
	}()
	st := Live(context.Background(), ln.Addr().String(), "site.test", nil)
	// A self-signed certificate names itself as its issuer.
	if st.Error != "" || st.Trusted || st.Issuer != "site.test" || st.NotAfter.IsZero() {
		t.Errorf("self-signed: %+v", st)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(c)
	if st := Live(context.Background(), ln.Addr().String(), "site.test", pool); !st.Trusted {
		t.Errorf("should be trusted with its own root: %+v", st)
	}
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := free.Addr().String()
	free.Close()
	if st := Live(context.Background(), addr, "site.test", nil); st.Error == "" {
		t.Error("nothing listening should be an error")
	}
}
