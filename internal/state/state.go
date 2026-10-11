// Package state holds everything Homeport knows about the server: settings, users, domains and installed apps.
// It lives in one JSON file, written atomically after every change, readable only by root.
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// TLSMode picks where certificates come from.
type TLSMode string

const (
	// TLSLetsEncrypt gets real certificates (the server must be reachable on ports 80 and 443).
	TLSLetsEncrypt TLSMode = "letsencrypt"
	// TLSInternal uses Caddy's own certificate authority: for test machines and private networks.
	TLSInternal TLSMode = "internal"
)

type Settings struct {
	// MainDomain hosts the portal (/) and the admin panel (/admin).
	MainDomain string  `json:"mainDomain"`
	ACMEEmail  string  `json:"acmeEmail,omitempty"`
	TLS        TLSMode `json:"tls"`
	// AssistantShell lets AI helpers ask to run shell commands on the server (each one still needs approval).
	AssistantShell bool `json:"assistantShell,omitempty"`
	// ApprovalMinutes is how long a change waits for an admin before it's dropped (default 10).
	ApprovalMinutes int `json:"approvalMinutes,omitempty"`
	// Terminal serves a web terminal (the system's login prompt) at /terminal/ on the main domain, for admins.
	Terminal bool `json:"terminal,omitempty"`
	// Chat is how the admin panel's assistant chat picks a model.
	Chat ChatSettings `json:"chat"`
}

// ChatSettings choose the models behind the assistant chat. The Anthropic API key isn't here: it's kept in its own
// root-only file and never sent back to the browser.
type ChatSettings struct {
	// Mode is "auto" (the local model for easy things, Claude for hard ones), "local" or "claude".
	Mode string `json:"mode,omitempty"`
	// AskBeforeCloud makes the chat ask before anything is sent to Claude.
	AskBeforeCloud bool `json:"askBeforeCloud,omitempty"`
	// MonthlyCapUSD stops Claude for the rest of the month once its estimated cost reaches this (0: no cap).
	MonthlyCapUSD float64 `json:"monthlyCapUSD,omitempty"`
	// LocalURL is an OpenAI-compatible server (LM Studio, Ollama…), e.g. http://192.168.1.20:1234/v1.
	LocalURL   string `json:"localURL,omitempty"`
	LocalModel string `json:"localModel,omitempty"`
	// ClaudeModel and Effort tune Claude (defaults claude-opus-5-5, medium).
	ClaudeModel string `json:"claudeModel,omitempty"`
	Effort      string `json:"effort,omitempty"`
}

type User struct {
	Name        string    `json:"name"`
	DisplayName string    `json:"displayName"`
	Email       string    `json:"email,omitempty"`
	Password    string    `json:"password"` // argon2id hash
	Groups      []string  `json:"groups,omitempty"`
	Admin       bool      `json:"admin,omitempty"`
	Created     time.Time `json:"created"`
}

// InGroup reports whether the user belongs to group g ("all" matches every user, "admins" every admin).
func (u User) InGroup(g string) bool {
	if g == "all" || (g == "admins" && u.Admin) {
		return true
	}
	for _, x := range u.Groups {
		if x == g {
			return true
		}
	}
	return false
}

type Domain struct {
	Name  string    `json:"name"`
	Added time.Time `json:"added"`
	// Cert is where this domain's certificate comes from; empty follows the server's setting (Settings.TLS).
	Cert CertMode `json:"cert,omitempty"`
	// DynDNS is set for a free domain from a dynamic DNS service, which Homeport keeps pointed at this server.
	DynDNS *DynDNS `json:"dyndns,omitempty"`
}

// CertMode picks one domain's certificate.
type CertMode string

const (
	CertLetsEncrypt CertMode = "letsencrypt" // Caddy gets it from Let's Encrypt
	CertInternal    CertMode = "internal"    // Caddy's own certificate authority
	CertCustom      CertMode = "custom"      // one the admin uploaded
)

// DynDNS is a free domain kept pointed at this server's public address. Its token isn't here: it's kept in its own
// root-only file.
type DynDNS struct {
	Provider string    `json:"provider"` // "desec" or "duckdns"
	IPv4     string    `json:"ipv4,omitempty"`
	IPv6     string    `json:"ipv6,omitempty"`
	Updated  time.Time `json:"updated,omitempty"` // last successful update
	Error    string    `json:"error,omitempty"`   // the last update's error, if it failed
}

// Access says who may open an app: everyone on the internet, or signed-in users in one of the groups.
type Access struct {
	Public bool     `json:"public,omitempty"`
	Groups []string `json:"groups,omitempty"`
}

type App struct {
	ID        string            `json:"id"`      // instance id, also the folder name (e.g. "nextcloud", "nextcloud-2")
	Catalog   string            `json:"catalog"` // which catalog entry it was installed from
	Name      string            `json:"name"`    // shown in the portal
	Domain    string            `json:"domain"`  // where it's served
	Port      int               `json:"port"`    // its web port on 127.0.0.1
	Access    Access            `json:"access"`
	Secrets   map[string]string `json:"secrets,omitempty"`
	Installed time.Time         `json:"installed"`
	Updated   time.Time         `json:"updated,omitempty"`
	// OIDC is set for apps that sign people in with Homeport over OpenID Connect (client id = app id; the client
	// secret is Secrets["oidc_client_secret"]).
	OIDC *OIDCClient `json:"oidc,omitempty"`
}

type OIDCClient struct {
	RedirectURIs []string `json:"redirectUris"`
}

// RefreshToken lets an app renew a sign-in without the person. Only its hash is stored.
type RefreshToken struct {
	Hash     string    `json:"hash"`
	User     string    `json:"user"`
	Client   string    `json:"client"`
	Scope    string    `json:"scope"`
	AuthTime time.Time `json:"authTime"`
	Expires  time.Time `json:"expires"`
}

// Session is a signed-in browser. Only a hash of its token is stored.
type Session struct {
	TokenHash string    `json:"tokenHash"`
	User      string    `json:"user"`
	Domain    string    `json:"domain"` // the cookie's domain
	Expires   time.Time `json:"expires"`
	// Parent is the portal session an app-domain session came from: signing out of the portal ends both.
	Parent string `json:"parent,omitempty"`
}

type data struct {
	Settings Settings       `json:"settings"`
	Users    []User         `json:"users"`
	Domains  []Domain       `json:"domains"`
	Apps     []App          `json:"apps"`
	Sessions []Session      `json:"sessions"`
	Refresh  []RefreshToken `json:"refresh,omitempty"`
}

// Store is the server's state, safe for use from many goroutines.
type Store struct {
	mu   sync.RWMutex
	path string
	d    data
}

var ErrNotFound = errors.New("not found")

// Open loads the state file, or starts empty if there isn't one yet.
func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.d); err != nil {
		return nil, err
	}
	return s, nil
}

// save writes the whole state atomically (lock held).
func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Update changes the state under the write lock and saves it if fn succeeds.
func (s *Store) Update(fn func(d *Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	view := Data{&s.d}
	if err := fn(&view); err != nil {
		return err
	}
	return s.save()
}

// Read looks at the state under the read lock. fn must not keep references to slices it gets.
func (s *Store) Read(fn func(d *Data)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&Data{&s.d})
}

// Data is the state as seen inside Read and Update.
type Data struct{ d *data }

func (v *Data) Settings() *Settings { return &v.d.Settings }

func (v *Data) Users() []User {
	out := append([]User(nil), v.d.Users...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (v *Data) User(name string) (*User, bool) {
	for i := range v.d.Users {
		if v.d.Users[i].Name == name {
			return &v.d.Users[i], true
		}
	}
	return nil, false
}

func (v *Data) AddUser(u User) { v.d.Users = append(v.d.Users, u) }

func (v *Data) DeleteUser(name string) bool {
	for i, u := range v.d.Users {
		if u.Name == name {
			v.d.Users = append(v.d.Users[:i], v.d.Users[i+1:]...)
			// Signed out everywhere.
			v.d.Sessions = filter(v.d.Sessions, func(s Session) bool { return s.User != name })
			v.d.Refresh = filter(v.d.Refresh, func(r RefreshToken) bool { return r.User != name })
			return true
		}
	}
	return false
}

func (v *Data) Domains() []Domain { return append([]Domain(nil), v.d.Domains...) }

func (v *Data) HasDomain(name string) bool {
	for _, d := range v.d.Domains {
		if d.Name == name {
			return true
		}
	}
	return false
}

func (v *Data) Domain(name string) (*Domain, bool) {
	for i := range v.d.Domains {
		if v.d.Domains[i].Name == name {
			return &v.d.Domains[i], true
		}
	}
	return nil, false
}

func (v *Data) AddDomain(d Domain) { v.d.Domains = append(v.d.Domains, d) }

func (v *Data) DeleteDomain(name string) bool {
	n := len(v.d.Domains)
	v.d.Domains = filter(v.d.Domains, func(d Domain) bool { return d.Name != name })
	return len(v.d.Domains) != n
}

func (v *Data) Apps() []App {
	out := append([]App(nil), v.d.Apps...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (v *Data) App(id string) (*App, bool) {
	for i := range v.d.Apps {
		if v.d.Apps[i].ID == id {
			return &v.d.Apps[i], true
		}
	}
	return nil, false
}

// AppOn is the app served on domain, if any.
func (v *Data) AppOn(domain string) (*App, bool) {
	for i := range v.d.Apps {
		if v.d.Apps[i].Domain == domain {
			return &v.d.Apps[i], true
		}
	}
	return nil, false
}

func (v *Data) AddApp(a App) { v.d.Apps = append(v.d.Apps, a) }

func (v *Data) DeleteApp(id string) bool {
	n := len(v.d.Apps)
	v.d.Apps = filter(v.d.Apps, func(a App) bool { return a.ID != id })
	v.DeleteRefreshOfClient(id)
	return len(v.d.Apps) != n
}

// UsedPorts lists the ports apps already have.
func (v *Data) UsedPorts() map[int]bool {
	used := map[int]bool{}
	for _, a := range v.d.Apps {
		used[a.Port] = true
	}
	return used
}

func (v *Data) AddSession(s Session) {
	now := time.Now()
	// Drop expired ones while here.
	v.d.Sessions = filter(v.d.Sessions, func(x Session) bool { return x.Expires.After(now) })
	v.d.Sessions = append(v.d.Sessions, s)
}

func (v *Data) Session(tokenHash string) (*Session, bool) {
	for i := range v.d.Sessions {
		if v.d.Sessions[i].TokenHash == tokenHash && v.d.Sessions[i].Expires.After(time.Now()) {
			return &v.d.Sessions[i], true
		}
	}
	return nil, false
}

// DeleteSession ends a session and every app-domain session made from it.
func (v *Data) DeleteSession(tokenHash string) {
	v.d.Sessions = filter(v.d.Sessions, func(s Session) bool { return s.TokenHash != tokenHash && s.Parent != tokenHash })
}

// DeleteSessionsOf signs a user out of every browser and app (after a password change, say).
func (v *Data) DeleteSessionsOf(user string) {
	v.d.Sessions = filter(v.d.Sessions, func(s Session) bool { return s.User != user })
	v.d.Refresh = filter(v.d.Refresh, func(r RefreshToken) bool { return r.User != user })
}

func (v *Data) AddRefresh(r RefreshToken) {
	now := time.Now()
	v.d.Refresh = filter(v.d.Refresh, func(x RefreshToken) bool { return x.Expires.After(now) })
	v.d.Refresh = append(v.d.Refresh, r)
}

// TakeRefresh uses up a refresh token (each one works once; a new one is issued with it).
func (v *Data) TakeRefresh(hash string) (RefreshToken, bool) {
	for i, r := range v.d.Refresh {
		if r.Hash == hash {
			v.d.Refresh = append(v.d.Refresh[:i], v.d.Refresh[i+1:]...)
			return r, r.Expires.After(time.Now())
		}
	}
	return RefreshToken{}, false
}

// DeleteRefreshOfClient forgets an app's refresh tokens (it was removed).
func (v *Data) DeleteRefreshOfClient(client string) {
	v.d.Refresh = filter(v.d.Refresh, func(r RefreshToken) bool { return r.Client != client })
}

func filter[T any](in []T, keep func(T) bool) []T {
	out := in[:0]
	for _, x := range in {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}
