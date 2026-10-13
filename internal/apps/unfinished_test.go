package apps

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"homeport/internal/state"
)

func unfinishedOf(t *testing.T, m *Manager, id string) string {
	t.Helper()
	var what string
	m.Store.Read(func(d *state.Data) {
		if a, ok := d.App(id); ok {
			what = a.Unfinished
		}
	})
	return what
}

// An install cut off when Homeport stopped is finished on the next start: the app is brought up, Caddy told about it,
// and the mark cleared.
func TestFinishACutOffInstall(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	_ = store.Update(func(d *state.Data) error {
		d.Settings().MainDomain = "home.test"
		d.AddUser(state.User{Name: "alice", Admin: true})
		d.AddApp(state.App{ID: "librespeed", Catalog: "librespeed", Name: "LibreSpeed", Domain: "speed.home.test", Port: 18010, Unfinished: "install"})
		d.AddApp(state.App{ID: "gone", Catalog: "librespeed", Name: "Gone", Domain: "gone.home.test", Port: 18011, Unfinished: "remove"})
		d.AddApp(state.App{ID: "fine", Catalog: "librespeed", Name: "Fine", Domain: "fine.home.test", Port: 18012})
		return nil
	})
	r := &slowStart{}
	applied := 0
	m := &Manager{Store: store, Root: t.TempDir(), Runner: r, Apply: func(context.Context) error { applied++; return nil }}

	var ids []string
	for _, a := range m.Unfinished() {
		ids = append(ids, a.ID+":"+a.Unfinished)
	}
	if strings.Join(ids, " ") != "gone:remove librespeed:install" {
		t.Fatalf("unfinished = %v", ids)
	}

	var log bytes.Buffer
	if err := m.Finish(context.Background(), "librespeed", &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	calls := strings.Join(r.calls, "\n")
	if !strings.Contains(calls, "compose.yaml pull") || !strings.Contains(calls, "compose.yaml up --detach") || applied == 0 {
		t.Errorf("the install wasn't brought up (applied %d):\n%s", applied, calls)
	}
	if u := unfinishedOf(t, m, "librespeed"); u != "" {
		t.Errorf("still marked %q", u)
	}

	if err := m.Finish(context.Background(), "gone", &log); err != nil {
		t.Fatal(err)
	}
	m.Store.Read(func(d *state.Data) {
		if _, ok := d.App("gone"); ok {
			t.Error("the cut-off removal wasn't finished")
		}
	})
	if err := m.Finish(context.Background(), "fine", &log); err != nil || len(m.Unfinished()) != 0 {
		t.Errorf("an app with nothing to finish: %v, %v", err, m.Unfinished())
	}
}

// Updates mark the app while they run, and clear the mark when they end (even when they fail).
func TestUpdateMarksTheApp(t *testing.T) {
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	_ = store.Update(func(d *state.Data) error {
		d.Settings().MainDomain = "home.test"
		d.AddApp(state.App{ID: "librespeed", Catalog: "librespeed", Name: "LibreSpeed", Domain: "speed.home.test", Port: 18010})
		return nil
	})
	seen := ""
	m := &Manager{Store: store, Root: t.TempDir()}
	m.Runner = &probe{slowStart: &slowStart{}, on: func(line string) {
		if strings.Contains(line, " up ") {
			seen = unfinishedOf(t, m, "librespeed")
		}
	}}
	if err := m.Update(context.Background(), "librespeed", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if seen != "update" || unfinishedOf(t, m, "librespeed") != "" {
		t.Errorf("during the update: %q, after: %q", seen, unfinishedOf(t, m, "librespeed"))
	}
}

// probe tells about each command as it runs.
type probe struct {
	*slowStart
	on func(line string)
}

func (p *probe) Stream(ctx context.Context, dir string, w io.Writer, name string, args ...string) error {
	p.on(strings.Join(args, " "))
	return p.slowStart.Stream(ctx, dir, w, name, args...)
}
