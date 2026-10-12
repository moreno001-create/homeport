package server

import (
	"net/http"
	"net/url"
	"testing"

	"homeport/internal/state"
)

func TestPortalAlsoOnHomeNames(t *testing.T) {
	r := newRig(t)
	_ = r.srv.Store.Update(func(d *state.Data) error {
		d.Settings().MainDomain = "wh91.duckdns.org"
		d.AddDomain(state.Domain{Name: "homeport.local"})
		d.AddDomain(state.Domain{Name: "cloud.homeport.local"})
		return nil
	})
	r.install("whoami", "cloud.homeport.local", &state.Access{Public: true})
	b := r.browser()
	if w := b.do("GET", "https://homeport.local/login", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("the sign-in page on homeport.local: %d", w.Code)
	}
	b.do("POST", "https://homeport.local/login", url.Values{"name": {"alice"}, "password": {"correct horse"}}, nil)
	if w := b.do("GET", "https://homeport.local/admin", nil, nil); w.Code != http.StatusOK {
		t.Fatalf("the admin panel on homeport.local after signing in there: %d", w.Code)
	}
	if w := b.do("GET", "https://cloud.homeport.local/login", nil, nil); w.Code != http.StatusNotFound {
		t.Fatalf("an app's own .local name isn't the portal: %d", w.Code)
	}
}
