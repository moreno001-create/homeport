package aria

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// With the real aria2 (ARIA_LIVE=1, aria2c installed): a link and a .torrent link from Kiwix, to the end.
func TestLive(t *testing.T) {
	if os.Getenv("ARIA_LIVE") == "" {
		t.Skip("ARIA_LIVE not set")
	}
	m := &Manager{StateDir: t.TempDir()}
	ctx := context.Background()
	if err := m.Ready(ctx, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ dir, uri, file string }{
		{"http", "https://download.kiwix.org/zim/other/zimgit-water_en_2024-08.zim", "zimgit-water_en_2024-08.zim"},
		{"torrent", "https://download.kiwix.org/zim/other/zimgit-water_en_2024-08.zim.torrent", "zimgit-water_en_2024-08.zim"},
		{"magnet", os.Getenv("ARIA_MAGNET"), "zimgit-water_en_2024-08.zim"},
	} {
		dir := filepath.Join(t.TempDir(), c.dir)
		os.MkdirAll(dir, 0o755)
		if _, err := m.Add(ctx, c.uri, dir); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Minute)
		for {
			list, _ := m.List(ctx)
			var mine []Download
			done := false
			for _, d := range list {
				if d.Dir == dir {
					mine = append(mine, d)
					done = done || (d.Name == c.file && d.Status == "complete")
				}
			}
			t.Logf("%s: %+v", c.dir, mine)
			if fi, err := os.Stat(filepath.Join(dir, c.file)); err == nil && done {
				if entries, _ := os.ReadDir(dir); len(entries) != 1 {
					t.Fatalf("only the file itself should be left: %v", entries)
				}
				if _, err := os.Stat(filepath.Join(dir, c.file+".aria2")); err == nil {
					t.Fatal("the control file should be gone once complete")
				}
				t.Logf("%s: done, %d bytes, mode %v", c.dir, fi.Size(), fi.Mode())
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: not finished in 3 minutes", c.dir)
			}
			time.Sleep(2 * time.Second)
		}
	}
}
