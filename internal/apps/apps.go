// Package apps installs, updates and removes apps from the catalog. Each installed app is a Docker Compose project in
// its own folder (/var/lib/homeport/apps/<id>), listening only on 127.0.0.1; Caddy is the way in.
package apps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"homeport/internal/auth"
	"homeport/internal/catalog"
	"homeport/internal/runner"
	"homeport/internal/state"
)

// Ports apps are given on 127.0.0.1.
const firstPort, lastPort = 18000, 18999

var idRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,40}$`)

type Manager struct {
	Store    *state.Store
	Root     string // folder holding one subfolder per app
	Runner   runner.Runner
	TimeZone string
	// Apply rewrites the proxy configuration after a change.
	Apply func(ctx context.Context) error
	// Where the system's trusted certificates and Caddy's own root (test servers) are; defaults for Ubuntu.
	SystemCA, CaddyRoot string
	// LocalAddrs are this machine's network addresses (the first is HostIP in manifests).
	LocalAddrs func() []string

	mu sync.Mutex // one install, update or removal at a time
}

type InstallRequest struct {
	Catalog string `json:"catalog"`
	// Manifest installs an app from outside the catalog instead (YAML in the catalog's format), from Source. It's
	// checked first: anything that reaches outside the app's own folder and port is refused.
	Manifest string        `json:"manifest,omitempty"`
	Source   *state.Source `json:"source,omitempty"`
	ID       string        `json:"id,omitempty"` // defaults to the catalog id (with -2, -3… if taken)
	Domain   string        `json:"domain"`
	Access   *state.Access `json:"access,omitempty"` // defaults by the app's sign-in style
}

func (m *Manager) dir(id string) string { return filepath.Join(m.Root, id) }

func project(id string) string { return "hp-" + id }

func (m *Manager) compose(ctx context.Context, id string, log io.Writer, args ...string) error {
	full := append([]string{"compose", "-p", project(id), "-f", "compose.yaml"}, args...)
	fmt.Fprintf(log, "$ docker %s\n", m.redact(id, strings.Join(full, " ")))
	return m.Runner.Stream(ctx, m.dir(id), log, "docker", full...)
}

// redact hides the app's secrets in text that goes into logs (commands can carry them, like an OIDC client secret).
func (m *Manager) redact(id, text string) string {
	if m.Store == nil {
		return text
	}
	m.Store.Read(func(d *state.Data) {
		if a, ok := d.App(id); ok {
			for _, v := range a.Secrets {
				if len(v) >= 8 {
					text = strings.ReplaceAll(text, v, "••••••")
				}
			}
		}
	})
	return text
}

// Install sets up a new app from the catalog and starts it. installer is the admin doing it (some apps make them
// their own admin).
func (m *Manager) Install(ctx context.Context, req InstallRequest, installer state.User, log io.Writer) (state.App, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var entry catalog.App
	if req.Manifest != "" {
		var err error
		if entry, err = Outside(req.Manifest); err != nil {
			return state.App{}, err
		}
		if req.Source == nil || req.Source.URL == "" {
			return state.App{}, errors.New("an app from outside the catalog needs to say where it came from")
		}
	} else {
		var ok bool
		if entry, ok = catalog.Get(req.Catalog); !ok {
			return state.App{}, fmt.Errorf("there's no %q in the catalog", req.Catalog)
		}
	}
	domain := strings.ToLower(strings.TrimSpace(req.Domain))
	if err := auth.ValidDomain(domain); err != nil {
		return state.App{}, err
	}
	access := defaultAccess(entry)
	if req.Access != nil {
		access = *req.Access
	}
	if err := ValidAccess(access); err != nil {
		return state.App{}, err
	}

	// Reserve the id, domain and port in the state first, so two installs can't take the same ones.
	var app state.App
	err := m.Store.Update(func(d *state.Data) error {
		s := d.Settings()
		if domain == s.MainDomain {
			return fmt.Errorf("%s is the main domain (portal and admin panel); give the app its own domain, like %s.%s", domain, entry.ID, domain)
		}
		if other, taken := d.AppOn(domain); taken {
			return fmt.Errorf("%s already serves %s", domain, other.Name)
		}
		if !d.HasDomain(domain) {
			if _, ok := parentDomain(d, domain); !ok {
				return fmt.Errorf("add %s (or the domain it belongs to) under Domains first", domain)
			}
			d.AddDomain(state.Domain{Name: domain, Added: time.Now()})
		}
		id := req.ID
		if id == "" {
			id = entry.ID
			for n := 2; ; n++ {
				if _, taken := d.App(id); !taken {
					break
				}
				id = fmt.Sprintf("%s-%d", entry.ID, n)
			}
		}
		if !idRe.MatchString(id) {
			return fmt.Errorf("app ids are 2–41 lowercase letters, digits or dashes, starting with a letter")
		}
		if _, taken := d.App(id); taken {
			return fmt.Errorf("an app called %s is already installed", id)
		}
		port := 0
		used := d.UsedPorts()
		for p := firstPort; p <= lastPort; p++ {
			if !used[p] {
				port = p
				break
			}
		}
		if port == 0 {
			return errors.New("no free ports left for apps")
		}
		secrets := map[string]string{}
		for _, name := range entry.AllSecrets() {
			secrets[name] = auth.NewSecret(32)
		}
		app = state.App{ID: id, Catalog: entry.ID, Name: entry.Name, Domain: domain, Port: port, Access: access,
			Secrets: secrets, Installed: time.Now(), OIDC: oidcClient(entry, domain)}
		if req.Manifest != "" {
			src := *req.Source
			app.Catalog, app.Source = "", &src
		}
		d.AddApp(app)
		return nil
	})
	if err != nil {
		return state.App{}, err
	}
	fmt.Fprintf(log, "Installing %s as %s on https://%s\n", entry.Name, app.ID, app.Domain)

	undo := func(cause error) (state.App, error) {
		fmt.Fprintf(log, "Install failed: %v\nCleaning up…\n", cause)
		_ = m.compose(context.Background(), app.ID, log, "down", "--volumes", "--remove-orphans")
		_ = os.RemoveAll(m.dir(app.ID))
		_ = m.Store.Update(func(d *state.Data) error { d.DeleteApp(app.ID); return nil })
		return state.App{}, cause
	}

	if err := m.writeCompose(entry, app, installer); err != nil {
		return undo(err)
	}
	if req.Manifest != "" {
		fmt.Fprintf(log, "From %s (commit %.12s). It isn't from Homeport's catalog: it was checked, not reviewed.\n", app.Source.URL, app.Source.Commit)
		if err := os.WriteFile(filepath.Join(m.dir(app.ID), manifestFile), []byte(req.Manifest), 0o600); err != nil {
			return undo(err)
		}
	}
	if err := m.compose(ctx, app.ID, log, "pull"); err != nil {
		return undo(fmt.Errorf("downloading the app failed: %w", err))
	}
	if err := m.compose(ctx, app.ID, log, "up", "--detach", "--remove-orphans"); err != nil {
		return undo(fmt.Errorf("starting the app failed: %w", err))
	}
	if m.Apply != nil {
		if err := m.Apply(ctx); err != nil {
			return undo(err)
		}
	}
	m.afterInstall(ctx, entry, app, installer, true, log)
	fmt.Fprintf(log, "Done: %s is at https://%s\n", entry.Name, app.Domain)
	if entry.Notes != "" {
		fmt.Fprintf(log, "\n%s\n", entry.RenderNotes(m.notesVars(app, installer.Name)))
	}
	fmt.Fprintf(log, "\nThese notes (and the passwords Homeport made for it) stay under Apps → %s → Notes.\n", entry.Name)
	return app, nil
}

// notesVars are the values an app's notes can use.
func (m *Manager) notesVars(app state.App, admin string) catalog.Vars {
	var main string
	m.Store.Read(func(d *state.Data) { main = d.Settings().MainDomain })
	return catalog.Vars{ID: app.ID, Domain: app.Domain, Port: app.Port, Data: filepath.Join(m.dir(app.ID), "data"),
		AdminUser: admin, MainDomain: main, Issuer: "https://" + main, ClientID: app.ID}
}

// Notes is what the admin panel shows for an installed app: its manifest's setup notes, filled in.
func (m *Manager) Notes(app state.App) string {
	entry, ok, err := m.entryOf(app)
	if err != nil || !ok || entry.Notes == "" {
		return ""
	}
	return entry.RenderNotes(m.notesVars(app, ""))
}

// manifestFile keeps the manifest of an app from outside the catalog, in its folder, for updates.
const manifestFile = "homeport.yaml"

// Outside reads and checks the manifest of an app from outside the catalog.
func Outside(text string) (catalog.App, error) {
	entry, err := catalog.Parse(text)
	if err != nil {
		return catalog.App{}, err
	}
	if r := entry.Check(); !r.OK() {
		return catalog.App{}, fmt.Errorf("not installed, because it could reach outside its own folder and port:\n- %s", strings.Join(r.Problems, "\n- "))
	}
	return entry, nil
}

// entryOf is the manifest an installed app runs from: its catalog entry, or its own.
func (m *Manager) entryOf(app state.App) (catalog.App, bool, error) {
	if app.Source != nil {
		b, err := os.ReadFile(filepath.Join(m.dir(app.ID), manifestFile))
		if err != nil {
			return catalog.App{}, false, fmt.Errorf("%s's manifest is missing: %w", app.Name, err)
		}
		e, err := Outside(string(b))
		return e, err == nil, err
	}
	e, ok := catalog.Get(app.Catalog)
	return e, ok, nil
}

// oidcClient is the OIDC registration an app gets from its manifest, on its domain.
func oidcClient(entry catalog.App, domain string) *state.OIDCClient {
	if entry.OIDC == nil {
		return nil
	}
	c := &state.OIDCClient{}
	for _, p := range entry.OIDC.RedirectPaths {
		c.RedirectURIs = append(c.RedirectURIs, "https://"+domain+p)
	}
	return c
}

// vars are the values a manifest's templates are filled with.
func (m *Manager) vars(app state.App, installer state.User) catalog.Vars {
	var st state.Settings
	m.Store.Read(func(d *state.Data) { st = *d.Settings() })
	tz := m.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	home := filepath.Dir(m.Root)
	return catalog.Vars{ID: app.ID, Domain: app.Domain, Port: app.Port, Data: filepath.Join(m.dir(app.ID), "data"),
		Secrets: app.Secrets, AdminUser: installer.Name, AdminEmail: installer.Email, TimeZone: tz,
		MainDomain: st.MainDomain, Issuer: "https://" + st.MainDomain, ClientID: app.ID,
		CABundle: filepath.Join(home, "ca-bundle.crt"), CALocal: filepath.Join(home, "ca-local.crt"),
		InternalTLS: st.TLS == state.TLSInternal, HostIP: m.hostIP()}
}

func (m *Manager) hostIP() string {
	if m.LocalAddrs != nil {
		if a := m.LocalAddrs(); len(a) > 0 {
			return a[0]
		}
	}
	return "0.0.0.0"
}

// writeCA puts the certificates apps should trust where they can mount them: the system's, plus Caddy's own root on
// a server using Caddy's certificates (so apps can reach Homeport over HTTPS there too).
func (m *Manager) writeCA(v catalog.Vars) error {
	sys := m.SystemCA
	if sys == "" {
		sys = "/etc/ssl/certs/ca-certificates.crt"
	}
	root := m.CaddyRoot
	if root == "" {
		root = "/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt"
	}
	bundle, _ := os.ReadFile(sys)
	var local []byte
	if v.InternalTLS {
		local, _ = os.ReadFile(root)
	}
	if len(local) > 0 {
		bundle = append(append(bundle, '\n'), local...)
	}
	for path, b := range map[string][]byte{v.CABundle: bundle, v.CALocal: local} {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o644); err != nil { // readable by the apps' own users, despite the service's umask
			return err
		}
	}
	return nil
}

func (m *Manager) writeCompose(entry catalog.App, app state.App, installer state.User) error {
	dir := m.dir(app.ID)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o750); err != nil {
		return err
	}
	v := m.vars(app, installer)
	if err := m.writeCA(v); err != nil {
		return err
	}
	text, err := entry.Render(v)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("# %s, installed by Homeport from the catalog entry %q. Homeport rewrites this file on updates.\n", app.Name, entry.ID)
	if app.Source != nil {
		header = fmt.Sprintf("# %s, installed by Homeport from %s (commit %.12s). Homeport rewrites this file on updates.\n", app.Name, app.Source.URL, app.Source.Commit)
	}
	return os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(header+text), 0o600)
}

// Update downloads the newest images for the app and restarts it with them (its data stays).
func (m *Manager) Update(ctx context.Context, id string, log io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	app, installer, err := m.lookup(id)
	if err != nil {
		return err
	}
	entry, known, err := m.entryOf(app)
	if err != nil {
		return err
	}
	if known {
		// A newer manifest may need secrets this install doesn't have yet (a new Redis password, say), or sign in
		// with Homeport now.
		var changed bool
		for _, name := range entry.AllSecrets() {
			if app.Secrets[name] == "" {
				if app.Secrets == nil {
					app.Secrets = map[string]string{}
				}
				app.Secrets[name], changed = auth.NewSecret(32), true
			}
		}
		if c := oidcClient(entry, app.Domain); (c == nil) != (app.OIDC == nil) || (c != nil && !slices.Equal(c.RedirectURIs, app.OIDC.RedirectURIs)) {
			app.OIDC, changed = c, true
		}
		if changed {
			err := m.Store.Update(func(d *state.Data) error {
				if a, ok := d.App(id); ok {
					a.Secrets, a.OIDC = app.Secrets, app.OIDC
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		// The manifest may have changed with a new Homeport: write the compose file again from it.
		if err := m.writeCompose(entry, app, installer); err != nil {
			return err
		}
	}
	if err := m.compose(ctx, id, log, "pull"); err != nil {
		return fmt.Errorf("downloading the update failed: %w", err)
	}
	if err := m.compose(ctx, id, log, "up", "--detach", "--remove-orphans"); err != nil {
		return fmt.Errorf("restarting the app failed: %w", err)
	}
	if known {
		m.afterInstall(ctx, entry, app, installer, false, log)
	}
	return m.Store.Update(func(d *state.Data) error {
		if a, ok := d.App(id); ok {
			a.Updated = time.Now()
		}
		return nil
	})
}

// Remove stops the app and takes it off its domain. With purge, its data is deleted too; otherwise the folder stays.
func (m *Manager) Remove(ctx context.Context, id string, purge bool, log io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, _, err := m.lookup(id); err != nil {
		return err
	}
	args := []string{"down", "--remove-orphans"}
	if purge {
		args = append(args, "--volumes")
	}
	if err := m.compose(ctx, id, log, args...); err != nil {
		fmt.Fprintf(log, "Stopping it reported an error (continuing): %v\n", err)
	}
	if err := m.Store.Update(func(d *state.Data) error { d.DeleteApp(id); return nil }); err != nil {
		return err
	}
	if m.Apply != nil {
		if err := m.Apply(ctx); err != nil {
			return err
		}
	}
	if purge {
		fmt.Fprintf(log, "Deleting %s\n", m.dir(id))
		return os.RemoveAll(m.dir(id))
	}
	fmt.Fprintf(log, "Its data is kept in %s\n", m.dir(id))
	return nil
}

// How long an after-install step may keep failing (the app still starting up) before it's given up on.
var stepPatience, stepRetry = 10 * time.Minute, 5 * time.Second

// afterInstall runs the manifest's steps in the running app. A step that never works is reported, not fatal: the app
// is installed and running, and the step can be run by hand.
func (m *Manager) afterInstall(ctx context.Context, entry catalog.App, app state.App, installer state.User, firstInstall bool, log io.Writer) {
	id := app.ID
	v := m.vars(app, installer)
	for _, step := range entry.AfterInstall {
		if step.InstallOnly && !firstInstall {
			continue
		}
		run, err := entry.RenderStep(step, v)
		if err != nil {
			fmt.Fprintf(log, "A setup step couldn't be prepared: %v\n", err)
			continue
		}
		args := []string{"exec", "-T"}
		if step.User != "" {
			args = append(args, "--user", step.User)
		}
		args = append(append(args, step.Service), run...)
		deadline := time.Now().Add(stepPatience)
		for attempt := 1; ; attempt++ {
			var out bytes.Buffer
			err := m.compose(ctx, id, &out, args...)
			if err == nil {
				log.Write(out.Bytes())
				break
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				fmt.Fprintf(log, "%sThis setup step didn't work (%v); the app is running, but you may want to run it by hand.\n", out.String(), err)
				break
			}
			if attempt == 1 {
				fmt.Fprintf(log, "Waiting for %s to finish starting up…\n", entry.Name)
			}
			select {
			case <-ctx.Done():
			case <-time.After(stepRetry):
			}
		}
	}
}

// SetAccess changes who may open an app.
func (m *Manager) SetAccess(ctx context.Context, id string, access state.Access) error {
	if err := ValidAccess(access); err != nil {
		return err
	}
	err := m.Store.Update(func(d *state.Data) error {
		a, ok := d.App(id)
		if !ok {
			return state.ErrNotFound
		}
		a.Access = access
		return nil
	})
	if err != nil {
		return err
	}
	if m.Apply != nil {
		return m.Apply(ctx)
	}
	return nil
}

// Status is "running", "stopped", "partly running" or "unknown", from Docker.
func (m *Manager) Status(ctx context.Context, id string) string {
	out, err := m.Runner.Output(ctx, m.dir(id), "docker", "compose", "-p", project(id), "-f", "compose.yaml", "ps", "--all", "--format", "json")
	if err != nil {
		return "unknown"
	}
	running, total := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Newer Compose prints one object per line; older ones a single array. A one-time setup container that finished
		// cleanly (exited 0) doesn't count.
		type ctr struct {
			State    string
			ExitCode int
		}
		var list []ctr
		var one ctr
		if json.Unmarshal([]byte(line), &list) != nil {
			if json.Unmarshal([]byte(line), &one) != nil {
				continue
			}
			list = []ctr{one}
		}
		for _, c := range list {
			if c.State == "exited" && c.ExitCode == 0 {
				continue
			}
			total++
			if c.State == "running" {
				running++
			}
		}
	}
	switch {
	case total == 0:
		return "stopped"
	case running == total:
		return "running"
	case running == 0:
		return "stopped"
	default:
		return "partly running"
	}
}

func (m *Manager) lookup(id string) (state.App, state.User, error) {
	var app state.App
	var installer state.User
	found := false
	m.Store.Read(func(d *state.Data) {
		if a, ok := d.App(id); ok {
			app, found = *a, true
			for _, u := range d.Users() {
				if u.Admin {
					installer = u
					break
				}
			}
		}
	})
	if !found {
		return app, installer, fmt.Errorf("no app called %s is installed", id)
	}
	return app, installer, nil
}

// ValidAccess: public, or at least one valid group.
func ValidAccess(a state.Access) error {
	if a.Public {
		return nil
	}
	if len(a.Groups) == 0 {
		return errors.New("choose who may open the app: public, or at least one group (\"all\" for every user)")
	}
	for _, g := range a.Groups {
		if err := auth.ValidGroup(g); err != nil && g != "all" {
			return err
		}
	}
	return nil
}

// Apps with their own accounts and phone apps stay public (a sign-in page in front would break their clients);
// the rest are for every signed-in user.
func defaultAccess(e catalog.App) state.Access {
	if e.SignIn == catalog.SignInOwn {
		return state.Access{Public: true}
	}
	return state.Access{Groups: []string{"all"}}
}

// parentDomain finds a listed domain that domain is a subdomain of (cloud.example.com → example.com).
func parentDomain(d *state.Data, domain string) (string, bool) {
	for _, x := range d.Domains() {
		if strings.HasSuffix(domain, "."+x.Name) {
			return x.Name, true
		}
	}
	if s := d.Settings().MainDomain; s != "" && strings.HasSuffix(domain, "."+s) {
		return s, true
	}
	return "", false
}

// Dir is the app's folder (compose.yaml and data/).
func (m *Manager) Dir(id string) string { return m.dir(id) }

// ComposeOutput runs a docker compose command for the app and returns what it printed.
func (m *Manager) ComposeOutput(ctx context.Context, id string, args ...string) (string, error) {
	full := append([]string{"compose", "-p", project(id), "-f", "compose.yaml"}, args...)
	return m.Runner.Output(ctx, m.dir(id), "docker", full...)
}
