package backup

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"homeport/internal/apps"
	"homeport/internal/state"
)

// composeRecorder stands in for docker compose (pause, unpause).
type composeRecorder struct{ calls []string }

func (c *composeRecorder) Output(_ context.Context, _, name string, args ...string) (string, error) {
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	return "", nil
}
func (c *composeRecorder) Stream(context.Context, string, io.Writer, string, ...string) error {
	return nil
}

type resticCall struct {
	env, args []string
}

func setup(t *testing.T, initialised bool) (*Manager, *[]resticCall, *composeRecorder) {
	dir := t.TempDir()
	store, _ := state.Open(filepath.Join(dir, "state.json"))
	_ = store.Update(func(d *state.Data) error {
		d.AddApp(state.App{ID: "wiki", Name: "Wiki"})
		d.AddApp(state.App{ID: "media", Name: "Media"})
		d.AddApp(state.App{ID: "space", Name: "Backup space", Catalog: "backup-space"})
		b := d.Backup()
		b.Skip = []string{"media"}
		b.Destinations = []state.BackupDestination{
			{ID: "nas", Kind: "sftp", Name: "NAS", Host: "nas.lan", User: "backup", Path: "/volume1/homeport", Port: 2222},
			{ID: "b2", Kind: "s3", Name: "B2", Endpoint: "https://s3.example.com/", Bucket: "mine", AccessKey: "AK", SecretKey: "SK", Off: true},
		}
		return nil
	})
	rec := &composeRecorder{}
	var calls []resticCall
	m := &Manager{Store: store, Apps: &apps.Manager{Store: store, Root: filepath.Join(dir, "apps"), Runner: rec}, StateDir: dir, Dir: filepath.Join(dir, "backup"),
		Run: func(_ context.Context, env, args []string, out io.Writer) error {
			calls = append(calls, resticCall{env, args})
			switch {
			case slices.Contains(args, "cat") && !initialised:
				io.WriteString(out, "Fatal: unable to open config file: Stat: file does not exist\nIs there a repository at the following location?")
				initialised = true
				return io.EOF
			case slices.Contains(args, "backup"):
				io.WriteString(out, `{"message_type":"summary","data_added":2048}`+"\n")
			}
			return nil
		}}
	return m, &calls, rec
}

func TestBackUp(t *testing.T) {
	m, calls, rec := setup(t, false)
	var log strings.Builder
	if err := m.BackUp(context.Background(), "", &log); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	var verbs []string
	for _, c := range *calls {
		i := slices.IndexFunc(c.args, func(a string) bool { return !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "sftp.args=") })
		verbs = append(verbs, c.args[i])
	}
	// Only the NAS (the B2 one is off): config missing → init, settings, the wiki (media is left out), then old ones go.
	if strings.Join(verbs, " ") != "cat init backup backup forget" {
		t.Fatalf("restic calls: %v", verbs)
	}
	first := (*calls)[0]
	if !slices.Contains(first.env, "RESTIC_REPOSITORY=sftp:backup@nas.lan:/volume1/homeport") || !strings.Contains(strings.Join(first.args, " "), "-p 2222") || !strings.HasPrefix(first.args[1], "sftp.args=-i /") {
		t.Fatalf("sftp: %v %v", first.env, first.args)
	}
	if !slices.ContainsFunc(first.env, func(e string) bool { return strings.HasPrefix(e, "RESTIC_PASSWORD=") && len(e) > 30 }) {
		t.Fatal("a password should have been made")
	}
	wiki := (*calls)[3].args
	if !slices.Contains(wiki, "app:wiki") || !slices.Contains(wiki, m.Apps.Dir("wiki")) {
		t.Fatalf("the wiki's backup: %v", wiki)
	}
	settings := (*calls)[2].args
	if !slices.Contains(settings, "homeport") || !slices.Contains(settings, filepath.Join(m.StateDir, "apps")) {
		t.Fatalf("the settings' backup leaves the apps out: %v", settings)
	}
	if strings.Join(rec.calls, "|") != "docker compose -p hp-wiki -f compose.yaml pause|docker compose -p hp-wiki -f compose.yaml unpause" {
		t.Fatalf("the wiki is paused while it's read: %v", rec.calls)
	}
	var d state.BackupDestination
	m.Store.Read(func(st *state.Data) { d = st.Backup().Destinations[0] })
	if !d.LastOK || d.LastAdded != 4096 || d.LastRun.IsZero() {
		t.Fatalf("the result is kept: %+v", d)
	}
}

func TestDestinations(t *testing.T) {
	m := &Manager{Dir: "/var/lib/homeport/backup"}
	repo, env, _, err := m.repo(state.BackupDestination{Kind: "s3", Endpoint: "https://s3.eu-central-003.backblazeb2.com/", Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err != nil || repo != "s3:https://s3.eu-central-003.backblazeb2.com/b/homeport" || !slices.Contains(env, "AWS_SECRET_ACCESS_KEY=s") {
		t.Fatalf("s3: %s %v %v", repo, env, err)
	}
	repo, env, _, err = m.repo(state.BackupDestination{Kind: "homeport", URL: "https://backup.friend.duckdns.org/", Username: "me", Secret: "pw"})
	if err != nil || repo != "rest:https://backup.friend.duckdns.org/me/" || !slices.Contains(env, "RESTIC_REST_PASSWORD=pw") {
		t.Fatalf("homeport: %s %v %v", repo, env, err)
	}
	if _, _, _, err := m.repo(state.BackupDestination{Kind: "homeport", URL: "http://plain", Username: "me", Secret: "pw"}); err == nil {
		t.Error("only https for another Homeport")
	}
	if _, _, _, err := m.repo(state.BackupDestination{Kind: "local", Path: "relative/path"}); err == nil {
		t.Error("a drive needs a full path")
	}
}

func TestRestoreOnlyThatAppsBackups(t *testing.T) {
	m, calls, _ := setup(t, true)
	m.Run = func(_ context.Context, env, args []string, out io.Writer) error {
		*calls = append(*calls, resticCall{env, args})
		if slices.Contains(args, "snapshots") {
			io.WriteString(out, `[{"id":"aaa","short_id":"a","time":"2026-10-12T03:00:00Z","tags":["app:wiki"]},{"id":"bbb","short_id":"b","time":"2026-10-12T03:00:00Z","tags":["homeport"]}]`)
		}
		return nil
	}
	_ = m.Prepare()
	if err := m.RestoreApp(context.Background(), "nas", "bbb", "wiki", io.Discard); err == nil || !strings.Contains(err.Error(), "isn't one of this app's") {
		t.Fatalf("the settings' backup isn't the wiki's: %v", err)
	}
}
