package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RustDesk's notes show the key the server makes when it first starts, from its data folder.
func TestNotesShowAFileFromTheDataFolder(t *testing.T) {
	a, _ := Get("rustdesk")
	if _, err := a.Render(Vars{ID: "rustdesk", Domain: "rustdesk.home.test", Port: 18001, Data: "/tmp/hp/rustdesk/data", MainDomain: "home.test"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if notes := a.RenderNotes(Vars{Data: dir, MainDomain: "home.test"}); !strings.Contains(notes, "not there yet") {
		t.Fatalf("before the key: %s", notes)
	}
	os.MkdirAll(filepath.Join(dir, "data"), 0o755)
	os.WriteFile(filepath.Join(dir, "data", "id_ed25519.pub"), []byte("AbCdEfKEY=\n"), 0o644)
	notes := a.RenderNotes(Vars{Data: dir, MainDomain: "home.test"})
	if !strings.Contains(notes, "- Key: AbCdEfKEY=") || !strings.Contains(notes, "- ID server: home.test") {
		t.Fatalf("with the key: %s", notes)
	}
	if got := dataFile(dir, "../../etc/passwd"); got != "(not available)" {
		t.Fatalf("outside the folder: %q", got)
	}
}
