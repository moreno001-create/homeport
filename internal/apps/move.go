package apps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"homeport/internal/state"
	"homeport/internal/storage"
)

// Move puts an app's folder (its settings, database and files: everything it keeps) on another drive: under base
// (a folder on that drive), or back in Homeport's own folder when base is "". The app keeps running during the long
// first copy; it's stopped only for a last copy of what changed meanwhile, then started from the new place and
// checked. If anything goes wrong it's started again from where it was, which is only deleted once it works.
func (m *Manager) Move(ctx context.Context, id, base string, progress storage.Progress, log io.Writer) error {
	// Other apps' jobs aren't held up by the long first copy: the lock is only held to stop, switch and start.
	m.mu.Lock()
	app, _, err := m.lookup(id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	link, src := m.dir(id), m.realDir(id)
	toHome := base == ""
	dst := filepath.Join(base, id)
	if toHome {
		if src == link {
			fmt.Fprintf(log, "%s is already in Homeport's own folder.\n", app.Name)
			return nil
		}
		dst = link + ".homeport-new"
	} else if dst == src {
		fmt.Fprintf(log, "%s is already there.\n", app.Name)
		return nil
	}
	m.setUnfinished(id, "move")
	defer m.setUnfinished(id, "")
	status := m.Status(ctx, id)
	running := status == "running" || status == "partly running"
	total := storage.Size(ctx, src)

	fmt.Fprintf(log, "Copying %s to %s (it keeps running meanwhile)…\n", app.Name, dst)
	if err := storage.Copy(ctx, src, dst, false, total, progress, log); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if running {
		fmt.Fprintf(log, "Stopping %s for a last copy of what changed…\n", app.Name)
		if err := m.compose(ctx, id, log, "stop"); err != nil {
			return fmt.Errorf("couldn't stop it: %w", err)
		}
	}
	restart := func() {
		if running {
			_ = m.compose(context.Background(), id, log, "up", "--detach")
		}
	}
	if err := storage.Copy(ctx, src, dst, true, total, progress, log); err != nil {
		restart()
		return err
	}
	if err := storage.Same(ctx, src, dst); err != nil {
		restart()
		return err
	}

	// Switch: the old folder (or link) steps aside, and the app's folder is the new one (or a link to it).
	old := link + ".homeport-old"
	_ = os.RemoveAll(old)
	if err := os.Rename(link, old); err != nil {
		restart()
		return err
	}
	if toHome {
		err = os.Rename(dst, link)
	} else {
		err = os.Symlink(dst, link)
	}
	putBack := func(cause error) error {
		fmt.Fprintf(log, "That didn't work (%v): %s goes back to where it was.\n", cause, app.Name)
		if running {
			_ = m.compose(context.Background(), id, log, "stop")
		}
		if toHome {
			_ = os.Rename(link, dst)
		} else {
			_ = os.Remove(link)
		}
		_ = os.Rename(old, link)
		if running {
			_ = m.compose(context.Background(), id, log, "up", "--detach", "--force-recreate")
		}
		return cause
	}
	if err != nil {
		return putBack(err)
	}
	if running {
		fmt.Fprintf(log, "Starting %s from its new place…\n", app.Name)
		// New containers, so their folders are found at the new place.
		if err := m.compose(ctx, id, log, "up", "--detach", "--force-recreate"); err != nil {
			return putBack(err)
		}
		if err := m.waitRunning(ctx, id, 2*time.Minute); err != nil {
			return putBack(err)
		}
	}

	// It works: the old copy goes.
	fmt.Fprintf(log, "Deleting the old copy at %s…\n", src)
	if fi, err := os.Lstat(old); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(old)
		_ = os.RemoveAll(src)
	} else {
		_ = os.RemoveAll(old)
	}
	fmt.Fprintf(log, "Done: %s is at %s now.\n", app.Name, m.realDir(id))
	return nil
}

// waitRunning waits until all of an app's containers run.
func (m *Manager) waitRunning(ctx context.Context, id string, limit time.Duration) error {
	end := time.Now().Add(limit)
	for {
		st := m.Status(ctx, id)
		if st == "running" {
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("it didn't start from the new place (it's %s)", st)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// Missing lists apps whose folder is a link to a drive that isn't there (unplugged, or not mounted).
func (m *Manager) Missing() []string {
	var out []string
	var list []state.App
	m.Store.Read(func(d *state.Data) { list = d.Apps() })
	for _, a := range list {
		link := m.dir(a.ID)
		if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if _, err := os.Stat(link); errors.Is(err, os.ErrNotExist) {
				target, _ := os.Readlink(link)
				out = append(out, a.Name+" ("+strings.TrimSuffix(target, "/"+a.ID)+")")
			}
		}
	}
	return out
}
