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
	// OIDC: the app signs people in with Homeport over OpenID Connect. Installing registers it as a client
	// (client id = its id, secret "oidc_client_secret" made for it).
	OIDC *OIDC `yaml:"oidc"`
}

type OIDC struct {
	// RedirectPaths are where the app receives the sign-in on its own domain, e.g. /apps/user_oidc/code.
	RedirectPaths []string `yaml:"redirectPaths"`
}

// AllSecrets is the manifest's secrets plus the OIDC client secret if it uses OIDC.
func (a App) AllSecrets() []string {
	if a.OIDC != nil {
		return append(append([]string(nil), a.Secrets...), "oidc_client_secret")
	}
	return a.Secrets
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
	MainDomain string // where the portal and the OIDC provider are
	Issuer     string // https://<main domain>
	ClientID   string // the app's OIDC client id
	// CABundle is the certificates apps should trust (the system's, plus Caddy's own on test servers), to mount at
	// /etc/ssl/certs/ca-certificates.crt. CALocal is Caddy's own root alone (empty with Let's Encrypt).
	CABundle    string
	CALocal     string
	InternalTLS bool
	// Origins are the addresses browsers open it at, comma-separated: https://<domain>, and with the internet port too
	// when that isn't 443 (https://<domain>:8443), for apps that check where requests come from (CSRF).
	Origins string
	// HostIP is this machine's address on the local network, for apps that serve the network directly (DNS on port
	// 53, say) rather than through Caddy.
	HostIP string
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

func (a App) fill(text string, v Vars) (string, error) {
	t, err := template.New(a.ID).Option("missingkey=error").Funcs(template.FuncMap{
		"secret": func(name string) (string, error) {
			s, ok := v.Secrets[name]
			if !ok || s == "" {
				return "", fmt.Errorf("secret %q isn't listed in the manifest's secrets", name)
			}
			return s, nil
		},
	}).Parse(text)
	if err != nil {
		return "", fmt.Errorf("%s: %w", a.ID, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, v); err != nil {
		return "", fmt.Errorf("%s: %w", a.ID, err)
	}
	return buf.String(), nil
}

// RenderStep fills in an after-install step's command.
func (a App) RenderStep(s Step, v Vars) ([]string, error) {
	out := make([]string, len(s.Run))
	for i, arg := range s.Run {
		x, err := a.fill(arg, v)
		if err != nil {
			return nil, err
		}
		out[i] = x
	}
	return out, nil
}

// RenderNotes fills in the notes' template values ({{.Domain}} and the like; secrets stay out of notes). Notes that
// don't render are shown as they are.
func (a App) RenderNotes(v Vars) string {
	v.Secrets = nil
	text, err := a.fill(a.Notes, v)
	if err != nil {
		return strings.TrimSpace(a.Notes)
	}
	return strings.TrimSpace(text)
}

// Render fills in the compose template. Unknown secrets are an error, so a typo can't ship an empty password.
func (a App) Render(v Vars) (string, error) {
	text, err := a.fill(a.Compose, v)
	if err != nil {
		return "", err
	}
	buf := bytes.NewBufferString(text)
	// Must be valid YAML with at least one service.
	var parsed struct {
		Services map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(buf.Bytes(), &parsed); err != nil || len(parsed.Services) == 0 {
		return "", fmt.Errorf("%s: the compose template doesn't make a valid compose file: %v", a.ID, err)
	}
	return buf.String(), nil
}
