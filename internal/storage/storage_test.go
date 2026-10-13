package storage

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestLsblk(t *testing.T) {
	// lsblk prints sizes as numbers or strings, and flags as booleans or "0"/"1", depending on its version.
	var tree struct {
		Blockdevices []lsblkDev `json:"blockdevices"`
	}
	raw := `{"blockdevices":[{"path":"/dev/sdb","type":"disk","fstype":null,"size":8001563222016,"mountpoints":[null],"model":"WDC WD80EFZZ","ro":false,
	  "children":[{"path":"/dev/sdb1","type":"part","fstype":"ext4","label":"big","uuid":"1234","size":"8001561124864","mountpoints":["/mnt/big"],"ro":"0"}]}]}`
	if err := json.Unmarshal([]byte(raw), &tree); err != nil {
		t.Fatal(err)
	}
	c := tree.Blockdevices[0].Children[0]
	if c.Size != 8001561124864 || c.RO || c.FSType != "ext4" || c.Mountpoints[0] != "/mnt/big" || tree.Blockdevices[0].Model != "WDC WD80EFZZ" {
		t.Errorf("got %+v", c)
	}
}

func TestDriveOf(t *testing.T) {
	drives := []Drive{{Mount: "/", Mounted: true}, {Mount: "/mnt/big", Mounted: true}, {Mount: "/mnt/bigger", Mounted: true}, {Device: "/dev/sdc1"}}
	for path, want := range map[string]string{"/var/lib/homeport/apps/x": "/", "/mnt/big/homeport/apps/x": "/mnt/big", "/mnt/bigger/a": "/mnt/bigger", "/mnt/big": "/mnt/big"} {
		if d, ok := DriveOf(drives, path); !ok || d.Mount != want {
			t.Errorf("%s: %q, want %q", path, d.Mount, want)
		}
	}
}

func TestCopyAndSame(t *testing.T) {
	src, dst := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	_ = os.MkdirAll(filepath.Join(src, "data", "media"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "data", "media", "movie.mkv"), make([]byte, 300000), 0o640)
	_ = os.WriteFile(filepath.Join(src, "compose.yaml"), []byte("services: {}\n"), 0o600)
	_ = os.Symlink("compose.yaml", filepath.Join(src, "link"))
	var last [2]int64
	if err := Copy(context.Background(), src, dst, false, 300013, func(d, t int64) { last = [2]int64{d, t} }, io.Discard); err != nil {
		t.Fatal(err)
	}
	if last[0] != last[1] {
		t.Errorf("progress ended at %v", last)
	}
	if err := Same(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(dst, "link")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("links should stay links")
	}
	if fi, _ := os.Stat(filepath.Join(dst, "data", "media", "movie.mkv")); fi == nil || fi.Mode().Perm() != 0o640 {
		t.Error("permissions should be kept")
	}
	// A change after the copy is found; the last pass takes it (and deletes what's gone).
	_ = os.WriteFile(filepath.Join(src, "data", "new.txt"), []byte("new"), 0o600)
	_ = os.Remove(filepath.Join(src, "compose.yaml"))
	if err := Same(context.Background(), src, dst); err == nil {
		t.Error("a difference went unnoticed")
	}
	if err := Copy(context.Background(), src, dst, true, 0, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := Same(context.Background(), src, dst); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "compose.yaml")); err == nil {
		t.Error("the last pass should delete what's gone")
	}
}
