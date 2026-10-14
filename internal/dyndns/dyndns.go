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
	"slices"
	"strconv"
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
	// LocalIPv4 is this server's address on the home network, for home names (DynDNS.Home).
	LocalIPv4 func() string

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
	return u.register(ctx, provider, domain, token, false)
}

// RegisterHome sets up a home name on deSEC: it points at this server's home-network address (it and every name under
// it), so every device at home finds the server with ordinary DNS, and it can have a real certificate (see Solver).
func (u *Updater) RegisterHome(ctx context.Context, domain, token string) error {
	return u.register(ctx, "desec", domain, token, true)
}

func (u *Updater) register(ctx context.Context, provider, domain, token string, home bool) error {
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
		x := state.Domain{Name: domain, Added: time.Now(), DynDNS: &state.DynDNS{Provider: provider, Home: home}}
		if home {
			x.Cert = state.CertDNS
		}
		d.AddDomain(x)
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
	pub4, pub6 := u.publicIPs(ctx)
	if pub4 == "" && pub6 == "" {
		return errors.New("couldn't find this server's public address (no internet?)")
	}
	for _, x := range todo {
		dd := x.DynDNS
		ip4, ip6 := pub4, pub6
		var internet string // home names: the apps on the internet, and where they point
		if dd.Home {
			// The home address only (a public IPv6 address would let devices go around the home one).
			ip4, ip6 = "", ""
			if u.LocalIPv4 != nil {
				ip4 = u.LocalIPv4()
			}
			if ip4 == "" {
				continue
			}
			internet = u.internetNames(x.Name, pub4)
		}
		if !force && dd.Error == "" && dd.IPv4 == ip4 && dd.IPv6 == ip6 && dd.Internet == internet && time.Since(dd.Updated) < 24*time.Hour {
			continue
		}
		var err error
		if dd.Home {
			if err = u.pointHome(ctx, x.Name, ip4); err == nil {
				err = u.setInternetNames(ctx, x.Name, pub4)
			}
		} else {
			err = u.update(ctx, x.Name, dd.Provider, ip4, ip6)
		}
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
			cur.DynDNS.Internet = internet
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

// MARK: Home names

// internetNames is what a home name's internet records should be: "ip label label…" for the apps put on the internet
// (App.Internet), or "" when there are none.
func (u *Updater) internetNames(domain, ip string) string {
	var labels []string
	u.Store.Read(func(d *state.Data) {
		for _, a := range d.Apps() {
			if a.Internet && strings.HasSuffix(a.Domain, "."+domain) {
				labels = append(labels, strings.TrimSuffix(a.Domain, "."+domain))
			}
		}
		// People outside sign in there (state.SignInName).
		if len(labels) > 0 && d.SignInName() == "signin."+domain {
			labels = append(labels, "signin")
		}
	})
	if len(labels) == 0 || ip == "" {
		return ""
	}
	slices.Sort(labels)
	return ip + " " + strings.Join(labels, " ")
}

// setInternetNames points the home name's apps that are on the internet at the public address (their own records
// win over the wildcard), and removes the records of apps that aren't any more.
func (u *Updater) setInternetNames(ctx context.Context, domain, ip string) error {
	token, err := u.token(domain)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	if f := strings.Fields(u.internetNames(domain, ip)); len(f) > 1 {
		for _, l := range f[1:] {
			want[l] = true
		}
	}
	body, status, err := u.api(ctx, "GET", "/domains/"+domain+"/rrsets/?type=A", token, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("deSEC said %d %s", status, short(body))
	}
	var have []struct {
		Subname string `json:"subname"`
	}
	_ = json.Unmarshal([]byte(body), &have)
	ttl := u.minTTL(ctx, domain, token)
	var changes []map[string]any
	for _, h := range have {
		if h.Subname != "" && h.Subname != "*" && !want[h.Subname] {
			changes = append(changes, map[string]any{"subname": h.Subname, "type": "A", "ttl": ttl, "records": []string{}})
		}
	}
	for l := range want {
		changes = append(changes, map[string]any{"subname": l, "type": "A", "ttl": ttl, "records": []string{ip}})
	}
	if len(changes) == 0 {
		return nil
	}
	body, status, err = u.api(ctx, "PATCH", "/domains/"+domain+"/rrsets/", token, changes)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("deSEC couldn't change the internet names: %d %s", status, short(body))
	}
	return nil
}

// pointHome points a home name at the server's home address (the update service may refuse private addresses, so
// it's set through deSEC's API), with no IPv6 address.
func (u *Updater) pointHome(ctx context.Context, domain, ip string) error {
	token, err := u.token(domain)
	if err != nil {
		return err
	}
	ttl := u.minTTL(ctx, domain, token)
	body, status, err := u.api(ctx, "PATCH", "/domains/"+domain+"/rrsets/", token, []map[string]any{
		{"subname": "", "type": "A", "ttl": ttl, "records": []string{ip}},
		{"subname": "", "type": "AAAA", "ttl": ttl, "records": []string{}},
	})
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return errors.New("deSEC refused the token")
	}
	if status/100 != 2 {
		return fmt.Errorf("deSEC couldn't point %s at %s: %d %s", domain, ip, status, short(body))
	}
	return nil
}

// SetTXT sets (values) or removes (none) the TXT records at name, a name under one of the free domains on deSEC
// (_acme-challenge.home.dedyn.io, for a certificate).
func (u *Updater) SetTXT(ctx context.Context, name string, values []string) error {
	domain, sub, err := u.zoneOf(name)
	if err != nil {
		return err
	}
	token, err := u.token(domain)
	if err != nil {
		return err
	}
	records := []string{}
	for _, v := range values {
		records = append(records, strconv.Quote(v))
	}
	body, status, err := u.api(ctx, "PATCH", "/domains/"+domain+"/rrsets/", token, []map[string]any{
		{"subname": sub, "type": "TXT", "ttl": u.minTTL(ctx, domain, token), "records": records}})
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("deSEC couldn't set %s: %d %s", name, status, short(body))
	}
	return nil
}

// zoneOf finds the free domain a name is under, and the name's part before it.
func (u *Updater) zoneOf(name string) (domain, sub string, err error) {
	u.Store.Read(func(d *state.Data) {
		for _, x := range d.Domains() {
			if x.DynDNS != nil && x.DynDNS.Provider == "desec" && strings.HasSuffix(name, "."+x.Name) {
				domain, sub = x.Name, strings.TrimSuffix(name, "."+x.Name)
			}
		}
	})
	if domain == "" {
		return "", "", fmt.Errorf("%s isn't under a free domain from deSEC", name)
	}
	return domain, sub, nil
}

// minTTL is the shortest time records may be cached for on this domain (deSEC sets it per domain; dedyn.io names
// allow short ones).
func (u *Updater) minTTL(ctx context.Context, domain, token string) int {
	body, status, err := u.api(ctx, "GET", "/domains/"+domain+"/", token, nil)
	var d struct {
		MinimumTTL int `json:"minimum_ttl"`
	}
	if err == nil && status == http.StatusOK && json.Unmarshal([]byte(body), &d) == nil && d.MinimumTTL > 0 {
		return max(d.MinimumTTL, 60)
	}
	return 3600
}

func (u *Updater) api(ctx context.Context, method, path, token string, v any) (string, int, error) {
	var r io.Reader
	if v != nil {
		b, _ := json.Marshal(v)
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, DesecAPI+path, r)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Token "+token)
	req.Header.Set("Content-Type", "application/json")
	return u.do(req)
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
// PublicIPv4 is this home's address on the internet, as the outside sees it ("" when it can't tell).
func (u *Updater) PublicIPv4(ctx context.Context) string {
	ip4, _ := u.publicIPs(ctx)
	return ip4
}

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
