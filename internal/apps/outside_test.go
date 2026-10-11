package apps

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"homeport/internal/state"
)

const outsideManifest = `id: notes
name: Notes
secrets: [key]
compose: |
  services:
    app:
      image: example/notes:1
      environment: {KEY: "{{secret "key"}}"}
      ports: ["127.0.0.1:{{.Port}}:80"]
      volumes: ["{{.Data}}/data:/data"]
`

func TestInstallFromOutsideTheCatalog(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	_ = store.Update(func(d *state.Data) error {
		d.Settings().MainDomain = "home.test"
		d.AddDomain(state.Domain{Name: "home.test"})
		return nil
	})
	r := &slowStart{}
	m := &Manager{Store: store, Root: t.TempDir(), Runner: r, SystemCA: "/dev/null", CaddyRoot: "/dev/null"}
	ctx := context.Background()
	src := &state.Source{URL: "https://github.com/o/notes", Commit: strings.Repeat("a", 40), By: "repository"}

	bad := strings.Replace(outsideManifest, `["{{.Data}}/data:/data"]`, `["/:/host"]`, 1)
	if _, err := m.Install(ctx, InstallRequest{Manifest: bad, Source: src, Domain: "notes.home.test"}, state.User{Name: "alice"}, io.Discard); err == nil || !strings.Contains(err.Error(), "mounts /") {
		t.Fatalf("a manifest mounting the server's root must be refused: %v", err)
	}
	if _, err := m.Install(ctx, InstallRequest{Manifest: outsideManifest, Domain: "notes.home.test"}, state.User{Name: "alice"}, io.Discard); err == nil {
		t.Fatal("an outside app needs its source")
	}
	app, err := m.Install(ctx, InstallRequest{Manifest: outsideManifest, Source: src, Domain: "notes.home.test"}, state.User{Name: "alice"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if app.Source == nil || app.Source.URL != src.URL || app.Catalog != "" || app.Secrets["key"] == "" {
		t.Fatalf("installed app: %+v", app)
	}
	compose, _ := os.ReadFile(filepath.Join(m.Dir("notes"), "compose.yaml"))
	if !strings.Contains(string(compose), "from https://github.com/o/notes (commit aaaaaaaaaaaa)") || !strings.Contains(string(compose), app.Secrets["key"]) {
		t.Errorf("compose file:\n%s", compose)
	}
	if b, _ := os.ReadFile(filepath.Join(m.Dir("notes"), "homeport.yaml")); string(b) != outsideManifest {
		t.Error("the manifest should be kept for updates")
	}
	// Updating uses the kept manifest.
	os.Remove(filepath.Join(m.Dir("notes"), "compose.yaml"))
	if err := m.Update(ctx, "notes", io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.Dir("notes"), "compose.yaml")); err != nil {
		t.Error("update should write the compose file again from the kept manifest")
	}
	// A kept manifest that was tampered with is refused on update.
	os.WriteFile(filepath.Join(m.Dir("notes"), "homeport.yaml"), []byte(strings.Replace(outsideManifest, `["{{.Data}}/data:/data"]`, `["/etc:/etc"]`, 1)), 0o600)
	if err := m.Update(ctx, "notes", io.Discard); err == nil {
		t.Error("a manifest that no longer passes the checks must not be used")
	}
}
