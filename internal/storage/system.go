package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Place is something a system service keeps on disk, which can live on another drive: the AI models (Ollama's
// folder) or Docker's images (Docker's and containerd's folders). On another drive, the folder is bind-mounted back
// where the service looks for it, so the service itself isn't changed.
type Place struct {
	Name  string
	Paths []string // where the service looks: /var/lib/docker, /var/lib/containerd
	Subs  []string // the folders on another drive (under <drive>/homeport/)
	Stop  []string // services stopped while it's switched, in this order (started again the other way round)
}

// Where says which folder each of the place's paths really is in now (its own, or one on another drive).
func (p Place) Where() []string {
	out := make([]string, len(p.Paths))
	for i, path := range p.Paths {
		out[i] = path
		if src := BindMounted(path); src != "" {
			out[i] = src
		}
	}
	return out
}

// Missing lists the place's folders that should be on another drive but aren't mounted (the drive is missing).
func (p Place) Missing(ctx context.Context) []string {
	var out []string
	for _, path := range p.Paths {
		if src := BindMounted(path); src != "" {
			if err := exec.CommandContext(ctx, "findmnt", "-n", "--mountpoint", path).Run(); err != nil {
				out = append(out, path+" (from "+src+")")
			}
		}
	}
	return out
}

// Existing leaves out the paths that aren't on this machine (an older Docker has no containerd folder).
func (p Place) Existing() Place {
	var q Place
	q.Name, q.Stop = p.Name, p.Stop
	for i, path := range p.Paths {
		if _, err := os.Stat(path); err == nil {
			q.Paths, q.Subs = append(q.Paths, path), append(q.Subs, p.Subs[i])
		}
	}
	return q
}

type step struct {
	path, cur, dst, old string // cur: where it is now on another drive ("" on the system drive)
}

// MovePlace moves a place to drive (its mount point), or back to the system drive (""). check says whether the
// service works afterwards; if it doesn't, everything goes back as it was.
func MovePlace(ctx context.Context, p Place, drive string, total int64, progress Progress, check func(context.Context) error, log io.Writer) (err error) {
	if runtime.GOOS == "darwin" {
		return ErrNotLinux
	}
	p = p.Existing()
	var steps []step
	var under string
	defer func() {
		if under != "" {
			_ = mountCmd(context.Background(), "umount", "-R", "-l", under).Run()
			_ = os.Remove(under)
		}
	}()
	for i, path := range p.Paths {
		s := step{path: path, cur: BindMounted(path)}
		if drive == "" {
			if s.cur == "" {
				continue
			}
			// The system drive's own folder, under the bind mount: seen through a second, plain mount of its drive.
			if under == "" {
				if under, err = mountUnderneath(ctx, path); err != nil {
					return err
				}
			}
			s.dst = filepath.Join(under, strings.TrimPrefix(path, mountOf(ctx, filepath.Dir(path))))
			if isMountpoint(ctx, s.dst) {
				return fmt.Errorf("%s isn't the system drive's own folder (something is mounted there): not moving", s.dst)
			}
		} else {
			s.dst = filepath.Join(drive, "homeport", p.Subs[i])
			if s.dst == s.cur {
				continue
			}
		}
		steps = append(steps, s)
	}
	if len(steps) == 0 {
		fmt.Fprintf(log, "%s are already there.\n", p.Name)
		return nil
	}

	fmt.Fprintf(log, "Copying %s (still running meanwhile)…\n", p.Name)
	for _, s := range steps {
		if err := Copy(ctx, s.path, s.dst, false, total, progress, log); err != nil {
			return err
		}
	}
	fmt.Fprintf(log, "Stopping %s for a last copy of what changed…\n", strings.Join(p.Stop, ", "))
	if err := services(ctx, "stop", p.Stop); err != nil {
		return err
	}
	start := func() { _ = services(context.Background(), "start", reversed(p.Stop)) }
	for _, s := range steps {
		if err := Copy(ctx, s.path, s.dst, true, total, progress, log); err != nil {
			start()
			return err
		}
		if err := Same(ctx, s.path, s.dst); err != nil {
			start()
			return err
		}
	}

	// Switch.
	done := 0
	putBack := func(cause error) error {
		fmt.Fprintf(log, "That didn't work (%v): %s go back to where they were.\n", cause, p.Name)
		_ = services(context.Background(), "stop", p.Stop)
		_ = requireMounts(context.Background(), p, steps[0].cur != "")
		for _, s := range steps[:done] {
			_ = SetBind(context.Background(), s.path, s.cur)
			if drive != "" && s.cur == "" {
				_ = os.Remove(s.path)
				_ = os.Rename(s.old, s.path)
			}
		}
		start()
		return cause
	}
	for i := range steps {
		s := &steps[i]
		if drive != "" && s.cur == "" {
			s.old = s.path + ".homeport-old"
			_ = os.RemoveAll(s.old)
			if err := os.Rename(s.path, s.old); err != nil {
				return putBack(err)
			}
			done = i + 1
			if err := emptyLike(s.old, s.path); err != nil {
				return putBack(err)
			}
			if err := SetBind(ctx, s.path, s.dst); err != nil {
				return putBack(err)
			}
			continue
		}
		done = i + 1
		target := s.dst
		if drive == "" {
			target = ""
		}
		if err := SetBind(ctx, s.path, target); err != nil {
			return putBack(err)
		}
		// Back on the system drive: the data must be there now, with nothing mounted over it (checked while the
		// services are still stopped, so nothing changes it meanwhile).
		if drive == "" {
			if isMountpoint(ctx, s.path) {
				return putBack(fmt.Errorf("%s is still mounted from elsewhere", s.path))
			}
			if err := Same(ctx, s.cur, s.path); err != nil {
				return putBack(err)
			}
		}
	}
	// On another drive, the services wait for it at startup (rather than start with an empty folder without it).
	if err := requireMounts(ctx, p, drive != ""); err != nil {
		return putBack(err)
	}
	fmt.Fprintf(log, "Starting %s again…\n", strings.Join(reversed(p.Stop), ", "))
	if err := services(ctx, "start", reversed(p.Stop)); err != nil {
		return putBack(err)
	}
	if check != nil {
		if err := check(ctx); err != nil {
			return putBack(err)
		}
	}

	for _, s := range steps {
		gone := s.cur
		if s.old != "" {
			gone = s.old
		}

		if gone != "" {
			fmt.Fprintf(log, "Deleting the old copy at %s…\n", gone)
			_ = os.RemoveAll(gone)
		}
	}
	fmt.Fprintf(log, "Done: %s are on %s now.\n", p.Name, map[bool]string{true: "the system drive", false: drive}[drive == ""])
	return nil
}

// requireMounts makes the place's services start only once its folders are mounted (a systemd drop-in), or undoes
// that.
func requireMounts(ctx context.Context, p Place, on bool) error {
	for _, unit := range p.Stop {
		if strings.HasSuffix(unit, ".socket") {
			continue
		}
		name := strings.TrimSuffix(unit, ".service")
		dir := "/etc/systemd/system/" + name + ".service.d"
		file := filepath.Join(dir, "homeport-storage.conf")
		if !on {
			_ = os.Remove(file)
			_ = os.Remove(dir) // only when nothing else is in it
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		text := "# Homeport: these folders are on another drive; start only once they're there.\n[Unit]\nRequiresMountsFor=" + strings.Join(p.Paths, " ") + "\n"
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			return err
		}
		_ = os.Chmod(file, 0o644)
	}
	return exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
}

func services(ctx context.Context, action string, units []string) error {
	if len(units) == 0 {
		return nil
	}
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{action}, units...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("couldn't %s %s: %v %s", action, strings.Join(units, ", "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func reversed(s []string) []string {
	out := slices.Clone(s)
	slices.Reverse(out)
	return out
}

// emptyLike makes an empty folder with the same owner and permissions as another.
func emptyLike(like, path string) error {
	fi, err := os.Stat(like)
	if err != nil {
		return err
	}
	if err := os.Mkdir(path, fi.Mode().Perm()); err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = os.Chown(path, int(st.Uid), int(st.Gid))
	}
	return os.Chmod(path, fi.Mode().Perm())
}

// mountOf is the mount point of the filesystem a path is on.
func mountOf(ctx context.Context, path string) string {
	out, err := exec.CommandContext(ctx, "findmnt", "-n", "-o", "TARGET", "-T", path).Output()
	if err != nil {
		return "/"
	}
	return strings.TrimSpace(strings.Split(string(out), "\n")[0])
}

// mountUnderneath mounts the filesystem a bind-mounted folder sits on a second time (without the mounts on top), so
// what's underneath can be written to.
func mountUnderneath(ctx context.Context, path string) (string, error) {
	fsRoot := mountOf(ctx, filepath.Dir(path))
	under := "/run/homeport-underneath"
	_ = mountCmd(ctx, "umount", "-R", "-l", under).Run()
	if err := os.MkdirAll(under, 0o700); err != nil {
		return "", err
	}
	// Where the copy (a system job) sees it too, and private: mounts made elsewhere afterwards (the system drive's
	// mounts are shared) mustn't show up in it.
	if out, err := mountCmd(ctx, "mount", "--bind", fsRoot, under).CombinedOutput(); err != nil {
		return "", fmt.Errorf("couldn't reach the system drive's own folder: %v %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := mountCmd(ctx, "mount", "--make-private", under).CombinedOutput(); err != nil {
		_ = mountCmd(ctx, "umount", "-l", under).Run()
		return "", fmt.Errorf("couldn't make the system drive's view private: %v %s", err, strings.TrimSpace(string(out)))
	}
	return under, nil
}

// isMountpoint says whether something is mounted at path (in the system's view).
func isMountpoint(ctx context.Context, path string) bool {
	return mountCmd(ctx, "findmnt", "-n", "--mountpoint", path).Run() == nil
}

// Retry runs f until it works or limit passes.
func Retry(ctx context.Context, limit time.Duration, f func() error) error {
	end := time.Now().Add(limit)
	for {
		err := f()
		if err == nil || time.Now().After(end) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(3 * time.Second):
		}
	}
}
