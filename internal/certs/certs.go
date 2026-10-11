// Package certs handles certificates an admin uploads for a domain, and looks at the certificate each domain is
// actually served with.
package certs

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Info describes a certificate.
type Info struct {
	Issuer   string    `json:"issuer"`
	Names    []string  `json:"names"`
	NotAfter time.Time `json:"notAfter"`
}

func describe(c *x509.Certificate) Info {
	issuer := c.Issuer.CommonName
	if len(c.Issuer.Organization) > 0 {
		issuer = c.Issuer.Organization[0]
		if c.Issuer.CommonName != "" && c.Issuer.CommonName != issuer {
			issuer += " (" + c.Issuer.CommonName + ")"
		}
	}
	return Info{Issuer: issuer, Names: c.DNSNames, NotAfter: c.NotAfter}
}

// Check makes sure an uploaded certificate (PEM, with its chain) and private key go together, cover domain and are
// in date.
func Check(certPEM, keyPEM []byte, domain string, now time.Time) (Info, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return Info{}, fmt.Errorf("the certificate and key don't work together: %v", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return Info{}, fmt.Errorf("couldn't read the certificate: %v", err)
	}
	if err := leaf.VerifyHostname(domain); err != nil {
		return Info{}, fmt.Errorf("the certificate is for %s, not %s", strings.Join(leaf.DNSNames, ", "), domain)
	}
	if now.After(leaf.NotAfter) {
		return Info{}, fmt.Errorf("the certificate expired on %s", leaf.NotAfter.Format("2 Jan 2006"))
	}
	if now.Before(leaf.NotBefore) {
		return Info{}, fmt.Errorf("the certificate isn't valid until %s", leaf.NotBefore.Format("2 Jan 2006"))
	}
	return describe(leaf), nil
}

// Paths are where an uploaded certificate for domain is kept in dir.
func Paths(dir, domain string) (cert, key string) {
	d := filepath.Join(dir, domain)
	return filepath.Join(d, "cert.pem"), filepath.Join(d, "key.pem")
}

// Save keeps an uploaded certificate where Caddy can read it: on Linux Caddy runs as its own user, so the files
// belong to the caddy group and nobody else can read the key.
func Save(dir, domain string, cert, key []byte) error {
	if strings.ContainsAny(domain, `/\`) || domain == "" || domain == "." || domain == ".." {
		return errors.New("bad domain")
	}
	certPath, keyPath := Paths(dir, domain)
	gid := -1
	if g, err := user.LookupGroup("caddy"); err == nil {
		gid, _ = strconv.Atoi(g.Gid)
	}
	for _, d := range []string{dir, filepath.Dir(certPath)} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return err
		}
		if err := own(d, gid, 0o750); err != nil {
			return err
		}
	}
	for path, data := range map[string][]byte{certPath: cert, keyPath: key} {
		tmp := path + ".new"
		if err := os.WriteFile(tmp, data, 0o640); err != nil {
			return err
		}
		if err := own(tmp, gid, 0o640); err != nil {
			os.Remove(tmp)
			return err
		}
		if err := os.Rename(tmp, path); err != nil {
			return err
		}
	}
	return nil
}

func own(path string, gid int, mode os.FileMode) error {
	if gid >= 0 {
		if err := os.Chown(path, -1, gid); err != nil {
			return err
		}
	}
	return os.Chmod(path, mode)
}

// Remove forgets an uploaded certificate.
func Remove(dir, domain string) error {
	if strings.ContainsAny(domain, `/\`) || domain == "" || domain == "." || domain == ".." {
		return errors.New("bad domain")
	}
	return os.RemoveAll(filepath.Join(dir, domain))
}

// Status is what a browser would see when opening a domain.
type Status struct {
	Info
	// Trusted says the certificate checks out against the system's trusted authorities (false for Caddy's own).
	Trusted bool   `json:"trusted"`
	Error   string `json:"error,omitempty"`
}

// Live connects to Caddy on addr (this machine's HTTPS port) as a browser opening domain would, and reports the
// certificate it gets. roots are the trusted authorities (nil: the system's).
func Live(ctx context.Context, addr, domain string, roots *x509.CertPool) Status {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	d := tls.Dialer{Config: &tls.Config{ServerName: domain, InsecureSkipVerify: true}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return Status{Error: "Caddy didn't answer"}
		}
		return Status{Error: "no certificate yet (" + err.Error() + ")"}
	}
	defer conn.Close()
	chain := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(chain) == 0 {
		return Status{Error: "no certificate"}
	}
	st := Status{Info: describe(chain[0])}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	_, err = chain[0].Verify(x509.VerifyOptions{DNSName: domain, Roots: roots, Intermediates: inter})
	st.Trusted = err == nil
	return st
}
