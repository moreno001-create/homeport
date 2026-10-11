// Package catalog is the list of apps Homeport can install. Each app is a YAML manifest with a Docker Compose
// template; the manifests are built into the binary.
package catalog

import (
	"bytes"
	"embed"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

//go:embed apps/*.yaml
var files embed.FS

// SignIn says how an app works with Homeport's sign-in.
type SignIn string

const (
	// SignInHeaders: the app trusts the Remote-User header Homeport adds, so people are signed in to it automatically.
	SignInHeaders SignIn = "headers"
	// SignInGate: Homeport checks who may open it; the app keeps its own accounts.
	SignInGate SignIn = "gate"
	// SignInOwn: the app has its own accounts and apps (phone clients) talk to it directly, so it's public by default.
	SignInOwn SignIn = "own"
)

type App struct {
	ID          string   `yaml:"id"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Category    string   `yaml:"category"`
	Website     string   `yaml:"website"`
	SignIn      SignIn   `yaml:"signIn"`
	Secrets     []string `yaml:"secrets"`
	// Notes are shown after installing (first sign-in, where the admin password is, and so on).
	Notes   string `yaml:"notes"`
	Compose string `yaml:"compose"`
	// AfterInstall runs inside the app once it's up (after installs and updates). Apps often finish setting
	// themselves up after their first start, so each step is retried for a few minutes until it works.
	AfterInstall []Step `yaml:"afterInstall"`
}

// A Step is a command run in one of the app's containers.
type Step struct {
	Service string   `yaml:"service"`
	User    string   `yaml:"user"`
	Run     []string `yaml:"run"`
	// InstallOnly: run it on the first install, not after updates (for steps that are slow on a big install).
	InstallOnly bool `yaml:"installOnly"`
}

// Vars fill in a manifest's compose template.
type Vars struct {
	ID         string            // instance id
	Domain     string            // where it's served, e.g. cloud.example.com
	Port       int               // the 127.0.0.1 port Caddy proxies to
	Data       string            // the app's data folder on the host
	Secrets    map[string]string // generated once at install, kept for updates
	AdminUser  string            // the admin who installed it
	AdminEmail string
	TimeZone   string
}

// All lists the catalog, sorted by name.
func All() ([]App, error) {
	entries, err := files.ReadDir("apps")
	if err != nil {
		return nil, err
	}
	var out []App
	for _, e := range entries {
		b, err := files.ReadFile("apps/" + e.Name())
		if err != nil {
			return nil, err
		}
		var a App
		if err := yaml.Unmarshal(b, &a); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		if a.ID+".yaml" != e.Name() {
			return nil, fmt.Errorf("%s: id %q doesn't match the file name", e.Name(), a.ID)
		}
		if a.SignIn == "" {
			a.SignIn = SignInGate
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// Get finds one app.
func Get(id string) (App, bool) {
	all, err := All()
	if err != nil {
		return App{}, false
	}
	for _, a := range all {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}

// Render fills in the compose template. Unknown secrets are an error, so a typo can't ship an empty password.
func (a App) Render(v Vars) (string, error) {
	t, err := template.New(a.ID).Option("missingkey=error").Funcs(template.FuncMap{
		"secret": func(name string) (string, error) {
			s, ok := v.Secrets[name]
			if !ok || s == "" {
				return "", fmt.Errorf("secret %q isn't listed in the manifest's secrets", name)
			}
			return s, nil
		},
	}).Parse(a.Compose)
	if err != nil {
		return "", fmt.Errorf("%s: %w", a.ID, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, v); err != nil {
		return "", fmt.Errorf("%s: %w", a.ID, err)
	}
	// Must be valid YAML with at least one service.
	var parsed struct {
		Services map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(buf.Bytes(), &parsed); err != nil || len(parsed.Services) == 0 {
		return "", fmt.Errorf("%s: the compose template doesn't make a valid compose file: %v", a.ID, err)
	}
	return buf.String(), nil
}
