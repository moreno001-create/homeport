package apps

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"homeport/internal/catalog"
	"homeport/internal/state"
)

// Every app a catalog entry requires is in the catalog.
func TestRequiresAreInTheCatalog(t *testing.T) {
	all, err := catalog.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		for _, id := range a.Requires {
			if _, ok := catalog.Get(id); !ok || id == a.ID {
				t.Errorf("%s requires %q, which isn't another catalog app", a.ID, id)
			}
		}
	}
}

// Installing Cove installs Collabora first, at collabora.<main domain>, and Cove's compose points at it.
func TestInstallingCoveInstallsCollabora(t *testing.T) {
	dir := t.TempDir()
	store, _ := state.Open(filepath.Join(dir, "state.json"))
	_ = store.Update(func(d *state.Data) error {
		d.Settings().MainDomain = "home.example.com"
		d.AddDomain(state.Domain{Name: "home.example.com"})
		d.AddUser(state.User{Name: "alice", Admin: true})
		return nil
	})
	r := &slowStart{}
	m := &Manager{Store: store, Root: filepath.Join(dir, "apps"), Runner: r}
	var log bytes.Buffer
	if _, err := m.Install(context.Background(), InstallRequest{Catalog: "cove", Domain: "cove.home.example.com"}, state.User{Name: "alice", Admin: true}, &log); err != nil {
		t.Fatalf("installing Cove: %v\n%s", err, log.String())
	}
	var ids []string
	store.Read(func(d *state.Data) {
		for _, a := range d.Apps() {
			ids = append(ids, a.ID+"@"+a.Domain)
		}
	})
	if strings.Join(ids, " ") != "collabora@collabora.home.example.com cove@cove.home.example.com" {
		t.Errorf("installed: %v\n%s", ids, log.String())
	}
	compose, _ := os.ReadFile(filepath.Join(dir, "apps", "cove", "compose.yaml"))
	if !strings.Contains(string(compose), `COVE_COLLABORA: "https://collabora.home.example.com"`) || strings.Contains(string(compose), "collabora/code") {
		t.Errorf("Cove's compose:\n%s", compose)
	}
	// installing another Cove doesn't make a second Collabora
	log.Reset()
	if _, err := m.Install(context.Background(), InstallRequest{Catalog: "cove", ID: "cove-2", Domain: "family.home.example.com"}, state.User{Name: "alice", Admin: true}, &log); err != nil {
		t.Fatalf("a second Cove: %v", err)
	}
	n := 0
	store.Read(func(d *state.Data) {
		for _, a := range d.Apps() {
			if a.Catalog == "collabora" {
				n++
			}
		}
	})
	if n != 1 {
		t.Errorf("%d Collaboras", n)
	}
}
