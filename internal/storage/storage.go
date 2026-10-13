// Package storage finds this machine's drives and moves folders between them safely: copied while things keep running,
// copied again (only what changed) once they're stopped, checked, and only then switched over.
package storage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Drive is a drive (a filesystem) things can live on.
type Drive struct {
	Mount   string `json:"mount"`  // where it's mounted ("" when it isn't)
	Device  string `json:"device"` // /dev/sdb1
	Label   string `json:"label,omitempty"`
	Model   string `json:"model,omitempty"`
	FS      string `json:"fs"`
	Size    int64  `json:"size"`
	Free    int64  `json:"free"`
	System  bool   `json:"system"`        // the drive Homeport (and the system) is on
	Usable  bool   `json:"usable"`        // things can be moved here
	Why     string `json:"why,omitempty"` // why not, or what to know
	Mounted bool   `json:"mounted"`
	// Lasting: it's mounted again after a restart (in /etc/fstab, or the system drive).
	Lasting bool   `json:"lasting"`
	UUID    string `json:"uuid,omitempty"`
}

// linuxFS are filesystems that keep Linux file owners and permissions (apps' databases need them).
var linuxFS = map[string]bool{"ext4": true, "ext3": true, "xfs": true, "btrfs": true, "f2fs": true, "zfs": true}

// macFS are a Mac's own filesystems.
var macFS = map[string]bool{"apfs": true, "hfs": true}

// Drives lists the drives: mounted ones with their space, and Linux ones that aren't mounted yet. systemDir is a
// folder on the system drive (Homeport's own).
func Drives(ctx context.Context, systemDir string) ([]Drive, error) {
	if runtime.GOOS == "darwin" {
		return macDrives(ctx, systemDir)
	}
	out, err := exec.CommandContext(ctx, "lsblk", "-J", "-b", "-o", "PATH,TYPE,FSTYPE,LABEL,UUID,SIZE,FSAVAIL,FSSIZE,MOUNTPOINTS,MODEL,RO").Output()
	if err != nil {
		return nil, fmt.Errorf("couldn't list the drives: %w", err)
	}
	var tree struct {
		Blockdevices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &tree); err != nil {
		return nil, err
	}
	fstab := fstabTargets()
	sysDev := deviceOf(systemDir)
	var list []Drive
	var walk func(d lsblkDev, model string)
	walk = func(d lsblkDev, model string) {
		if d.Model != "" {
			model = d.Model
		}
		for _, c := range d.Children {
			walk(c, model)
		}
		// (ZFS pools are listed from their mounts instead: lsblk doesn't show where their datasets are.)
		if d.FSType == "" || d.FSType == "swap" || d.FSType == "LVM2_member" || d.FSType == "crypto_LUKS" ||
			d.FSType == "linux_raid_member" || d.FSType == "squashfs" || d.FSType == "zfs_member" {
			return
		}
		mount := ""
		for _, m := range d.Mountpoints {
			if m != "" && (mount == "" || len(m) < len(mount)) {
				mount = m
			}
		}
		if mount == "/boot" || mount == "/boot/efi" || strings.HasPrefix(mount, "/snap/") {
			return
		}
		dr := Drive{Mount: mount, Device: d.Path, Label: d.Label, Model: strings.TrimSpace(model), FS: d.FSType, UUID: d.UUID,
			Size: d.Size, Mounted: mount != ""}
		if dr.Mounted {
			var st syscall.Statfs_t
			if syscall.Statfs(mount, &st) == nil {
				dr.Size, dr.Free = int64(st.Blocks)*int64(st.Bsize), int64(st.Bavail)*int64(st.Bsize)
			}
			dr.System = deviceOf(mount) == sysDev && sysDev != 0
			dr.Lasting = dr.System || mount == "/" || fstab[mount]
		}
		switch {
		case dr.System:
			dr.Usable = true
		case !linuxFS[dr.FS]:
			dr.Why = fmt.Sprintf("It's formatted as %s, which can't keep Linux file owners and permissions (apps' databases would break). Formatting it as ext4 makes it usable, and erases it.", dr.FS)
		case d.RO:
			dr.Why = "It's read-only."
		case !dr.Mounted:
			dr.Why = "It isn't mounted yet: Homeport can mount it (and keep it mounted after restarts)."
		case !dr.Lasting:
			dr.Usable = true
			dr.Why = "It's mounted only until the next restart (opened from the desktop, say). Keep it mounted first, so apps on it come back after a restart."
		default:
			dr.Usable = true
		}
		list = append(list, dr)
	}
	for _, d := range tree.Blockdevices {
		walk(d, "")
	}
	list = append(list, zfsDrives(ctx, systemDir)...)
	if slices.ContainsFunc(list, func(d Drive) bool { return d.FS == "zfs" && d.System }) {
		for i := range list {
			if list[i].FS != "zfs" {
				list[i].System = false
			}
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].System && !list[j].System })
	return list, nil
}

type lsblkDev struct {
	Path        string
	Type        string
	FSType      string
	Label       string
	UUID        string
	Size        int64
	Mountpoints []string
	Model       string
	RO          bool
	Children    []lsblkDev
}

// UnmarshalJSON reads lsblk's JSON, whose numbers and flags are strings or not depending on its version.
func (d *lsblkDev) UnmarshalJSON(b []byte) error {
	var raw struct {
		Path        string     `json:"path"`
		Type        string     `json:"type"`
		FSType      string     `json:"fstype"`
		Label       string     `json:"label"`
		UUID        string     `json:"uuid"`
		Size        any        `json:"size"`
		Mountpoints []string   `json:"mountpoints"`
		Model       string     `json:"model"`
		RO          any        `json:"ro"`
		Children    []lsblkDev `json:"children"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	size, _ := strconv.ParseFloat(fmt.Sprint(raw.Size), 64)
	ro := fmt.Sprint(raw.RO)
	*d = lsblkDev{Path: raw.Path, Type: raw.Type, FSType: raw.FSType, Label: raw.Label, UUID: raw.UUID, Size: int64(size),
		Mountpoints: raw.Mountpoints, Model: raw.Model, RO: ro == "true" || ro == "1", Children: raw.Children}
	return nil
}

// zfsDrives are the ZFS pools with mounted datasets, each as one drive at its top mount point (/ for the system's
// pool). ZFS mounts them itself at startup.
func zfsDrives(ctx context.Context, systemDir string) []Drive {
	out, err := exec.CommandContext(ctx, "findmnt", "-n", "-l", "-o", "TARGET,SOURCE", "-t", "zfs").Output()
	if err != nil {
		return nil
	}
	tops := zfsTops(string(out))
	var sysPool string
	if src, err := exec.CommandContext(ctx, "findmnt", "-n", "-o", "SOURCE", "-T", systemDir).Output(); err == nil {
		sysPool = strings.SplitN(strings.TrimSpace(string(src)), "/", 2)[0]
	}
	sizes := map[string]int64{}
	if b, err := exec.CommandContext(ctx, "zpool", "list", "-H", "-p", "-o", "name,size").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if f := strings.Fields(line); len(f) == 2 {
				sizes[f[0]], _ = strconv.ParseInt(f[1], 10, 64)
			}
		}
	}
	var list []Drive
	for pool, mount := range tops {
		if mount == "/boot" || strings.HasPrefix(mount, "/boot/") {
			continue
		}
		d := Drive{Mount: mount, Device: pool, Label: pool + " (ZFS)", FS: "zfs", Mounted: true, Lasting: true, Usable: true,
			System: pool == sysPool, Size: sizes[pool]}
		var st syscall.Statfs_t
		if syscall.Statfs(mount, &st) == nil {
			d.Free = int64(st.Bavail) * int64(st.Bsize)
			if d.Size == 0 {
				d.Size = int64(st.Blocks) * int64(st.Bsize)
			}
		}
		list = append(list, d)
	}
	return list
}

// zfsTops reads findmnt's "TARGET SOURCE" lines: each pool's shortest mount point.
func zfsTops(out string) map[string]string {
	tops := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pool := strings.SplitN(f[1], "/", 2)[0]
		if cur, ok := tops[pool]; !ok || len(f[0]) < len(cur) {
			tops[pool] = f[0]
		}
	}
	return tops
}

func deviceOf(path string) uint64 {
	var st syscall.Stat_t
	if syscall.Stat(path, &st) != nil {
		return 0
	}
	return uint64(st.Dev)
}

// SameDrive says whether two paths are on the same drive.
func SameDrive(a, b string) bool {
	da, db := deviceOf(a), deviceOf(b)
	return da != 0 && da == db
}

func fstabTargets() map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile("/etc/fstab")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && !strings.HasPrefix(f[0], "#") {
			out[f[1]] = true
		}
	}
	return out
}

var dfLine = regexp.MustCompile(`^(\S+)\s+(\d+)\s+(\d+)\s+(\d+)\s+\S+\s+(/.*)$`)

func macDrives(ctx context.Context, systemDir string) ([]Drive, error) {
	out, err := exec.CommandContext(ctx, "df", "-k", "-P").Output()
	if err != nil {
		return nil, err
	}
	types := map[string]string{}
	if m, err := exec.CommandContext(ctx, "mount").Output(); err == nil {
		for _, line := range strings.Split(string(m), "\n") { // /dev/disk3s1 on /System/Volumes/Data (apfs, local, …)
			if _, rest, ok := strings.Cut(line, " on "); ok {
				if at, kind, ok := strings.Cut(rest, " ("); ok {
					types[at] = strings.SplitN(kind, ",", 2)[0]
				}
			}
		}
	}
	sysDev := deviceOf(systemDir)
	var list []Drive
	for _, line := range strings.Split(string(out), "\n") {
		m := dfLine.FindStringSubmatch(line)
		if m == nil || !strings.HasPrefix(m[1], "/dev/") {
			continue
		}
		at := m[5]
		system := deviceOf(at) == sysDev
		if !system && !strings.HasPrefix(at, "/Volumes/") {
			continue
		}
		size, _ := strconv.ParseInt(m[2], 10, 64)
		free, _ := strconv.ParseInt(m[4], 10, 64)
		d := Drive{Mount: at, Device: m[1], Label: filepath.Base(at), FS: types[at], Size: size * 1024, Free: free * 1024,
			System: system, Mounted: true, Lasting: true}
		if system {
			d.Label = "This Mac"
		}
		switch {
		case system || macFS[d.FS]:
			d.Usable = true
		default:
			d.Why = fmt.Sprintf("It's formatted as %s, which can't keep file owners and permissions (apps' databases would break). Formatting it as APFS in Disk Utility makes it usable, and erases it.", d.FS)
		}
		list = append(list, d)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].System && !list[j].System })
	return list, nil
}

// DriveOf is the drive a path is on (the mounted drive with the longest mount point above it).
func DriveOf(drives []Drive, path string) (Drive, bool) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		real = path
	}
	best := -1
	for i, d := range drives {
		if !d.Mounted {
			continue
		}
		if real == d.Mount || strings.HasPrefix(real, strings.TrimSuffix(d.Mount, "/")+"/") {
			if best < 0 || len(d.Mount) > len(drives[best].Mount) {
				best = i
			}
		}
	}
	if best < 0 {
		return Drive{}, false
	}
	return drives[best], true
}

// Size is how much a folder holds (following a link at the top).
func Size(ctx context.Context, path string) int64 {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	args := []string{"-sb", path}
	if runtime.GOOS == "darwin" {
		args = []string{"-sk", path}
	}
	out, err := exec.CommandContext(ctx, "du", args...).Output()
	if err != nil && len(out) == 0 {
		return 0
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(f[0], 10, 64)
	if runtime.GOOS == "darwin" {
		n *= 1024
	}
	return n
}

// Progress is told how a copy is going: bytes copied so far of about total.
type Progress func(done, total int64)

var rsyncProgress = regexp.MustCompile(`^\s*([\d,]+)\s+(\d+)%`)

// Copy copies src's contents into dst exactly: owners, permissions, links, hard links, extended attributes. final
// also deletes what's in dst but not in src (the last pass, once nothing writes to src). It's safe to run again
// after it was cut off: it carries on.
func Copy(ctx context.Context, src, dst string, final bool, total int64, progress Progress, log io.Writer) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	// -x: stay on the folder's own drive (running containers' filesystems are mounted inside Docker's folder).
	args := []string{"-aHXx", "--numeric-ids", "--info=progress2", "--no-inc-recursive"}
	if runtime.GOOS == "darwin" {
		args = []string{"-aHx"} // macOS's openrsync
	} else {
		args = append(args, "-A")
	}
	if final {
		args = append(args, "--delete")
	}
	args = append(args, strings.TrimSuffix(src, "/")+"/", strings.TrimSuffix(dst, "/")+"/")
	cmd, stop := copyCmd(ctx, args)
	defer stop()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var errText strings.Builder
	cmd.Stderr = &errText
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("couldn't start rsync (is it installed?): %w", err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Split(splitCR)
	last := time.Time{}
	for sc.Scan() {
		if m := rsyncProgress.FindStringSubmatch(sc.Text()); m != nil && progress != nil {
			done, _ := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
			if time.Since(last) > 500*time.Millisecond {
				progress(done, total)
				last = time.Now()
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(errText.String())
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		return fmt.Errorf("copying %s failed: %v %s", src, err, msg)
	}
	if progress != nil {
		progress(total, total)
	}
	return nil
}

// copyCmd runs rsync. Homeport's service may not make setuid files (systemd's RestrictSUIDSGID), which Docker's
// images hold, so on Linux it runs as its own short system job (systemd-run) instead, outside that sandbox. stop
// ends the job if the copy is cut short.
func copyCmd(ctx context.Context, args []string) (*exec.Cmd, func()) {
	if runtime.GOOS != "linux" || os.Getenv("INVOCATION_ID") == "" { // not under systemd (tests, a terminal)
		return exec.CommandContext(ctx, "rsync", args...), func() {}
	}
	unit := fmt.Sprintf("homeport-copy-%d", time.Now().UnixNano())
	cmd := exec.Command("systemd-run", append([]string{"--quiet", "--wait", "--pipe", "--collect", "--unit", unit,
		"--property=Type=exec", "rsync"}, args...)...)
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = exec.Command("systemctl", "stop", unit).Run()
		case <-done:
		}
	}()
	return cmd, func() { close(done) }
}

// splitCR splits rsync's output on carriage returns as well as newlines (its progress line rewrites itself).
func splitCR(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// Same checks that dst holds the same files as src (names, kinds and sizes), after a copy.
func Same(ctx context.Context, src, dst string) error {
	type entry struct {
		mode os.FileMode
		size int64
	}
	list := func(root string) (map[string]entry, error) {
		out := map[string]entry{}
		rootDev := deviceOf(root)
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			if d.IsDir() && path != root && deviceOf(path) != rootDev {
				return filepath.SkipDir // another filesystem mounted inside (as the copy skips it too)
			}
			rel, _ := filepath.Rel(root, path)
			e := entry{mode: fi.Mode().Type()}
			if fi.Mode().IsRegular() {
				e.size = fi.Size()
			}
			out[rel] = e
			return nil
		})
		return out, err
	}
	a, err := list(src)
	if err != nil {
		return fmt.Errorf("couldn't read %s: %w", src, err)
	}
	b, err := list(dst)
	if err != nil {
		return fmt.Errorf("couldn't read the copy: %w", err)
	}
	var diffs []string
	for rel, e := range a {
		if f, ok := b[rel]; !ok {
			diffs = append(diffs, rel+" is missing")
		} else if f != e {
			diffs = append(diffs, rel+" differs")
		}
	}
	for rel := range b {
		if _, ok := a[rel]; !ok {
			diffs = append(diffs, rel+" shouldn't be there")
		}
	}
	if len(diffs) > 0 {
		sort.Strings(diffs)
		if len(diffs) > 5 {
			diffs = append(diffs[:5], fmt.Sprintf("and %d more", len(diffs)-5))
		}
		return fmt.Errorf("the copy differs from the original: %s", strings.Join(diffs, "; "))
	}
	return nil
}

// MARK: Mounts that last

// ErrNotLinux is returned for what only Linux does here.
var ErrNotLinux = errors.New("on a Mac, macOS mounts drives itself")

// mountCmd runs mount or umount where the whole system sees it. Homeport's service has its own view of the mounts
// (systemd's PrivateTmp), so a drive it mounted there would be invisible to Docker and Ollama: nsenter runs the
// command in the system's own view instead.
func mountCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	own, err1 := os.Readlink("/proc/self/ns/mnt")
	system, err2 := os.Readlink("/proc/1/ns/mnt")
	if err1 == nil && err2 == nil && own != system {
		return exec.CommandContext(ctx, "nsenter", append([]string{"--mount=/proc/1/ns/mnt", "--", name}, args...)...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// fstabMark marks the lines Homeport adds to /etc/fstab.
const fstabMark = "# Homeport:"

// KeepMounted adds a drive to /etc/fstab (by its UUID, with nofail so the machine still starts without it), mounting
// it at mount (made under /mnt when it isn't mounted yet), and mounts it.
func KeepMounted(ctx context.Context, d Drive) (string, error) {
	if runtime.GOOS == "darwin" {
		return "", ErrNotLinux
	}
	if d.UUID == "" {
		return "", errors.New("this drive has no UUID to find it by: format it first")
	}
	if !linuxFS[d.FS] {
		return "", fmt.Errorf("it's formatted as %s; Homeport only keeps Linux filesystems (ext4, xfs…) mounted", d.FS)
	}
	mount := d.Mount
	if mount == "" {
		name := d.Label
		if name == "" {
			name = filepath.Base(d.Device)
		}
		name = regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(name, "-")
		mount = "/mnt/" + name
		if err := os.MkdirAll(mount, 0o755); err != nil {
			return "", err
		}
	}
	if !fstabTargets()[mount] {
		line := fmt.Sprintf("%s %s\nUUID=%s %s %s defaults,nofail 0 2\n", fstabMark, "keeps this drive mounted", d.UUID, mount, d.FS)
		if err := appendFile("/etc/fstab", line); err != nil {
			return "", err
		}
	}
	_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
	if !d.Mounted {
		if out, err := mountCmd(ctx, "mount", mount).CombinedOutput(); err != nil {
			return "", fmt.Errorf("couldn't mount it: %v %s", err, strings.TrimSpace(string(out)))
		}
	}
	return mount, nil
}

// BindMounted says where a folder really is when Homeport bind-mounted it from another drive ("" when it isn't).
func BindMounted(target string) string {
	b, err := os.ReadFile("/etc/fstab")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[1] == target && strings.Contains(f[3], "bind") {
			return f[0]
		}
	}
	return ""
}

// SetBind makes target show src's contents (a bind mount kept in /etc/fstab, mounted at startup once the drive
// is), or, with src "", undoes that. The folder is unmounted or mounted now too.
func SetBind(ctx context.Context, target, src string) error {
	if runtime.GOOS == "darwin" {
		return ErrNotLinux
	}
	if old := BindMounted(target); old != "" {
		if out, err := mountCmd(ctx, "umount", target).CombinedOutput(); err != nil && !strings.Contains(string(out), "not mounted") {
			return fmt.Errorf("couldn't unmount %s: %v %s", target, err, strings.TrimSpace(string(out)))
		}
		if err := editFstab(func(lines []string) []string {
			var out []string
			for i := 0; i < len(lines); i++ {
				f := strings.Fields(lines[i])
				if len(f) >= 4 && f[1] == target && strings.Contains(f[3], "bind") {
					if len(out) > 0 && strings.HasPrefix(out[len(out)-1], fstabMark) {
						out = out[:len(out)-1]
					}
					continue
				}
				out = append(out, lines[i])
			}
			return out
		}); err != nil {
			return err
		}
	}
	if src != "" {
		line := fmt.Sprintf("%s %s is kept on another drive\n%s %s none bind,nofail,x-systemd.requires-mounts-for=%s 0 0\n",
			fstabMark, target, src, target, src)
		if err := appendFile("/etc/fstab", line); err != nil {
			return err
		}
	}
	_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
	if src != "" {
		if out, err := mountCmd(ctx, "mount", target).CombinedOutput(); err != nil {
			return fmt.Errorf("couldn't mount %s: %v %s", target, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

func appendFile(path, text string) error {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		text = "\n" + text
	}
	return writeKeepingMode(path, append(b, text...))
}

func editFstab(f func([]string) []string) error {
	b, err := os.ReadFile("/etc/fstab")
	if err != nil {
		return err
	}
	lines := f(strings.Split(strings.TrimRight(string(b), "\n"), "\n"))
	return writeKeepingMode("/etc/fstab", []byte(strings.Join(lines, "\n")+"\n"))
}

// writeKeepingMode replaces a system file in one step, keeping its permissions (a copy of the old one is kept as
// <file>.homeport-backup).
func writeKeepingMode(path string, b []byte) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
		if old, err := os.ReadFile(path); err == nil {
			_ = os.WriteFile(path+".homeport-backup", old, mode)
		}
	}
	tmp := path + ".homeport-new"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
