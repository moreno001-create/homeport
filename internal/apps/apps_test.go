package apps

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"homeport/internal/catalog"
	"homeport/internal/state"
)

// slowStart fails `exec` a few times, like an app still setting itself up, then works.
type slowStart struct {
	failures int
	calls    []string
}

func (s *slowStart) Output(ctx context.Context, dir, name string, args ...string) (string, error) { return "", nil }

func (s *slowStart) Stream(ctx context.Context, dir string, w io.Writer, name string, args ...string) error {
	line := strings.Join(args, " ")
	s.calls = append(s.calls, line)
	if strings.Contains(line, " exec ") && s.failures > 0 {
		s.failures--
		io.WriteString(w, "not installed yet\n")
		return io.ErrUnexpectedEOF
	}
	if strings.Contains(line, " exec ") {
		io.WriteString(w, "ok\n")
	}
	return nil
}

func TestAfterInstallWaitsForTheApp(t *testing.T) {
	stepRetry = time.Millisecond
	r := &slowStart{failures: 3}
	m := &Manager{Root: t.TempDir(), Runner: r}
	entry := catalog.App{ID: "x", Name: "X", AfterInstall: []catalog.Step{{Service: "app", User: "www-data", Run: []string{"php", "occ", "background:cron"}}}}
	var log bytes.Buffer
	m.afterInstall(context.Background(), entry, "x", true, &log)
	if !strings.Contains(log.String(), "ok") || strings.Contains(log.String(), "didn't work") {
		t.Fatalf("the step should succeed after the app finishes starting:\n%s", log.String())
	}
	if !strings.Contains(r.calls[len(r.calls)-1], "exec -T --user www-data app php occ background:cron") {
		t.Fatalf("unexpected command: %s", r.calls[len(r.calls)-1])
	}
}

func TestUpdateAddsSecretsANewerManifestNeeds(t *testing.T) {
	dir := t.TempDir()
	store, _ := state.Open(filepath.Join(dir, "state.json"))
	_ = store.Update(func(d *state.Data) error {
		d.AddUser(state.User{Name: "alice", Admin: true})
		// Installed before the manifest asked for a Redis password.
		d.AddApp(state.App{ID: "nextcloud", Catalog: "nextcloud", Name: "Nextcloud", Domain: "cloud.example.test", Port: 18000,
			Secrets: map[string]string{"db_password": "a", "admin_password": "b"}})
		return nil
	})
	stepRetry = time.Millisecond
	m := &Manager{Store: store, Root: filepath.Join(dir, "apps"), Runner: &slowStart{}}
	if err := m.Update(context.Background(), "nextcloud", io.Discard); err != nil {
		t.Fatal(err)
	}
	store.Read(func(d *state.Data) {
		a, _ := d.App("nextcloud")
		if len(a.Secrets["redis_password"]) < 20 || a.Secrets["db_password"] != "a" {
			t.Fatalf("want a new redis password and the old ones kept: %v", a.Secrets)
		}
	})
}
