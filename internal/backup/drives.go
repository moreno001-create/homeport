package backup

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Drive is a mounted disk that isn't the system's own: a USB drive, or another disk mounted under /media or /mnt.
type Drive struct {
	Path  string `json:"path"` // where it's mounted
	Label string `json:"label"`
	Size  string `json:"size"`
	Free  string `json:"free"`
}

// Drives lists mounted drives a backup could go to.
func Drives(ctx context.Context) []Drive {
	out := []Drive{}
	if runtime.GOOS == "darwin" {
		entries, _ := os.ReadDir("/Volumes")
		for _, e := range entries {
			p := filepath.Join("/Volumes", e.Name())
			if target, err := os.Readlink(p); err == nil && target == "/" {
				continue // the Mac's own disk
			}
			out = append(out, Drive{Path: p, Label: e.Name()})
		}
		return out
	}
	b, err := exec.CommandContext(ctx, "lsblk", "--json", "--bytes", "--output", "NAME,MOUNTPOINT,LABEL,SIZE,FSAVAIL,TRAN,RM,TYPE").Output()
	if err != nil {
		return out
	}
	var l struct{ Blockdevices []dev }
	if json.Unmarshal(b, &l) != nil {
		return out
	}
	var walk func(ds []dev, usb bool)
	walk = func(ds []dev, usb bool) {
		for _, d := range ds {
			u := usb || d.Tran == "usb" || d.RM
			mp := d.Mountpoint
			if mp != "" && mp != "/" && !strings.HasPrefix(mp, "/boot") && !strings.HasPrefix(mp, "/snap") && mp != "[SWAP]" &&
				(u || strings.HasPrefix(mp, "/media/") || strings.HasPrefix(mp, "/mnt/") || strings.HasPrefix(mp, "/srv/")) {
				out = append(out, Drive{Path: mp, Label: d.Label, Size: human(d.Size), Free: human(d.Fsavail)})
			}
			walk(d.Children, u)
		}
	}
	walk(l.Blockdevices, false)
	return out
}

type dev struct {
	Name       string `json:"name"`
	Mountpoint string `json:"mountpoint"`
	Label      string `json:"label"`
	Size       any    `json:"size"`
	Fsavail    any    `json:"fsavail"`
	Tran       string `json:"tran"`
	RM         bool   `json:"rm"`
	Children   []dev  `json:"children"`
}

func human(v any) string {
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case string:
		_ = json.Unmarshal([]byte(x), &n)
	}
	if n <= 0 {
		return ""
	}
	return size(int64(n))
}
