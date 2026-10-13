package apps

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"homeport/internal/state"
)

// An app's folder moves to another drive (a link to it is left in its place) and back, data and all.
func TestMoveAppToAnotherDriveAndBack(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	_ = store.Update(func(d *state.Data) error {
		d.AddApp(state.App{ID: "jellyfin", Catalog: "jellyfin", Name: "Jellyfin", Domain: "jellyfin.home.test", Port: 18010})
		return nil
	})
	root, big := t.TempDir(), t.TempDir()
	m := &Manager{Store: store, Root: root, Runner: &slowStart{}}
	movie := filepath.Join("data", "media", "movie.mkv")
	_ = os.MkdirAll(filepath.Join(root, "jellyfin", "data", "media"), 0o750)
	_ = os.WriteFile(filepath.Join(root, "jellyfin", movie), []byte("a movie"), 0o640)
	_ = os.WriteFile(filepath.Join(root, "jellyfin", "compose.yaml"), []byte("services: {}\n"), 0o600)

	var log bytes.Buffer
	if err := m.Move(context.Background(), "jellyfin", filepath.Join(big, "homeport", "apps"), nil, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	link := filepath.Join(root, "jellyfin")
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("its folder should be a link now (%v)", err)
	}
	want := filepath.Join(big, "homeport", "apps", "jellyfin")
	if real, _ := filepath.EvalSymlinks(want); m.Dir("jellyfin") != real {
		t.Errorf("Dir = %s, want %s", m.Dir("jellyfin"), want)
	}
	if b, err := os.ReadFile(filepath.Join(link, movie)); err != nil || string(b) != "a movie" {
		t.Errorf("the movie didn't come along: %q %v", b, err)
	}
	if _, err := os.Stat(link + ".homeport-old"); err == nil {
		t.Error("the old copy should be gone")
	}
	if u := unfinishedOf(t, m, "jellyfin"); u != "" {
		t.Errorf("still marked %q", u)
	}

	// And back.
	if err := m.Move(context.Background(), "jellyfin", "", nil, &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if fi, err := os.Lstat(link); err != nil || !fi.IsDir() {
		t.Fatal("its folder should be a real folder again")
	}
	if b, _ := os.ReadFile(filepath.Join(link, movie)); string(b) != "a movie" {
		t.Error("the movie didn't come back")
	}
	if _, err := os.Stat(want); err == nil {
		t.Error("the copy on the other drive should be gone")
	}

	// New apps can go straight to another drive; removing one deletes its folder there too.
	_ = os.MkdirAll(filepath.Join(big, "apps"), 0o700)
	if err := m.placeNew("new", filepath.Join(big, "apps")); err != nil {
		t.Fatal(err)
	}
	if real := m.realDir("new"); real == filepath.Join(root, "new") {
		t.Error("the new app's folder should be on the other drive")
	}
	m.removeDir("new")
	if _, err := os.Stat(filepath.Join(big, "apps", "new")); err == nil {
		t.Error("its folder on the other drive should be gone")
	}
}
