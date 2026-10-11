// Package apps installs, updates and removes apps from the catalog. Each installed app is a Docker Compose project in
// its own folder (/var/lib/homeport/apps/<id>), listening only on 127.0.0.1; Caddy is the way in.
package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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

	mu sync.Mutex // one install, update or removal at a time
}

type InstallRequest struct {
	Catalog string       `json:"catalog"`
	ID      string       `json:"id,omitempty"` // defaults to the catalog id (with -2, -3… if taken)
	Domain  string       `json:"domain"`
	Access  *state.Access `json:"access,omitempty"` // defaults by the app's sign-in style
}

func (m *Manager) dir(id string) string { return filepath.Join(m.Root, id) }

func project(id string) string { return "hp-" + id }

func (m *Manager) compose(ctx context.Context, id string, log io.Writer, args ...string) error {
	full := append([]string{"compose", "-p", project(id), "-f", "compose.yaml"}, args...)
	fmt.Fprintf(log, "$ docker %s\n", strings.Join(full, " "))
	return m.Runner.Stream(ctx, m.dir(id), log, "docker", full...)
}

// Install sets up a new app from the catalog and starts it. installer is the admin doing it (some apps make them
// their own admin).
func (m *Manager) Install(ctx context.Context, req InstallRequest, installer state.User, log io.Writer) (state.App, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := catalog.Get(req.Catalog)
	if !ok {
		return state.App{}, fmt.Errorf("there's no %q in the catalog", req.Catalog)
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
		for _, name := range entry.Secrets {
			secrets[name] = auth.NewSecret(32)
		}
		app = state.App{ID: id, Catalog: entry.ID, Name: entry.Name, Domain: domain, Port: port, Access: access,
			Secrets: secrets, Installed: time.Now()}
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
	fmt.Fprintf(log, "Done: %s is at https://%s\n", entry.Name, app.Domain)
	if entry.Notes != "" {
		fmt.Fprintf(log, "\n%s\n", strings.TrimSpace(entry.Notes))
	}
	return app, nil
}

func (m *Manager) writeCompose(entry catalog.App, app state.App, installer state.User) error {
	dir := m.dir(app.ID)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o750); err != nil {
		return err
	}
	tz := m.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	text, err := entry.Render(catalog.Vars{ID: app.ID, Domain: app.Domain, Port: app.Port, Data: filepath.Join(dir, "data"),
		Secrets: app.Secrets, AdminUser: installer.Name, AdminEmail: installer.Email, TimeZone: tz})
	if err != nil {
		return err
	}
	header := fmt.Sprintf("# %s, installed by Homeport from the catalog entry %q. Homeport rewrites this file on updates.\n", app.Name, entry.ID)
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
	if entry, ok := catalog.Get(app.Catalog); ok {
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
		// Newer Compose prints one object per line; older ones a single array.
		var many []struct{ State string }
		var one struct{ State string }
		if json.Unmarshal([]byte(line), &many) == nil {
			for _, c := range many {
				total++
				if c.State == "running" {
					running++
				}
			}
		} else if json.Unmarshal([]byte(line), &one) == nil {
			total++
			if one.State == "running" {
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
