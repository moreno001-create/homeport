package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"homeport/internal/state"
)

// "How to reach this server" gives each person the addresses they can open, with the internet port.
func TestReach(t *testing.T) {
	r := newRig(t)
	_ = r.srv.Store.Update(func(d *state.Data) error {
		d.Settings().MainDomain = "home.dedyn.io"
		d.Settings().InternetPort = 8443
		d.Settings().HomeOnInternet = true
		d.AddDomain(state.Domain{Name: "home.dedyn.io", Cert: state.CertDNS, DynDNS: &state.DynDNS{Provider: "desec", Home: true}})
		return nil
	})
	r.install("mealie", "mealie.home.dedyn.io", &state.Access{Groups: []string{"family"}})
	r.install("whoami", "who.home.dedyn.io", &state.Access{Groups: []string{"admins"}})
	r.srv.LocalAddrs = func() []string { return []string{"192.168.7.3"} }

	get := func(name, password string) reach {
		b := r.browser()
		b.do("POST", "https://home.dedyn.io/login", url.Values{"name": {name}, "password": {password}, "return": {"/"}}, nil)
		w := b.do("GET", "https://home.dedyn.io/api/me/reach", nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
		var x reach
		_ = json.Unmarshal(w.Body.Bytes(), &x)
		return x
	}
	bob := get("bob", "battery staple")
	if bob.Port != 8443 || bob.Portal.URL != "https://home.dedyn.io:8443/" || !bob.Portal.Internet || bob.AI == nil || bob.AI.URL != "https://ai.home.dedyn.io:8443/" {
		t.Fatalf("portal and AI page with the port: %+v", bob)
	}
	if len(bob.Apps) != 1 || bob.Apps[0].URL != "https://mealie.home.dedyn.io:8443/" || bob.Admin != nil {
		t.Fatalf("only bob's apps, no admin part: %+v", bob)
	}
	if len(bob.Local) != 1 || bob.Local[0] != "https://192.168.7.3/" {
		t.Fatalf("home addresses: %v", bob.Local)
	}
	alice := get("alice", "correct horse")
	if len(alice.Apps) != 1 || alice.Apps[0].Name != "Who Am I" || alice.Admin == nil || alice.Admin.AdminURL != "https://home.dedyn.io:8443/admin" ||
		alice.Admin.Router != "outside port 8443 → 192.168.7.3 port 443" {
		t.Fatalf("admins get the admin part: %+v %+v", alice, alice.Admin)
	}
	if w := r.browser().do("GET", "https://home.dedyn.io/api/me/reach", nil, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d", w.Code)
	}
}
