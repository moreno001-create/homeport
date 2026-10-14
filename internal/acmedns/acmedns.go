// Package acmedns gets certificates from Let's Encrypt for a name and every name under it (a wildcard), proving the
// name is ours through its DNS (the dns-01 challenge). Nothing has to be reachable from the internet, so it works for
// home names, which point at a home address.
package acmedns

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"golang.org/x/crypto/acme"
)

// TXT sets the TXT records at a name (none: removes them).
type TXT interface {
	SetTXT(ctx context.Context, name string, values []string) error
}

// Client gets certificates.
type Client struct {
	// Dir keeps the Let's Encrypt account's key.
	Dir   string
	Email string
	// Directory is the ACME server (default Let's Encrypt; HOMEPORT_ACME_DIRECTORY overrides it, for its staging server).
	Directory string
	// Nameservers answer for the names (host:port), asked until the records are there before Let's Encrypt checks.
	Nameservers []string
	// Wait is how long the records may take to arrive (default 5 minutes).
	Wait time.Duration
	// HTTPClient talks to the ACME server (tests, with a test server's certificate).
	HTTPClient *http.Client
}

// DesecNameservers are deSEC's (dedyn.io names).
var DesecNameservers = []string{"ns1.desec.io:53", "ns2.desec.org:53"}

// Obtain gets a certificate for domain and *.domain. It returns the certificate with its chain and its key (PEM), and
// when it runs out.
func (c *Client) Obtain(ctx context.Context, domain string, txt TXT, log io.Writer) (certPEM, keyPEM []byte, until time.Time, err error) {
	key, err := c.accountKey()
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	dir := c.Directory
	if d := os.Getenv("HOMEPORT_ACME_DIRECTORY"); d != "" {
		dir = d
	}
	if dir == "" {
		dir = acme.LetsEncryptURL
	}
	cl := &acme.Client{Key: key, DirectoryURL: dir, UserAgent: "Homeport", HTTPClient: c.HTTPClient}
	acct := &acme.Account{}
	if c.Email != "" {
		acct.Contact = []string{"mailto:" + c.Email}
	}
	if _, err := cl.Register(ctx, acct, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, nil, time.Time{}, fmt.Errorf("couldn't sign up with Let's Encrypt: %w", plain(err))
	}
	fmt.Fprintf(log, "Asking Let's Encrypt for a certificate for %s and *.%s…\n", domain, domain)
	order, err := cl.AuthorizeOrder(ctx, acme.DomainIDs(domain, "*."+domain))
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("Let's Encrypt refused the order: %w", plain(err))
	}
	var chals []*acme.Challenge
	var values []string
	for _, u := range order.AuthzURLs {
		z, err := cl.GetAuthorization(ctx, u)
		if err != nil {
			return nil, nil, time.Time{}, plain(err)
		}
		if z.Status == acme.StatusValid {
			continue
		}
		i := slices.IndexFunc(z.Challenges, func(ch *acme.Challenge) bool { return ch.Type == "dns-01" })
		if i < 0 {
			return nil, nil, time.Time{}, errors.New("Let's Encrypt offered no DNS check")
		}
		v, err := cl.DNS01ChallengeRecord(z.Challenges[i].Token)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		chals, values = append(chals, z.Challenges[i]), append(values, v)
	}
	name := "_acme-challenge." + domain
	if len(values) > 0 {
		fmt.Fprintf(log, "Putting the proof in %s's DNS…\n", domain)
		if err := txt.SetTXT(ctx, name, values); err != nil {
			return nil, nil, time.Time{}, err
		}
		defer func() { _ = txt.SetTXT(context.Background(), name, nil) }()
		fmt.Fprintln(log, "Waiting for it to reach the name's DNS servers…")
		if err := c.waitFor(ctx, name, values); err != nil {
			return nil, nil, time.Time{}, err
		}
		for _, ch := range chals {
			if _, err := cl.Accept(ctx, ch); err != nil {
				return nil, nil, time.Time{}, plain(err)
			}
		}
		fmt.Fprintln(log, "Let's Encrypt is checking…")
		for _, u := range order.AuthzURLs {
			if _, err := cl.WaitAuthorization(ctx, u); err != nil {
				return nil, nil, time.Time{}, fmt.Errorf("Let's Encrypt couldn't check the name: %w", plain(err))
			}
		}
	}
	order, err = cl.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, nil, time.Time{}, plain(err)
	}
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{domain, "*." + domain}}, certKey)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	der, _, err := cl.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("Let's Encrypt didn't issue it: %w", plain(err))
	}
	for _, b := range der {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b})...)
	}
	kb, err := x509.MarshalECPrivateKey(certKey)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
	leaf, err := x509.ParseCertificate(der[0])
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	fmt.Fprintf(log, "Got it: valid until %s.\n", leaf.NotAfter.Format("2 January 2006"))
	return certPEM, keyPEM, leaf.NotAfter, nil
}

// waitFor asks each nameserver until all the values are at name.
func (c *Client) waitFor(ctx context.Context, name string, values []string) error {
	wait := c.Wait
	if wait == 0 {
		wait = 5 * time.Minute
	}
	end := time.Now().Add(wait)
	if len(c.Nameservers) == 0 { // not known: give the change time to spread
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(45 * time.Second):
		}
	}
	for _, ns := range c.Nameservers {
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, ns)
		}}
		for {
			got, _ := r.LookupTXT(ctx, name)
			if containsAll(got, values) {
				break
			}
			if time.Now().After(end) {
				return fmt.Errorf("the proof didn't reach %s within %s", ns, wait)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	return nil
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// accountKey is the Let's Encrypt account's key, made the first time.
func (c *Client) accountKey() (crypto.Signer, error) {
	path := filepath.Join(c.Dir, "account.key")
	if b, err := os.ReadFile(path); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			return x509.ParseECPrivateKey(blk.Bytes)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	b, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b}), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// plain makes an ACME error readable: its detail, and why (rate limits say when to try again).
func plain(err error) error {
	var ae *acme.Error
	if errors.As(err, &ae) {
		msg := ae.Detail
		if msg == "" {
			msg = ae.ProblemType
		}
		for _, sub := range ae.Subproblems {
			msg += "; " + sub.Detail
		}
		return errors.New(msg)
	}
	var oe *acme.OrderError
	if errors.As(err, &oe) {
		return fmt.Errorf("the order ended as %s", oe.Status)
	}
	return err
}
