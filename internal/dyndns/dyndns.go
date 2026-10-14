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
	"regexp"
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

var domainName = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// HomeName turns what someone typed into a home name: "myhome" (or "myhome.dedyn.io") is myhome.dedyn.io, a free name;
// anything else with a dot is a domain of their own on deSEC (home.example.com, or example.com).
func HomeName(typed string) (string, error) {
	name := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(typed)), ".")
	if !strings.Contains(name, ".") || strings.HasSuffix(name, Suffix["desec"]) {
		return FullName("desec", name)
	}
	if len(name) > 253 || !domainRe(name) {
		return "", fmt.Errorf("%q isn't a domain name like home.example.com (or type just a name, for name.dedyn.io)", typed)
	}
	return name, nil
}

func domainRe(name string) bool { return domainName.MatchString(name) }

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
	var zone string
	switch {
	case home:
		z, err := u.desecHomeZone(ctx, domain, token)
		if err != nil {
			return err
		}
		if z != domain {
			zone = z
		}
	case provider == "desec":
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
		x := state.Domain{Name: domain, Added: time.Now(), DynDNS: &state.DynDNS{Provider: provider, Home: home, Zone: zone}}
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
			if err = u.pointHome(ctx, x, ip4); err == nil {
				err = u.setInternetNames(ctx, x, pub4)
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

// zoneParts is the deSEC domain a home name is in, and the name's part of it ("" when it's the whole domain).
func zoneParts(x state.Domain) (zone, part string) {
	zone = x.Name
	if x.DynDNS != nil && x.DynDNS.Zone != "" {
		zone = x.DynDNS.Zone
	}
	return zone, strings.TrimSuffix(strings.TrimSuffix(x.Name, zone), ".")
}

// under is a record's name in the zone for a name under part (label "" is part itself).
func under(part, label string) string {
	switch {
	case label == "":
		return part
	case part == "":
		return label
	}
	return label + "." + part
}

// setInternetNames points the home name's apps that are on the internet at the public address (their own records
// win over the wildcard), and removes the records it set before for apps that aren't any more. It only ever touches
// those records: anything else in the domain (a website, mail) is left alone.
func (u *Updater) setInternetNames(ctx context.Context, x state.Domain, ip string) error {
	token, err := u.token(x.Name)
	if err != nil {
		return err
	}
	zone, part := zoneParts(x)
	labels := func(set string) map[string]bool {
		out := map[string]bool{}
		if f := strings.Fields(set); len(f) > 1 {
			for _, l := range f[1:] {
				out[l] = true
			}
		}
		return out
	}
	want, had := labels(u.internetNames(x.Name, ip)), labels(x.DynDNS.Internet)
	ttl := u.minTTL(ctx, zone, token)
	var changes []map[string]any
	for l := range had {
		if !want[l] {
			changes = append(changes, map[string]any{"subname": under(part, l), "type": "A", "ttl": ttl, "records": []string{}})
		}
	}
	for l := range want {
		changes = append(changes, map[string]any{"subname": under(part, l), "type": "A", "ttl": ttl, "records": []string{ip}})
	}
	if len(changes) == 0 {
		return nil
	}
	body, status, err := u.api(ctx, "PATCH", "/domains/"+zone+"/rrsets/", token, changes)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("deSEC couldn't change the internet names: %d %s", status, short(body))
	}
	return nil
}

// pointHome points a home name, and every name under it, at the server's home address (set through deSEC's API: the
// update service may refuse private addresses), with no IPv6 address.
func (u *Updater) pointHome(ctx context.Context, x state.Domain, ip string) error {
	token, err := u.token(x.Name)
	if err != nil {
		return err
	}
	zone, part := zoneParts(x)
	ttl := u.minTTL(ctx, zone, token)
	body, status, err := u.api(ctx, "PATCH", "/domains/"+zone+"/rrsets/", token, []map[string]any{
		{"subname": part, "type": "A", "ttl": ttl, "records": []string{ip}},
		{"subname": part, "type": "AAAA", "ttl": ttl, "records": []string{}},
		{"subname": under(part, "*"), "type": "CNAME", "ttl": ttl, "records": []string{x.Name + "."}},
	})
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return errors.New("deSEC refused the token")
	}
	if status/100 != 2 {
		return fmt.Errorf("deSEC couldn't point %s at %s: %d %s", x.Name, ip, status, short(body))
	}
	return nil
}

// lookupNS asks who answers for a domain (tests replace it).
var lookupNS = net.DefaultResolver.LookupNS

// desecHomeZone finds (or makes) the deSEC domain a home name goes in: the name itself for a dedyn.io name, else the
// account's domain it's under (home.example.com in example.com), added to the account when it isn't there yet. A
// domain of your own must use deSEC's nameservers, or nothing would find the records.
func (u *Updater) desecHomeZone(ctx context.Context, name, token string) (string, error) {
	body, status, err := u.api(ctx, "GET", "/domains/", token, nil)
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "", errors.New("deSEC refused the token")
	}
	var have []struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal([]byte(body), &have)
	zone := ""
	for _, d := range have {
		if (name == d.Name || strings.HasSuffix(name, "."+d.Name)) && len(d.Name) > len(zone) {
			zone = d.Name
		}
	}
	if zone == "" && strings.HasSuffix(name, Suffix["desec"]) {
		if err := u.desecCreate(ctx, name, token); err != nil {
			return "", err
		}
		return name, nil
	}
	if zone == "" {
		// The shortest part deSEC takes (it refuses public suffixes like co.uk): example.com for home.example.com.
		labels := strings.Split(name, ".")
		for i := len(labels) - 2; i >= 0 && zone == ""; i-- {
			cand := strings.Join(labels[i:], ".")
			body, status, err := u.api(ctx, "POST", "/domains/", token, map[string]string{"name": cand})
			switch {
			case err != nil:
				return "", err
			case status == http.StatusCreated:
				zone = cand
			case strings.Contains(body, "already") || status == http.StatusConflict:
				return "", fmt.Errorf("%s is in someone else's deSEC account", cand)
			case strings.Contains(strings.ToLower(body), "limit"):
				return "", fmt.Errorf("deSEC won't add %s: %s", cand, short(body))
			}
		}
		if zone == "" {
			return "", fmt.Errorf("deSEC wouldn't add %s: add it in your deSEC account first", name)
		}
	}
	if !strings.HasSuffix(zone, Suffix["desec"]) {
		ns, _ := lookupNS(ctx, zone)
		ok := false
		var names []string
		for _, n := range ns {
			names = append(names, strings.TrimSuffix(n.Host, "."))
			ok = ok || strings.Contains(n.Host, "desec.")
		}
		if !ok {
			now := "no nameservers yet"
			if len(names) > 0 {
				now = "its nameservers are " + strings.Join(names, ", ")
			}
			return "", fmt.Errorf("%s is in your deSEC account, but %s. At the registrar where you bought it, set them to ns1.desec.io and ns2.desec.org, then try again (it can take a few hours to take effect)", zone, now)
		}
	}
	return zone, nil
}

// SetTXT sets (values) or removes (none) the TXT records at name, a name under one of the free domains or home names
// on deSEC (_acme-challenge.home.example.com, for a certificate).
func (u *Updater) SetTXT(ctx context.Context, name string, values []string) error {
	var x state.Domain
	u.Store.Read(func(d *state.Data) {
		for _, y := range d.Domains() {
			if y.DynDNS != nil && y.DynDNS.Provider == "desec" && strings.HasSuffix(name, "."+y.Name) && len(y.Name) > len(x.Name) {
				x = y
			}
		}
	})
	if x.Name == "" {
		return fmt.Errorf("%s isn't under a name from deSEC", name)
	}
	token, err := u.token(x.Name)
	if err != nil {
		return err
	}
	zone, _ := zoneParts(x)
	records := []string{}
	for _, v := range values {
		records = append(records, strconv.Quote(v))
	}
	body, status, err := u.api(ctx, "PATCH", "/domains/"+zone+"/rrsets/", token, []map[string]any{
		{"subname": strings.TrimSuffix(name, "."+zone), "type": "TXT", "ttl": u.minTTL(ctx, zone, token), "records": records}})
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return fmt.Errorf("deSEC couldn't set %s: %d %s", name, status, short(body))
	}
	return nil
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
