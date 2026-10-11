// Package dyndns gives Homeport free domains from dynamic DNS services (deSEC's dedyn.io and DuckDNS), in the spirit
// of YunoHost's nohost.me, and keeps them pointed at this server's public address when it changes.
package dyndns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"homeport/internal/state"
)

// The services' addresses (tests point them elsewhere).
var (
	DesecAPI     = "https://desec.io/api/v1"
	DesecUpdate  = "https://update.dedyn.io/"
	DuckDNSURL   = "https://www.duckdns.org/update"
	CheckIPv4URL = "https://checkipv4.dedyn.io/"
	CheckIPv6URL = "https://checkipv6.dedyn.io/"
)

// Providers, with the domain each gives names under.
var Suffix = map[string]string{"desec": ".dedyn.io", "duckdns": ".duckdns.org"}

// FullName turns what someone typed ("myhome", or "myhome.dedyn.io") into the full domain for provider.
func FullName(provider, name string) (string, error) {
	suffix, ok := Suffix[provider]
	if !ok {
		return "", fmt.Errorf(`unknown provider %q (use "desec" or "duckdns")`, provider)
	}
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), suffix)
	if name == "" || len(name) > 63 || strings.Contains(name, ".") || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return "", fmt.Errorf("pick a name of letters, digits and dashes, like myhome%s", suffix)
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return "", fmt.Errorf("pick a name of letters, digits and dashes, like myhome%s", suffix)
		}
	}
	return name + suffix, nil
}

// Updater keeps free domains pointed at this server.
type Updater struct {
	Store *state.Store
	// Dir holds each domain's token (root-only).
	Dir    string
	Client *http.Client
	// Every is how often the public address is checked (default 5 minutes).
	Every time.Duration

	mu sync.Mutex // one update round at a time
}

func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (u *Updater) tokenPath(domain string) string { return filepath.Join(u.Dir, domain+".token") }

func (u *Updater) token(domain string) (string, error) {
	b, err := os.ReadFile(u.tokenPath(domain))
	if err != nil {
		return "", fmt.Errorf("no token kept for %s", domain)
	}
	return strings.TrimSpace(string(b)), nil
}

// Register sets up a free domain: on deSEC it makes the domain in the account (and *.domain, so apps can have
// subdomains), then points it here. The domain is only kept if that works.
func (u *Updater) Register(ctx context.Context, provider, domain, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("paste the token from your " + map[string]string{"desec": "deSEC", "duckdns": "DuckDNS"}[provider] + " account")
	}
	var taken bool
	u.Store.Read(func(d *state.Data) { taken = d.HasDomain(domain) })
	if taken {
		return fmt.Errorf("%s is already added", domain)
	}
	if provider == "desec" {
		if err := u.desecCreate(ctx, domain, token); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(u.Dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(u.tokenPath(domain), []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	err := u.Store.Update(func(d *state.Data) error {
		if d.HasDomain(domain) {
			return fmt.Errorf("%s is already added", domain)
		}
		d.AddDomain(state.Domain{Name: domain, Added: time.Now(), DynDNS: &state.DynDNS{Provider: provider}})
		return nil
	})
	if err == nil {
		err = u.Sync(ctx, true)
		if err == nil {
			u.Store.Read(func(d *state.Data) {
				if x, ok := d.Domain(domain); ok && x.DynDNS.Error != "" {
					err = errors.New(x.DynDNS.Error)
				}
			})
		}
		if err != nil {
			u.Store.Update(func(d *state.Data) error { d.DeleteDomain(domain); return nil })
		}
	}
	if err != nil {
		os.Remove(u.tokenPath(domain))
		return err
	}
	return nil
}

// Forget removes a domain's token (the domain was removed).
func (u *Updater) Forget(domain string) {
	os.Remove(u.tokenPath(domain))
}

// Run updates every few minutes until ctx ends.
func (u *Updater) Run(ctx context.Context) {
	every := u.Every
	if every == 0 {
		every = 5 * time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := u.Sync(ctx, false); err != nil {
			log.Printf("Free domains: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sync points every free domain at this server's public addresses. It only calls a service when the address
// changed, the last update failed, or a day went by (force: always).
func (u *Updater) Sync(ctx context.Context, force bool) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	var todo []state.Domain
	u.Store.Read(func(d *state.Data) {
		for _, x := range d.Domains() {
			if x.DynDNS != nil {
				todo = append(todo, x)
			}
		}
	})
	if len(todo) == 0 {
		return nil
	}
	ip4, ip6 := u.publicIPs(ctx)
	if ip4 == "" && ip6 == "" {
		return errors.New("couldn't find this server's public address (no internet?)")
	}
	for _, x := range todo {
		dd := x.DynDNS
		if !force && dd.Error == "" && dd.IPv4 == ip4 && dd.IPv6 == ip6 && time.Since(dd.Updated) < 24*time.Hour {
			continue
		}
		err := u.update(ctx, x.Name, dd.Provider, ip4, ip6)
		u.Store.Update(func(d *state.Data) error {
			cur, ok := d.Domain(x.Name)
			if !ok || cur.DynDNS == nil {
				return nil // removed meanwhile
			}
			if err != nil {
				cur.DynDNS.Error = err.Error()
				return nil
			}
			cur.DynDNS.IPv4, cur.DynDNS.IPv6, cur.DynDNS.Updated, cur.DynDNS.Error = ip4, ip6, time.Now(), ""
			return nil
		})
		if err != nil {
			log.Printf("Free domain %s: %v", x.Name, err)
		}
	}
	return nil
}

func (u *Updater) update(ctx context.Context, domain, provider, ip4, ip6 string) error {
	token, err := u.token(domain)
	if err != nil {
		return err
	}
	switch provider {
	case "desec":
		// Both addresses are always given (an empty one removes that record), so deSEC never guesses from where
		// the request came from.
		q := url.Values{"hostname": {domain}, "myipv4": {ip4}, "myipv6": {ip6}}
		req, _ := http.NewRequestWithContext(ctx, "GET", DesecUpdate+"?"+q.Encode(), nil)
		req.SetBasicAuth(domain, token)
		body, status, err := u.do(req)
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized {
			return errors.New("deSEC refused the token")
		}
		if status != http.StatusOK || !strings.HasPrefix(body, "good") {
			return fmt.Errorf("deSEC said %d %s", status, short(body))
		}
		return nil
	case "duckdns":
		q := url.Values{"domains": {strings.TrimSuffix(domain, Suffix["duckdns"])}, "token": {token}, "ip": {ip4}, "ipv6": {ip6}}
		req, _ := http.NewRequestWithContext(ctx, "GET", DuckDNSURL+"?"+q.Encode(), nil)
		body, _, err := u.do(req)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(body, "OK") {
			return errors.New("DuckDNS refused the update: check the name was made on duckdns.org with this account, and the token")
		}
		return nil
	}
	return fmt.Errorf("unknown provider %q", provider)
}

// desecCreate makes the domain in the deSEC account the token belongs to (fine if it's already there), with a
// wildcard so app subdomains reach this server too.
func (u *Updater) desecCreate(ctx context.Context, domain, token string) error {
	post := func(path string, v any) (string, int, error) {
		b, _ := json.Marshal(v)
		req, _ := http.NewRequestWithContext(ctx, "POST", DesecAPI+path, strings.NewReader(string(b)))
		req.Header.Set("Authorization", "Token "+token)
		req.Header.Set("Content-Type", "application/json")
		return u.do(req)
	}
	body, status, err := post("/domains/", map[string]string{"name": domain})
	if err != nil {
		return err
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return errors.New("deSEC refused the token")
	case status == http.StatusCreated:
	case strings.Contains(body, "already") || status == http.StatusConflict:
		// Taken: fine if it's this account's own (it can look at it), otherwise someone else has it.
		req, _ := http.NewRequestWithContext(ctx, "GET", DesecAPI+"/domains/"+domain+"/", nil)
		req.Header.Set("Authorization", "Token "+token)
		if _, st, err := u.do(req); err != nil || st != http.StatusOK {
			return fmt.Errorf("%s is taken; try another name", domain)
		}
	default:
		return fmt.Errorf("deSEC couldn't make %s: %s", domain, short(body))
	}
	body, status, err = post("/domains/"+domain+"/rrsets/", map[string]any{
		"subname": "*", "type": "CNAME", "ttl": 3600, "records": []string{domain + "."}})
	if err != nil {
		return err
	}
	if status != http.StatusCreated && !strings.Contains(body, "already") && status != http.StatusConflict {
		return fmt.Errorf("deSEC couldn't add *.%s: %s", domain, short(body))
	}
	return nil
}

func (u *Updater) do(req *http.Request) (string, int, error) {
	req.Header.Set("User-Agent", "Homeport")
	res, err := u.client().Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("couldn't reach %s: %v", req.URL.Host, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	return strings.TrimSpace(string(b)), res.StatusCode, nil
}

// publicIPs asks what address this server's requests come from, over IPv4 and over IPv6 (empty when there's none).
func (u *Updater) publicIPs(ctx context.Context) (ip4, ip6 string) {
	ask := func(url, network string) string {
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		c := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
			},
		}}
		if u.Client != nil && u.Client.Transport != nil {
			c = u.Client // tests
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		res, err := c.Do(req)
		if err != nil {
			return ""
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 100))
		ip := net.ParseIP(strings.TrimSpace(string(b)))
		if ip == nil || (network == "tcp4") != (ip.To4() != nil) {
			return ""
		}
		return ip.String()
	}
	return ask(CheckIPv4URL, "tcp4"), ask(CheckIPv6URL, "tcp6")
}

func short(s string) string {
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
