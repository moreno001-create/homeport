// Package backup backs Homeport up with restic: its settings and each app's folder, encrypted, to any of four kinds of
// destination: an SSH/SFTP server or NAS, S3-compatible storage, another Homeport's Backup space (restic's REST
// server), or a drive plugged into the server. Each app is paused while its files are read, so databases are copied in
// a consistent state, and is back in a moment.
package backup

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"homeport/internal/apps"
	"homeport/internal/auth"
	"homeport/internal/state"
)

type Manager struct {
	Store    *state.Store
	Apps     *apps.Manager
	StateDir string // Homeport's state folder (/var/lib/homeport)
	Dir      string // the backup's own files: restic's cache, the CA bundle, the last log
	// SSHDir holds the SSH key and known hosts for SFTP places (default Dir). Its path can't have spaces: ssh gets
	// them through restic's sftp.args, which is split at spaces (a Mac's Application Support folder has one).
	SSHDir string
	// Run runs restic with extra environment, writing what it prints to out (tests stand in for it).
	Run func(ctx context.Context, env, args []string, out io.Writer) error
	// SystemCA and CaddyRoot are trusted for HTTPS destinations: the usual certificates, and Caddy's own (a Backup
	// space on this home network, behind Caddy's certificates).
	SystemCA, CaddyRoot string

	mu      sync.Mutex
	running string // what's running now ("" when nothing)
}

// Prepare fills in what a first backup needs: the encryption password and sensible defaults.
func (m *Manager) Prepare() error {
	return m.Store.Update(func(d *state.Data) error {
		b := d.Backup()
		if b.Password == "" {
			b.Password = auth.NewSecret(32)
			b.Time = cmp.Or(b.Time, "03:00")
			b.KeepDaily, b.KeepWeekly, b.KeepMonthly = 7, 4, 6
		}
		return nil
	})
}

func (m *Manager) settings() state.Backup {
	var b state.Backup
	m.Store.Read(func(d *state.Data) { b = *d.Backup(); b.Destinations = slices.Clone(b.Destinations) })
	return b
}

func (m *Manager) sshDir() string { return cmp.Or(m.SSHDir, m.Dir) }

func (m *Manager) keyPath() string { return filepath.Join(m.sshDir(), "id_ed25519") }

// PublicKey is the SSH key SFTP destinations need to let in (made the first time).
func (m *Manager) PublicKey(ctx context.Context) (string, error) {
	if _, err := os.Stat(m.keyPath()); err != nil {
		if err := os.MkdirAll(m.sshDir(), 0o700); err != nil {
			return "", err
		}
		host, _ := os.Hostname()
		out, err := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "homeport-backup@"+host, "-f", m.keyPath()).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("couldn't make the SSH key: %v %s", err, out)
		}
	}
	b, err := os.ReadFile(m.keyPath() + ".pub")
	return strings.TrimSpace(string(b)), err
}

// repo is where restic finds a destination's repository, with what it needs in its environment and options.
func (m *Manager) repo(d state.BackupDestination) (repo string, env, opts []string, err error) {
	switch d.Kind {
	case "sftp":
		if d.Host == "" || d.User == "" || d.Path == "" {
			return "", nil, nil, errors.New("an SFTP destination needs a host, a user name and a folder")
		}
		repo = fmt.Sprintf("sftp:%s@%s:%s", d.User, d.Host, d.Path)
		if strings.ContainsAny(m.sshDir(), " \t,\"") {
			return "", nil, nil, fmt.Errorf("the SSH key's folder %q can't have spaces, commas or quotes", m.sshDir())
		}
		sshArgs := fmt.Sprintf("-i %s -p %d -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s",
			m.keyPath(), cmp.Or(d.Port, 22), filepath.Join(m.sshDir(), "known_hosts"))
		opts = []string{"-o", "sftp.args=" + sshArgs}
	case "s3":
		if d.Endpoint == "" || d.Bucket == "" || d.AccessKey == "" || d.SecretKey == "" {
			return "", nil, nil, errors.New("S3 storage needs its address, a bucket and both keys")
		}
		repo = "s3:" + strings.TrimRight(d.Endpoint, "/") + "/" + d.Bucket + "/homeport"
		env = []string{"AWS_ACCESS_KEY_ID=" + d.AccessKey, "AWS_SECRET_ACCESS_KEY=" + d.SecretKey}
		if d.Region != "" {
			env = append(env, "AWS_DEFAULT_REGION="+d.Region)
		}
	case "homeport":
		if !strings.HasPrefix(d.URL, "https://") || d.Username == "" || d.Secret == "" {
			return "", nil, nil, errors.New("another Homeport's Backup space needs its https:// address, a user name and a password")
		}
		// Each user has their own folder there (the server runs with --private-repos).
		repo = "rest:" + strings.TrimRight(d.URL, "/") + "/" + d.Username + "/"
		env = []string{"RESTIC_REST_USERNAME=" + d.Username, "RESTIC_REST_PASSWORD=" + d.Secret}
	case "local":
		if !filepath.IsAbs(d.Path) {
			return "", nil, nil, errors.New("a drive's folder is a full path, like /media/backup/homeport")
		}
		repo = d.Path
	default:
		return "", nil, nil, fmt.Errorf("unknown kind of destination %q", d.Kind)
	}
	return repo, env, opts, nil
}

// restic runs restic against a destination.
func (m *Manager) restic(ctx context.Context, d state.BackupDestination, out io.Writer, args ...string) error {
	repo, env, opts, err := m.repo(d)
	if err != nil {
		return err
	}
	b := m.settings()
	if b.Password == "" {
		return errors.New("backups aren't set up yet")
	}
	env = append(env, "RESTIC_REPOSITORY="+repo, "RESTIC_PASSWORD="+b.Password, "RESTIC_CACHE_DIR="+filepath.Join(m.Dir, "cache"))
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return err
	}
	if d.Kind == "homeport" || d.Kind == "s3" {
		if ca := m.caBundle(); ca != "" {
			opts = append(opts, "--cacert", ca)
		}
	}
	return m.Run(ctx, env, append(opts, args...), out)
}

// caBundle writes the certificates HTTPS destinations are checked against (the system's and Caddy's own), or "".
func (m *Manager) caBundle() string {
	if m.SystemCA == "" {
		return ""
	}
	bundle, err := os.ReadFile(m.SystemCA)
	if err != nil {
		return ""
	}
	if root, err := os.ReadFile(m.CaddyRoot); err == nil {
		bundle = append(append(bundle, '\n'), root...)
	}
	path := filepath.Join(m.Dir, "ca.crt")
	if os.WriteFile(path, bundle, 0o600) != nil {
		return ""
	}
	return path
}

// output is restic's output for a command (errors include what it printed).
func (m *Manager) output(ctx context.Context, d state.BackupDestination, args ...string) ([]byte, error) {
	var buf bytes.Buffer
	err := m.restic(ctx, d, &buf, args...)
	if err != nil {
		return buf.Bytes(), fmt.Errorf("%v: %s", err, lastLines(buf.String(), 4))
	}
	return buf.Bytes(), nil
}

var noRepo = regexp.MustCompile(`(?i)(Is there a repository at the following location|unable to open config file|repository does not exist|config: file does not exist|404 Not Found|bucket does not exist|NoSuchBucket)`)

// Check reaches a destination, and makes the repository there the first time. It says what it did.
func (m *Manager) Check(ctx context.Context, d state.BackupDestination) (string, error) {
	// A first look, with its own short limit: restic retries some answers (a bucket that doesn't exist yet) for
	// minutes, but what it printed in the first seconds already says.
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	out, err := m.output(probe, d, "cat", "config")
	cancel()
	if err == nil {
		return "It's reachable, and backups are already there.", nil
	}
	if !noRepo.Match(out) && !noRepo.MatchString(err.Error()) {
		return "", friendly(d, err)
	}
	if _, err := m.output(ctx, d, "init"); err != nil {
		return "", friendly(d, err)
	}
	return "It's reachable; Homeport started a new, encrypted backup store there.", nil
}

// friendly explains the usual reasons a destination can't be reached.
func friendly(d state.BackupDestination, err error) error {
	s := err.Error()
	switch {
	case strings.Contains(s, "Permission denied (publickey"):
		return fmt.Errorf("%s didn't accept Homeport's SSH key: add it to ~%s/.ssh/authorized_keys there (it's shown on the Backup page)", d.Host, d.User)
	case strings.Contains(s, "wrong password") || strings.Contains(s, "no key found"):
		return errors.New("backups are there, but made with another password: use that password (Backup page → encryption password), or another folder")
	case strings.Contains(s, "401 Unauthorized") || strings.Contains(s, "403 Forbidden"):
		return errors.New("the user name or password wasn't accepted there")
	}
	return err
}

// Running is what a backup or restore is doing now, or "".
func (m *Manager) Running() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

func (m *Manager) start(what string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running != "" {
		return fmt.Errorf("busy: %s", m.running)
	}
	m.running = what
	return nil
}

func (m *Manager) done() {
	m.mu.Lock()
	m.running = ""
	m.mu.Unlock()
}

// BackUp backs everything up to every destination that's on (or only to destination only), logging as it goes.
func (m *Manager) BackUp(ctx context.Context, only string, w io.Writer) error {
	if err := m.Prepare(); err != nil {
		return err
	}
	if err := m.start("backing up"); err != nil {
		return err
	}
	defer m.done()
	b := m.settings()
	var dests []state.BackupDestination
	for _, d := range b.Destinations {
		if (only == "" && !d.Off) || d.ID == only {
			dests = append(dests, d)
		}
	}
	if len(dests) == 0 {
		return errors.New("there's nowhere to back up to yet: add a destination first")
	}
	var failed []string
	for _, d := range dests {
		fmt.Fprintf(w, "\n== %s\n", d.Name)
		added, err := m.backUpTo(ctx, d, b, w)
		if err != nil {
			fmt.Fprintf(w, "Didn't work: %v\n", err)
			failed = append(failed, d.Name)
		}
		_ = m.Store.Update(func(st *state.Data) error {
			for i := range st.Backup().Destinations {
				x := &st.Backup().Destinations[i]
				if x.ID == d.ID {
					x.LastRun, x.LastOK, x.LastAdded, x.LastError = time.Now(), err == nil, added, ""
					if err != nil {
						x.LastError = err.Error()
					}
				}
			}
			return nil
		})
	}
	if len(failed) > 0 {
		return fmt.Errorf("the backup to %s didn't work (see above)", strings.Join(failed, " and "))
	}
	fmt.Fprintln(w, "\nAll done.")
	return nil
}

// backUpTo makes one snapshot of Homeport's settings and one of each app, then lets old ones go.
func (m *Manager) backUpTo(ctx context.Context, d state.BackupDestination, b state.Backup, w io.Writer) (int64, error) {
	msg, err := m.Check(ctx, d)
	if err != nil {
		return 0, err
	}
	fmt.Fprintln(w, msg)
	var total int64
	snap := func(tag string, paths ...string) error {
		out, err := m.output(ctx, d, append([]string{"backup", "--json", "--quiet", "--host", "homeport", "--tag", tag}, paths...)...)
		if err != nil {
			return err
		}
		added := summary(out)
		total += added
		fmt.Fprintf(w, "done, %s new\n", size(added))
		return nil
	}
	// Everything in Homeport's folder but the apps (each its own backup) and the backup's own files.
	fmt.Fprint(w, "Homeport's settings… ")
	if err := snap("homeport", m.StateDir, "--exclude", filepath.Join(m.StateDir, "apps"), "--exclude", m.Dir); err != nil {
		return total, err
	}
	var list []state.App
	m.Store.Read(func(st *state.Data) { list = st.Apps() })
	for _, a := range list {
		if slices.Contains(b.Skip, a.ID) {
			fmt.Fprintf(w, "%s: left out (as set on the Backup page)\n", a.Name)
			continue
		}
		if a.Catalog == "backup-space" {
			// Others' backups: encrypted, theirs to keep, often big (and pausing it would stop a backup going to it).
			fmt.Fprintf(w, "%s: left out (it holds other people's backups)\n", a.Name)
			continue
		}
		fmt.Fprintf(w, "%s… ", a.Name)
		// Paused while its files are read; unpaused whatever happens (pausing can half-work, and a container that
		// wasn't paused just says so).
		if !b.NoPause {
			_, _ = m.Apps.ComposeOutput(ctx, a.ID, "pause")
		}
		err := snap("app:"+a.ID, m.Apps.Dir(a.ID))
		if !b.NoPause {
			if out, uerr := m.Apps.ComposeOutput(context.Background(), a.ID, "unpause"); uerr != nil && !strings.Contains(out, "not paused") {
				log.Printf("Backup: unpausing %s: %v %s", a.ID, uerr, out)
			}
		}
		if err != nil {
			return total, fmt.Errorf("%s: %w", a.Name, err)
		}
	}
	fmt.Fprint(w, "Letting old backups go… ")
	if _, err := m.output(ctx, d, "forget", "--prune", "--quiet", "--group-by", "host,tags",
		"--keep-daily", fmt.Sprint(cmp.Or(b.KeepDaily, 7)), "--keep-weekly", fmt.Sprint(cmp.Or(b.KeepWeekly, 4)),
		"--keep-monthly", fmt.Sprint(cmp.Or(b.KeepMonthly, 6))); err != nil {
		return total, err
	}
	fmt.Fprintln(w, "done")
	return total, nil
}

// summary is how much a backup added, from restic's --json output.
func summary(out []byte) int64 {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var added int64
	for sc.Scan() {
		var s struct {
			MessageType string `json:"message_type"`
			DataAdded   int64  `json:"data_added"`
		}
		if json.Unmarshal(sc.Bytes(), &s) == nil && s.MessageType == "summary" {
			added = s.DataAdded
		}
	}
	return added
}

// Snapshot is one backup of Homeport's settings or of one app.
type Snapshot struct {
	ID    string    `json:"id"`
	Short string    `json:"short"`
	Time  time.Time `json:"time"`
	What  string    `json:"what"` // "homeport", or an app's id
	Size  int64     `json:"size,omitempty"`
	Paths []string  `json:"paths,omitempty"` // the folders it holds, where they were then
}

func (m *Manager) Snapshots(ctx context.Context, destID string) ([]Snapshot, error) {
	d, ok := m.destination(destID)
	if !ok {
		return nil, state.ErrNotFound
	}
	out, err := m.output(ctx, d, "snapshots", "--json")
	if err != nil {
		return nil, friendly(d, err)
	}
	var raw []struct {
		ID      string    `json:"id"`
		ShortID string    `json:"short_id"`
		Time    time.Time `json:"time"`
		Tags    []string  `json:"tags"`
		Paths   []string  `json:"paths"`
		Summary struct {
			TotalBytes int64 `json:"total_bytes_processed"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("couldn't read the list of backups: %w", err)
	}
	list := []Snapshot{}
	for _, r := range raw {
		s := Snapshot{ID: r.ID, Short: r.ShortID, Time: r.Time, Size: r.Summary.TotalBytes, Paths: r.Paths}
		for _, t := range r.Tags {
			if t == "homeport" {
				s.What = "homeport"
			} else if id, ok := strings.CutPrefix(t, "app:"); ok {
				s.What = id
			}
		}
		list = append(list, s)
	}
	slices.SortFunc(list, func(a, b Snapshot) int { return b.Time.Compare(a.Time) })
	return list, nil
}

func (m *Manager) destination(id string) (state.BackupDestination, bool) {
	for _, d := range m.settings().Destinations {
		if d.ID == id {
			return d, true
		}
	}
	return state.BackupDestination{}, false
}

// RestoreApp puts an app's folder back as it was in a backup. The current folder is kept beside it
// (<folder>.before-restore-<time>), so a restore can be undone.
func (m *Manager) RestoreApp(ctx context.Context, destID, snapshotID, appID string, w io.Writer) error {
	d, ok := m.destination(destID)
	if !ok {
		return state.ErrNotFound
	}
	snaps, err := m.Snapshots(ctx, destID)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(snaps, func(s Snapshot) bool { return s.ID == snapshotID && s.What == appID })
	if i < 0 || len(snaps[i].Paths) != 1 {
		return errors.New("that backup isn't one of this app's")
	}
	from := snaps[i].Paths[0] // where the app's folder was when it was backed up (it may have moved drives since)
	if err := m.start("restoring " + appID); err != nil {
		return err
	}
	defer m.done()
	dir := m.Apps.Dir(appID)
	fmt.Fprintf(w, "Stopping %s…\n", appID)
	if out, err := m.Apps.ComposeOutput(ctx, appID, "stop"); err != nil {
		return fmt.Errorf("couldn't stop it: %v %s", err, out)
	}
	aside := dir + ".before-restore-" + time.Now().Format("20060102-150405")
	if err := os.Rename(dir, aside); err != nil {
		return err
	}
	fmt.Fprintf(w, "Its current folder is kept as %s.\nRestoring the backup…\n", aside)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		_ = os.Rename(aside, dir)
		return err
	}
	// The folder's contents go where the app's folder is now.
	if _, err := m.output(ctx, d, "restore", snapshotID+":"+from, "--target", dir); err != nil {
		_ = os.RemoveAll(dir)
		_ = os.Rename(aside, dir)
		_, _ = m.Apps.ComposeOutput(context.Background(), appID, "up", "--detach")
		return fmt.Errorf("the restore didn't work, so the app is back as it was: %w", err)
	}
	fmt.Fprintf(w, "Starting %s…\n", appID)
	if out, err := m.Apps.ComposeOutput(ctx, appID, "up", "--detach"); err != nil {
		return fmt.Errorf("restored, but it didn't start: %v %s", err, out)
	}
	fmt.Fprintln(w, "Done. When it works as it should, the kept folder can be deleted.")
	return nil
}

// Schedule runs the daily backup at its time, once a day, until ctx ends.
func (m *Manager) Schedule(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		b := m.settings()
		now := time.Now()
		today := now.Format("2006-01-02")
		if b.Time == "" || b.LastScheduled == today || now.Format("15:04") < b.Time || len(b.Destinations) == 0 {
			continue
		}
		_ = m.Store.Update(func(d *state.Data) error { d.Backup().LastScheduled = today; return nil })
		var logBuf bytes.Buffer
		if err := m.BackUp(ctx, "", &logBuf); err != nil {
			log.Printf("Daily backup: %v", err)
		}
		_ = os.WriteFile(filepath.Join(m.Dir, "last-backup.log"), logBuf.Bytes(), 0o600)
	}
}

// RunRestic is Run for real: restic itself.
func RunRestic(ctx context.Context, env, args []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "restic", args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d kB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
