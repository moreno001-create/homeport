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
	"strconv"
	"strings"
	"sync"
	"time"

	"homeport/internal/auth"
	"homeport/internal/catalog"
	"homeport/internal/firewall"
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
	// Drive is the drive its folder goes on (a mount point; "" for the drive chosen for new apps). The server turns it
	// into Place.
	Drive string `json:"drive,omitempty"`
	// Place is the folder its folder goes in: "" for the drive chosen for new apps, "home" for Homeport's own folder.
	Place string `json:"-"`
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
			Secrets: secrets, Installed: time.Now(), OIDC: oidcClient(entry, domain), Unfinished: "install"}
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
		m.removeDir(app.ID)
		_ = m.Store.Update(func(d *state.Data) error { d.DeleteApp(app.ID); return nil })
		return state.App{}, cause
	}

	if err := m.placeNew(app.ID, req.Place); err != nil {
		return undo(err)
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
	m.setUnfinished(app.ID, "")
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

// Description is what an app is for, from its manifest ("" when it has none).
func (m *Manager) Description(app state.App) string {
	if e, ok, _ := m.entryOf(app); ok {
		return e.Description
	}
	return ""
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
	origins := "https://" + app.Domain
	m.Store.Read(func(d *state.Data) {
		st = *d.Settings()
		if p := st.InternetPort; p != 0 && p != 443 && d.UnderHome(app.Domain) {
			origins += ",https://" + app.Domain + ":" + strconv.Itoa(p)
		}
	})
	tz := m.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	home := filepath.Dir(m.Root)
	return catalog.Vars{ID: app.ID, Domain: app.Domain, Port: app.Port, Data: filepath.Join(m.dir(app.ID), "data"),
		Secrets: app.Secrets, AdminUser: installer.Name, AdminEmail: installer.Email, TimeZone: tz,
		MainDomain: st.MainDomain, Issuer: "https://" + st.MainDomain, ClientID: app.ID,
		CABundle: filepath.Join(home, "ca-bundle.crt"), CALocal: filepath.Join(home, "ca-local.crt"),
		InternalTLS: st.TLS == state.TLSInternal, HostIP: m.hostIP(), Origins: origins,
		HomeNetworks: strings.Join(firewall.LocalNetworks(), ",")}
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
	return m.update(ctx, id, log)
}

func (m *Manager) update(ctx context.Context, id string, log io.Writer) error {
	app, installer, err := m.lookup(id)
	if err != nil {
		return err
	}
	if app.Unfinished == "" {
		m.setUnfinished(id, "update")
	}
	defer m.setUnfinished(id, "")
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
	return m.remove(ctx, id, purge, log)
}

func (m *Manager) remove(ctx context.Context, id string, purge bool, log io.Writer) error {
	app, _, err := m.lookup(id)
	if err != nil {
		return err
	}
	m.setUnfinished(id, map[bool]string{false: "remove", true: "remove-purge"}[purge])
	defer m.setUnfinished(id, "") // only matters if it fails before the app is gone
	args := []string{"down", "--remove-orphans"}
	if purge {
		args = append(args, "--volumes")
	}
	if err := m.compose(ctx, id, log, args...); err != nil {
		fmt.Fprintf(log, "Stopping it reported an error (continuing): %v\n", err)
	}
	if err := m.Store.Update(func(d *state.Data) error { d.DeleteApp(id); dropUnused(d, app.Domain); return nil }); err != nil {
		return err
	}
	if m.Apply != nil {
		if err := m.Apply(ctx); err != nil {
			return err
		}
	}
	if purge {
		fmt.Fprintf(log, "Deleting %s\n", m.realDir(id))
		m.removeDir(id)
		return nil
	}
	fmt.Fprintf(log, "Its data is kept in %s\n", m.realDir(id))
	return nil
}

// How long an after-install step may keep failing (the app still starting up) before it's given up on.
var stepPatience, stepRetry = 10 * time.Minute, 5 * time.Second

// afterInstall runs the manifest's steps in the running app. A step that never works is reported, not fatal: the app
// setUnfinished marks the job under way on an app (kept in the state, so a job cut off by Homeport stopping is
// finished when it starts again), or clears it ("").
func (m *Manager) setUnfinished(id, what string) {
	_ = m.Store.Update(func(d *state.Data) error {
		if a, ok := d.App(id); ok {
			a.Unfinished = what
		}
		return nil
	})
}

// Unfinished lists the apps whose last job was cut off.
func (m *Manager) Unfinished() []state.App {
	var out []state.App
	m.Store.Read(func(d *state.Data) {
		for _, a := range d.Apps() {
			if a.Unfinished != "" {
				out = append(out, a)
			}
		}
	})
	return out
}

// Finish completes an app's cut-off job. An install is brought up from its settings with its first-run steps, an
// update or address change is done again as an update, and a removal is done again.
func (m *Manager) Finish(ctx context.Context, id string, log io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	app, installer, err := m.lookup(id)
	if err != nil {
		return err
	}
	switch app.Unfinished {
	case "":
		return nil
	case "move":
		// Cut off while moving drives: the app goes back to where it was if the switch didn't happen, and starts.
		fmt.Fprintf(log, "Starting %s again after a move to another drive was cut off\n", app.Name)
		defer m.setUnfinished(id, "")
		link := m.dir(id)
		if _, err := os.Lstat(link); errors.Is(err, os.ErrNotExist) {
			if err := os.Rename(link+".homeport-old", link); err != nil {
				return fmt.Errorf("%s's folder is missing: %w", app.Name, err)
			}
		}
		return m.compose(ctx, id, log, "up", "--detach")
	case "remove", "remove-purge":
		fmt.Fprintf(log, "Finishing the removal of %s\n", app.Name)
		return m.remove(ctx, id, app.Unfinished == "remove-purge", log)
	case "install":
		fmt.Fprintf(log, "Finishing the install of %s\n", app.Name)
		defer m.setUnfinished(id, "")
		entry, ok, err := m.entryOf(app)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s's catalog entry is gone", app.Name)
		}
		if err := m.writeCompose(entry, app, installer); err != nil {
			return err
		}
		if err := m.compose(ctx, id, log, "pull"); err != nil {
			return fmt.Errorf("downloading the app failed: %w", err)
		}
		if err := m.compose(ctx, id, log, "up", "--detach", "--remove-orphans"); err != nil {
			return fmt.Errorf("starting the app failed: %w", err)
		}
		if m.Apply != nil {
			if err := m.Apply(ctx); err != nil {
				return err
			}
		}
		m.afterInstall(ctx, entry, app, installer, true, log)
		fmt.Fprintf(log, "Done: %s is at https://%s\n", app.Name, app.Domain)
		return nil
	default:
		fmt.Fprintf(log, "Finishing the %s of %s\n", app.Unfinished, app.Name)
		return m.update(ctx, id, log)
	}
}

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

// Container is one of an app's containers, for the Services page.
type Container struct {
	Service  string `json:"service"`
	Name     string `json:"name"`
	Image    string `json:"image"`
	State    string `json:"state"`  // running, exited, restarting, created, paused…
	Status   string `json:"status"` // Docker's words: "Up 2 hours (healthy)"
	Health   string `json:"health,omitempty"`
	ExitCode int    `json:"exitCode"`
	// Setup is a one-time setup container that finished cleanly (it isn't meant to keep running).
	Setup    bool   `json:"setup,omitempty"`
	Restarts int    `json:"restarts"`
	CPU      string `json:"cpu,omitempty"`    // "1.25%"
	Memory   string `json:"memory,omitempty"` // "212MiB / 11.05GiB"
}

// Containers lists an app's containers (without CPU and memory: see Usage).
func (m *Manager) Containers(ctx context.Context, id string) ([]Container, error) {
	out, err := m.ComposeOutput(ctx, id, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("couldn't list %s's containers: %v", id, err)
	}
	var list []Container
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Newer Compose prints one object per line; older ones a single array.
		var some []Container
		var one Container
		if json.Unmarshal([]byte(line), &some) != nil {
			if json.Unmarshal([]byte(line), &one) != nil {
				continue
			}
			some = []Container{one}
		}
		for _, c := range some {
			c.Setup = c.State == "exited" && c.ExitCode == 0
			list = append(list, c)
		}
	}
	slices.SortFunc(list, func(a, b Container) int { return strings.Compare(a.Service, b.Service) })
	return list, nil
}

// Usage is every running container's CPU and memory use, and every container's restart count, by container name.
type Usage struct {
	CPU, Memory string
	Restarts    int
}

func (m *Manager) Usage(ctx context.Context, names []string) map[string]Usage {
	use := map[string]Usage{}
	if len(names) == 0 {
		return use
	}
	if out, err := m.Runner.Output(ctx, "", "docker", append([]string{"stats", "--no-stream", "--format", "{{json .}}"}, names...)...); err == nil || out != "" {
		for _, line := range strings.Split(out, "\n") {
			var s struct{ Name, CPUPerc, MemUsage string }
			if json.Unmarshal([]byte(line), &s) == nil && s.Name != "" {
				use[s.Name] = Usage{CPU: s.CPUPerc, Memory: s.MemUsage}
			}
		}
	}
	if out, err := m.Runner.Output(ctx, "", "docker", append([]string{"inspect", "--format", "{{.Name}} {{.RestartCount}}"}, names...)...); err == nil || out != "" {
		for _, line := range strings.Split(out, "\n") {
			name, n, ok := strings.Cut(strings.TrimPrefix(strings.TrimSpace(line), "/"), " ")
			if !ok {
				continue
			}
			u := use[name]
			u.Restarts, _ = strconv.Atoi(n)
			use[name] = u
		}
	}
	return use
}

// ChangeAddress moves an app to another address: its compose file is written again (apps that know their own address
// are told the new one), the app is started again with it, Caddy is updated, and the old name leaves the domain list
// when nothing else uses it.
func (m *Manager) ChangeAddress(ctx context.Context, id, domain string, log io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	app, installer, err := m.lookup(id)
	if err != nil {
		return err
	}
	domain = strings.ToLower(strings.TrimSpace(domain))
	if err := auth.ValidDomain(domain); err != nil {
		return err
	}
	if domain == app.Domain {
		fmt.Fprintf(log, "%s is already at %s.\n", app.Name, domain)
		return nil
	}
	m.setUnfinished(id, "address")
	defer m.setUnfinished(id, "")
	entry, known, err := m.entryOf(app)
	if err != nil {
		return err
	}
	old := app.Domain
	err = m.Store.Update(func(d *state.Data) error {
		switch {
		case domain == d.Settings().MainDomain:
			return fmt.Errorf("%s is the main domain (portal and admin panel)", domain)
		case domain == d.AIDomain():
			return fmt.Errorf("%s is the AI page's address", domain)
		case domain == d.SignInName():
			return fmt.Errorf("%s is where people outside sign in", domain)
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
		a, _ := d.App(id)
		a.Domain = domain
		if known {
			a.OIDC = oidcClient(entry, domain)
		}
		app = *a
		dropUnused(d, old)
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "Moving %s from %s to %s…\n", app.Name, old, domain)
	if known {
		if err := m.writeCompose(entry, app, installer); err != nil {
			return err
		}
	}
	if err := m.compose(ctx, id, log, "up", "--detach", "--remove-orphans"); err != nil {
		return fmt.Errorf("starting it at its new address failed: %w", err)
	}
	if m.Apply != nil {
		if err := m.Apply(ctx); err != nil {
			return err
		}
	}
	if known {
		m.afterInstall(ctx, entry, app, installer, false, log)
	}
	fmt.Fprintf(log, "Done: %s is at https://%s\n", app.Name, domain)
	return nil
}

// dropUnused takes a name off the domain list when nothing uses it any more: no app, not the main domain or a name
// others are under, not a free name, no certificate of its own.
func dropUnused(d *state.Data, name string) {
	x, ok := d.Domain(name)
	if !ok || x.DynDNS != nil || x.Cert != "" || name == d.Settings().MainDomain {
		return
	}
	if _, used := d.AppOn(name); used {
		return
	}
	for _, y := range d.Domains() {
		if strings.HasSuffix(y.Name, "."+name) {
			return
		}
	}
	for _, a := range d.Apps() {
		if strings.HasSuffix(a.Domain, "."+name) {
			return
		}
	}
	d.DeleteDomain(name)
}

// AppAction starts, stops or restarts a whole app. Starting brings up whatever is missing (it's docker compose up), so
// it also works after its containers were removed.
func (m *Manager) AppAction(ctx context.Context, id, action string) error {
	if _, _, err := m.lookup(id); err != nil {
		return err
	}
	var args []string
	switch action {
	case "start":
		args = []string{"up", "--detach"}
	case "stop", "restart":
		args = []string{action}
	default:
		return errors.New("start, stop or restart")
	}
	if out, err := m.ComposeOutput(ctx, id, args...); err != nil {
		return fmt.Errorf("couldn't %s %s: %v %s", action, id, err, strings.TrimSpace(m.redact(id, out)))
	}
	return nil
}

// ContainerAction restarts, stops or starts one of an app's containers (by its service name).
func (m *Manager) ContainerAction(ctx context.Context, id, service, action string) error {
	if action != "restart" && action != "stop" && action != "start" {
		return errors.New("restart, stop or start")
	}
	list, err := m.Containers(ctx, id)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(list, func(c Container) bool { return c.Service == service }) {
		return fmt.Errorf("%s has no service %q", id, service)
	}
	if out, err := m.ComposeOutput(ctx, id, action, service); err != nil {
		return fmt.Errorf("docker compose %s %s: %v %s", action, service, err, strings.TrimSpace(out))
	}
	return nil
}

// Dir is the app's folder (compose.yaml and data/), where it really is (it may be on another drive).
func (m *Manager) Dir(id string) string { return m.realDir(id) }

// realDir is where an app's folder really is: Root/<id> is the folder, or a link to it on another drive.
func (m *Manager) realDir(id string) string {
	if real, err := filepath.EvalSymlinks(m.dir(id)); err == nil {
		return real
	}
	return m.dir(id)
}

// removeDir deletes an app's folder, wherever it is, and the link to it.
func (m *Manager) removeDir(id string) {
	if real := m.realDir(id); real != m.dir(id) {
		_ = os.RemoveAll(real)
	}
	_ = os.RemoveAll(m.dir(id))
}

// placeNew puts a new app's folder in place (a folder on another drive, linked from Root/<id>): "" is the drive
// chosen for new apps, "home" Homeport's own folder.
func (m *Manager) placeNew(id, place string) error {
	base := place
	if base == "" {
		m.Store.Read(func(d *state.Data) { base = d.Settings().NewAppsDrive })
	}
	if base == "" || base == "home" {
		return nil
	}
	if _, err := os.Stat(base); err != nil {
		return fmt.Errorf("the drive chosen for new apps (%s) isn't there: %w", base, err)
	}
	real := filepath.Join(base, id)
	if err := os.MkdirAll(real, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(m.Root, 0o700); err != nil {
		return err
	}
	return os.Symlink(real, m.dir(id))
}

// ComposeOutput runs a docker compose command for the app and returns what it printed.
func (m *Manager) ComposeOutput(ctx context.Context, id string, args ...string) (string, error) {
	full := append([]string{"compose", "-p", project(id), "-f", "compose.yaml"}, args...)
	return m.Runner.Output(ctx, m.dir(id), "docker", full...)
}
