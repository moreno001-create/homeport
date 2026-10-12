// Package server is Homeport's web side: the sign-in portal and admin panel on the main domain, the sign-in check
// Caddy asks before letting anyone into an app, and the JSON API used by the admin panel and the `homeport` command.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"homeport/internal/apps"
	"homeport/internal/assist"
	"homeport/internal/auth"
	"homeport/internal/dyndns"
	"homeport/internal/state"
)

const (
	cookieName     = "hp_session"
	sessionLife    = 14 * 24 * time.Hour
	handoffLife    = 60 * time.Second
	maxBadLogins   = 8 // per address, per 10 minutes
	badLoginWindow = 10 * time.Minute
)

type Server struct {
	Store   *state.Store
	Apps    *apps.Manager
	Version string
	// ApplyProxy rewrites Caddy's configuration (after settings or domain changes).
	ApplyProxy func(ctx context.Context) error
	// Signer signs OpenID Connect tokens for apps.
	Signer *Signer
	// Assist runs tools for AI helpers (MCP on the root socket, approvals and audit in the admin panel).
	Assist *assist.Assistant
	// SetTerminal turns the web terminal on or off (nil where there isn't one).
	SetTerminal func(ctx context.Context, on bool) error
	// Chat is the admin panel's assistant chat (Claude or a local model, with the same tools).
	Chat *assist.Chat
	// LocalAddrs are this machine's own network addresses: the portal and admin panel answer on them too, so
	// https://192.168.1.50/admin works before any DNS is set up.
	LocalAddrs func() []string
	// CertDir holds uploaded certificates (where Caddy can read them).
	CertDir string
	// DynDNS gives free domains and keeps them pointed here (nil: not offered).
	DynDNS *dyndns.Updater
	// HTTPSAddr is where Caddy serves HTTPS on this machine, for looking at each domain's certificate.
	HTTPSAddr string

	jobs     jobs
	mu       sync.Mutex
	handoffs map[string]handoff     // one-time codes that carry a sign-in to an app's domain
	bad      map[string][]time.Time // failed sign-ins by address
	codes    map[string]authCode    // OIDC authorization codes
}

// A handoff carries a signed-in portal session to one app domain, once, within a minute.
type handoff struct {
	user, parent, domain string
	expires              time.Time
}

// Handler serves browsers (through Caddy) on 127.0.0.1.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Caddy's sign-in check for apps, and the hand-off pages it routes to on app domains.
	mux.HandleFunc("GET /auth/verify", s.verify)
	mux.HandleFunc("GET /auth/admin", s.verifyAdmin)
	mux.HandleFunc("GET /.homeport/callback", s.callback)
	mux.HandleFunc("GET /.homeport/signout", s.appSignOut)
	// The main domain: portal, sign-in, admin panel and its API.
	mux.HandleFunc("GET /{$}", s.portal)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /sso/continue", s.continueTo)
	mux.HandleFunc("GET /admin", s.adminPage)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", assets()))
	s.oidcRoutes(mux)
	s.api(mux, s.browserAdmin)
	ai := s.aiHandler()
	return securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The AI page has its own name; Caddy's sign-in checks (/auth/) come from any site.
		if h := s.aiHost(); h != "" && host(r) == h && !strings.HasPrefix(r.URL.Path, "/auth/") {
			ai.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	}))
}

// LocalHandler serves the root-only socket the `homeport` command uses: same API, already trusted.
func (s *Server) LocalHandler() http.Handler {
	mux := http.NewServeMux()
	s.api(mux, func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			next(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, state.User{Name: "root", DisplayName: "root", Admin: true})))
		}
	})
	mux.HandleFunc("POST /api/init", s.initServer)
	// MCP for AI agents (`homeport mcp` carries it over stdin/stdout). Root-only, like the rest of this socket.
	mux.HandleFunc("POST /api/mcp", s.mcp)
	return mux
}

// ListenLocal serves the root-only socket.
func (s *Server) ListenLocal(path string) error {
	_ = os.Remove(path)
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		return err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return http.Serve(l, s.LocalHandler())
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/auth/") {
			h.Set("X-Frame-Options", "DENY")
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

// host is the domain the browser asked for (Caddy passes it through).
func host(r *http.Request) string {
	h := r.Header.Get("X-Forwarded-Host")
	if h == "" {
		h = r.Host
	}
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return strings.ToLower(h)
}

func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	return h
}

func (s *Server) settings() state.Settings {
	var st state.Settings
	s.Store.Read(func(d *state.Data) { st = *d.Settings() })
	return st
}

// isMainHost: the main domain, or one of this machine's own addresses.
func (s *Server) isMainHost(h string) bool {
	if h == s.settings().MainDomain {
		return true
	}
	if s.LocalAddrs != nil {
		for _, a := range s.LocalAddrs() {
			if h == a {
				return true
			}
		}
	}
	return false
}

// sessionUser is who the browser is signed in as on the domain it's talking to.
func (s *Server) sessionUser(r *http.Request) (state.User, string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return state.User{}, "", false
	}
	th := auth.TokenHash(c.Value)
	var u state.User
	ok := false
	s.Store.Read(func(d *state.Data) {
		sess, found := d.Session(th)
		if !found || sess.Domain != host(r) {
			return
		}
		if user, found := d.User(sess.User); found {
			u, ok = *user, true
		}
	})
	return u, th, ok
}

// newSession signs a browser in on one domain.
func (s *Server) newSession(w http.ResponseWriter, user, domain, parent string) (string, error) {
	token := auth.NewToken()
	th := auth.TokenHash(token)
	err := s.Store.Update(func(d *state.Data) error {
		d.AddSession(state.Session{TokenHash: th, User: user, Domain: domain, Expires: time.Now().Add(sessionLife), Parent: parent})
		return nil
	})
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(sessionLife)})
	return th, nil
}

func clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// canOpen: may this user open this app?
func canOpen(u state.User, a state.App) bool {
	if a.Access.Public {
		return true
	}
	for _, g := range a.Access.Groups {
		if u.InGroup(g) {
			return true
		}
	}
	return false
}

// safeReturn accepts only https links to the main domain or an installed app, so sign-in can't be used to send
// people to another site.
func (s *Server) safeReturn(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" {
		return "", false
	}
	h := strings.ToLower(u.Hostname())
	ok := false
	s.Store.Read(func(d *state.Data) {
		if h == d.Settings().MainDomain || h == d.AIDomain() {
			ok = true
			return
		}
		if s.LocalAddrs != nil {
			for _, a := range s.LocalAddrs() {
				if h == a {
					ok = true
					return
				}
			}
		}
		_, ok = d.AppOn(h)
	})
	return u.String(), ok
}

func (s *Server) tooManyBadLogins(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := time.Now().Add(-badLoginWindow)
	recent := s.bad[ip][:0]
	for _, t := range s.bad[ip] {
		if t.After(cut) {
			recent = append(recent, t)
		}
	}
	if s.bad == nil {
		s.bad = map[string][]time.Time{}
	}
	s.bad[ip] = recent
	return len(recent) >= maxBadLogins
}

func (s *Server) noteBadLogin(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bad == nil {
		s.bad = map[string][]time.Time{}
	}
	s.bad[ip] = append(s.bad[ip], time.Now())
}

func (s *Server) newHandoff(h handoff) string {
	code := auth.NewToken()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handoffs == nil {
		s.handoffs = map[string]handoff{}
	}
	now := time.Now()
	for k, v := range s.handoffs {
		if v.expires.Before(now) {
			delete(s.handoffs, k)
		}
	}
	s.handoffs[auth.TokenHash(code)] = h
	return code
}

func (s *Server) takeHandoff(code string) (handoff, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := auth.TokenHash(code)
	h, ok := s.handoffs[k]
	delete(s.handoffs, k) // usable once
	if !ok || h.expires.Before(time.Now()) {
		return handoff{}, false
	}
	return h, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	if errors.Is(err, state.ErrNotFound) {
		status = http.StatusNotFound
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
