package apps

import (
	"path/filepath"
	"testing"

	"homeport/internal/state"
)

func TestOriginsHaveTheInternetPort(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	_ = store.Update(func(d *state.Data) error {
		d.Settings().MainDomain = "home.example.com"
		d.AddDomain(state.Domain{Name: "home.example.com", DynDNS: &state.DynDNS{Provider: "cloudflare", Home: true}})
		return nil
	})
	m := &Manager{Store: store, Root: t.TempDir()}
	if v := m.vars(state.App{ID: "x", Domain: "x.home.example.com"}, state.User{}); v.Origins != "https://x.home.example.com" {
		t.Fatalf("port 443: %q", v.Origins)
	}
	_ = store.Update(func(d *state.Data) error { d.Settings().InternetPort = 8443; return nil })
	if v := m.vars(state.App{ID: "x", Domain: "x.home.example.com"}, state.User{}); v.Origins != "https://x.home.example.com,https://x.home.example.com:8443" {
		t.Fatalf("port 8443: %q", v.Origins)
	}
	if v := m.vars(state.App{ID: "y", Domain: "y.other.example"}, state.User{}); v.Origins != "https://y.other.example" {
		t.Fatalf("not under the home name: %q", v.Origins)
	}
}
