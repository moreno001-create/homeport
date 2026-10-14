package dyndns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"homeport/internal/state"
)

// MARK: Cloudflare
//
// A home name can also be in a domain of your own on Cloudflare's DNS (free): Homeport uses an API token with
// permission to edit the domain's DNS ("Edit zone DNS"). It only ever changes the home name's own records: the name,
// *.name, apps put on the internet, and the proof Let's Encrypt checks.

// CloudflareAPI is Cloudflare's API (tests point it elsewhere).
var CloudflareAPI = "https://api.cloudflare.com/client/v4"

type cfZone struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Status      string   `json:"status"` // active once the domain uses Cloudflare's nameservers
	NameServers []string `json:"name_servers"`
}

type cfRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

// cf calls Cloudflare's API, reading the result into out.
func (u *Updater) cf(ctx context.Context, method, path, token string, v, out any) error {
	var r io.Reader
	if v != nil {
		b, _ := json.Marshal(v)
		r = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, CloudflareAPI+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	body, status, err := u.do(req)
	if err != nil {
		return err
	}
	var res struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.Unmarshal([]byte(body), &res)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return errors.New("Cloudflare refused the token (it needs \"Edit zone DNS\" for the domain)")
	}
	if !res.Success {
		var msgs []string
		for _, e := range res.Errors {
			msgs = append(msgs, e.Message)
		}
		if len(msgs) == 0 {
			msgs = append(msgs, fmt.Sprintf("%d %s", status, short(body)))
		}
		return fmt.Errorf("Cloudflare said: %s", strings.Join(msgs, "; "))
	}
	if out != nil {
		return json.Unmarshal(res.Result, out)
	}
	return nil
}

// cfHomeZone finds the domain (zone) a home name is in among those the token can see, and checks it's active (uses
// Cloudflare's nameservers).
func (u *Updater) cfHomeZone(ctx context.Context, name, token string) (cfZone, error) {
	var best cfZone
	for page := 1; page <= 20; page++ {
		var zones []cfZone
		if err := u.cf(ctx, "GET", fmt.Sprintf("/zones?per_page=50&page=%d", page), token, nil, &zones); err != nil {
			return cfZone{}, err
		}
		for _, z := range zones {
			if (name == z.Name || strings.HasSuffix(name, "."+z.Name)) && len(z.Name) > len(best.Name) {
				best = z
			}
		}
		if len(zones) < 50 {
			break
		}
	}
	if best.ID == "" {
		return cfZone{}, fmt.Errorf("none of the domains this token can see holds %s: add the domain to Cloudflare, and give the token \"Edit zone DNS\" for it", name)
	}
	if best.Status != "active" {
		return cfZone{}, fmt.Errorf("%s is on Cloudflare but not active yet: at the registrar where you bought it, set its nameservers to %s (it can take a few hours)",
			best.Name, strings.Join(best.NameServers, " and "))
	}
	return best, nil
}

// cfSet makes the records of a type at a name exactly values (none: removes them).
func (u *Updater) cfSet(ctx context.Context, token, zoneID, typ, name string, values []string) error {
	var have []cfRecord
	q := url.Values{"type": {typ}, "name": {name}, "per_page": {"100"}}
	if err := u.cf(ctx, "GET", "/zones/"+zoneID+"/dns_records?"+q.Encode(), token, nil, &have); err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, r := range have {
		want := false
		for _, v := range values {
			want = want || v == r.Content
		}
		if want && !keep[r.Content] {
			keep[r.Content] = true
			continue
		}
		if err := u.cf(ctx, "DELETE", "/zones/"+zoneID+"/dns_records/"+r.ID, token, nil, nil); err != nil {
			return err
		}
	}
	for _, v := range values {
		if keep[v] {
			continue
		}
		rec := cfRecord{Type: typ, Name: name, Content: v, TTL: 60}
		if err := u.cf(ctx, "POST", "/zones/"+zoneID+"/dns_records", token, rec, nil); err != nil {
			return err
		}
	}
	return nil
}

func (u *Updater) cfPointHome(ctx context.Context, x state.Domain, ip string) error {
	token, err := u.token(x.Name)
	if err != nil {
		return err
	}
	id := x.DynDNS.ZoneID
	for _, step := range []struct {
		typ, name string
		values    []string
	}{{"A", x.Name, []string{ip}}, {"AAAA", x.Name, nil}, {"A", "*." + x.Name, []string{ip}}} {
		if err := u.cfSet(ctx, token, id, step.typ, step.name, step.values); err != nil {
			return err
		}
	}
	return nil
}

func (u *Updater) cfInternetNames(ctx context.Context, x state.Domain, ip string) error {
	token, err := u.token(x.Name)
	if err != nil {
		return err
	}
	want, had := labelSet(u.internetNames(x.Name, ip)), labelSet(x.DynDNS.Internet)
	for l := range had {
		if !want[l] {
			if err := u.cfSet(ctx, token, x.DynDNS.ZoneID, "A", l+"."+x.Name, nil); err != nil {
				return err
			}
		}
	}
	for l := range want {
		if err := u.cfSet(ctx, token, x.DynDNS.ZoneID, "A", l+"."+x.Name, []string{ip}); err != nil {
			return err
		}
	}
	return nil
}

// labelSet reads "ip label label…" (DynDNS.Internet) into its labels.
func labelSet(set string) map[string]bool {
	out := map[string]bool{}
	if f := strings.Fields(set); len(f) > 1 {
		for _, l := range f[1:] {
			out[l] = true
		}
	}
	return out
}

func (u *Updater) cfSetTXT(ctx context.Context, x state.Domain, name string, values []string) error {
	token, err := u.token(x.Name)
	if err != nil {
		return err
	}
	return u.cfSet(ctx, token, x.DynDNS.ZoneID, "TXT", name, values)
}
