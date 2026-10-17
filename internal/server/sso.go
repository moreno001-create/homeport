package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"homeport/internal/auth"
	"homeport/internal/state"
)

// verify is Caddy's forward_auth check for an app domain: 200 with the user's details lets the request through,
// anything else is sent back to the browser (a redirect to sign in, or a "not allowed" page).
func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("app")
	var app state.App
	found := false
	s.Store.Read(func(d *state.Data) {
		if a, ok := d.App(id); ok {
			app, found = *a, true
		}
	})
	if !found || app.Domain != host(r) {
		http.Error(w, "Unknown app", http.StatusNotFound)
		return
	}
	if app.Access.Public {
		w.WriteHeader(http.StatusOK)
		return
	}
	u, _, ok := s.sessionUser(r)
	if !ok {
		// A browser opening a page is sent to sign in; anything else (an API call, a form post) just gets 401.
		method := r.Header.Get("X-Forwarded-Method")
		if method != "" && method != http.MethodGet && method != http.MethodHead {
			http.Error(w, "Sign in first", http.StatusUnauthorized)
			return
		}
		back := "https://" + hostWithPort(r) + r.Header.Get("X-Forwarded-Uri")
		// Sign-in is on the main domain; for an app on the internet under the home name (whose main domain only
		// works at home) it's on the sign-in name, which the internet reaches too (on the same port as the app).
		signIn := s.address(s.settings().MainDomain)
		s.Store.Read(func(d *state.Data) {
			if n := d.SignInName(); n != "" && app.Internet && d.UnderHome(app.Domain) && !d.Settings().HomeOnInternet {
				signIn = n
				if p := d.Settings().InternetPort; p != 0 && p != 443 {
					signIn += ":" + strconv.Itoa(p)
				}
			}
		})
		http.Redirect(w, r, "https://"+signIn+"/sso/continue?return="+url.QueryEscape(back), http.StatusFound)
		return
	}
	if !canOpen(u, app) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		renderPage(w, "denied", pageData{Title: "Not allowed", User: &u, Main: s.settings().MainDomain, MainAddr: s.address(s.settings().MainDomain), App: &app})
		return
	}
	h := w.Header()
	h.Set("Remote-User", u.Name)
	h.Set("Remote-Name", headerSafe(u.DisplayName))
	h.Set("Remote-Email", headerSafe(u.Email))
	h.Set("Remote-Groups", strings.Join(groupsOf(u), ","))
	w.WriteHeader(http.StatusOK)
}

// verifyAdmin lets only admins through to the web terminal (Caddy asks before every request, the WebSocket included).
func (s *Server) verifyAdmin(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.sessionUser(r)
	switch {
	case ok && u.Admin:
		w.WriteHeader(http.StatusOK)
	case ok:
		http.Error(w, "Only admins can use the terminal", http.StatusForbidden)
	case r.Header.Get("X-Forwarded-Method") != "" && r.Header.Get("X-Forwarded-Method") != http.MethodGet:
		http.Error(w, "Sign in first", http.StatusUnauthorized)
	default:
		// Signing in happens on this same address, so the way back is just the path.
		back := r.Header.Get("X-Forwarded-Uri")
		if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") {
			back = "/terminal/"
		}
		http.Redirect(w, r, "/login?return="+url.QueryEscape(back), http.StatusFound)
	}
}

func groupsOf(u state.User) []string {
	g := append([]string(nil), u.Groups...)
	if u.Admin {
		g = append(g, "admins")
	}
	return g
}

// headerSafe keeps a value to printable ASCII on one line.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, s)
}

// continueTo is where an app sends someone who isn't signed in on its domain. Signed in on the portal: hand the
// sign-in over to the app's domain. Not signed in: sign in first, then come back here.
func (s *Server) continueTo(w http.ResponseWriter, r *http.Request) {
	ret, ok := s.safeReturn(r.URL.Query().Get("return"))
	if !ok {
		http.Error(w, "That link isn't one of this server's apps.", http.StatusBadRequest)
		return
	}
	u, th, signedIn := s.sessionUser(r)
	if !signedIn {
		http.Redirect(w, r, "/login?return="+url.QueryEscape(r.URL.String()), http.StatusFound)
		return
	}
	target, _ := url.Parse(ret)
	if s.isMainHost(strings.ToLower(target.Hostname())) {
		http.Redirect(w, r, ret, http.StatusFound)
		return
	}
	code := s.newHandoff(handoff{user: u.Name, parent: th, domain: strings.ToLower(target.Hostname()), expires: time.Now().Add(handoffLife)})
	http.Redirect(w, r, "https://"+target.Host+"/.homeport/callback?code="+url.QueryEscape(code)+"&return="+url.QueryEscape(ret), http.StatusFound)
}

// callback runs on an app's domain: trade the one-time code for a session there, then go where the person was going.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	h, ok := s.takeHandoff(r.URL.Query().Get("code"))
	if !ok || h.domain != host(r) {
		http.Error(w, "This sign-in link has expired or was already used. Open the app again.", http.StatusBadRequest)
		return
	}
	ret, ok := s.safeReturn(r.URL.Query().Get("return"))
	if !ok {
		ret = "https://" + h.domain + "/"
	}
	if t, _ := url.Parse(ret); t == nil || !strings.EqualFold(t.Hostname(), h.domain) {
		ret = "https://" + h.domain + "/"
	}
	if _, err := s.newSession(w, h.user, h.domain, h.parent); err != nil {
		http.Error(w, "Couldn't sign you in: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, ret, http.StatusFound)
}

// appSignOut on an app's domain signs out there and on the portal.
func (s *Server) appSignOut(w http.ResponseWriter, r *http.Request) {
	if _, th, ok := s.sessionUser(r); ok {
		var parent string
		s.Store.Read(func(d *state.Data) {
			if sess, found := d.Session(th); found {
				parent = sess.Parent
			}
		})
		_ = s.Store.Update(func(d *state.Data) error {
			d.DeleteSession(th)
			if parent != "" {
				d.DeleteSession(parent)
			}
			return nil
		})
	}
	clearCookie(w)
	http.Redirect(w, r, "https://"+s.address(s.settings().MainDomain)+"/login", http.StatusFound)
}

func (s *Server) onMain(w http.ResponseWriter, r *http.Request) bool {
	if !s.isMainHost(host(r)) {
		http.NotFound(w, r)
		return false
	}
	return true
}

// portal lists the apps this person may open.
func (s *Server) portal(w http.ResponseWriter, r *http.Request) {
	if !s.onMain(w, r) {
		return
	}
	u, _, ok := s.sessionUser(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	var mine []state.App
	links, descs := map[string]string{}, map[string]string{}
	s.Store.Read(func(d *state.Data) {
		for _, a := range d.Apps() {
			if canOpen(u, a) {
				mine = append(mine, a)
				links[a.ID] = d.AppAddress(a)
			}
		}
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	for _, a := range mine {
		descs[a.ID] = s.Apps.Description(a)
	}
	look := s.lookOf(u.Name)
	hidden := map[string]bool{}
	for _, p := range look.Hide {
		hidden[p] = true
	}
	renderPage(w, "portal", pageData{Title: "Your apps", User: &u, Main: s.settings().MainDomain, Apps: mine, Links: links, Descs: descs, AI: s.address(s.aiHost()),
		Greeting: greeting(time.Now()), BodyClass: lookClass(look), Hide: hidden})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.onMain(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	renderPage(w, "login", pageData{Title: "Sign in", Main: s.settings().MainDomain, Return: r.URL.Query().Get("return")})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.onMain(w, r) {
		return
	}
	ip := clientIP(r)
	ret := r.FormValue("return")
	fail := func(msg string, status int) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		renderPage(w, "login", pageData{Title: "Sign in", Main: s.settings().MainDomain, Return: ret, Error: msg, Name: r.FormValue("name")})
	}
	if s.tooManyBadLogins(ip) {
		fail("Too many wrong passwords from this address. Wait a few minutes and try again.", http.StatusTooManyRequests)
		return
	}
	name := strings.ToLower(strings.TrimSpace(r.FormValue("name")))
	var hash string
	s.Store.Read(func(d *state.Data) {
		if u, ok := d.User(name); ok {
			hash = u.Password
		}
	})
	if hash == "" {
		// Spend the same time as a real check, so timing doesn't reveal which names exist.
		_ = auth.CheckPassword(dummyHash(), r.FormValue("password"))
	}
	if hash == "" || !auth.CheckPassword(hash, r.FormValue("password")) {
		s.noteBadLogin(ip)
		fail("That user name and password don't match.", http.StatusUnauthorized)
		return
	}
	// Signed in on the address used: the main domain, or this machine's own address.
	if _, err := s.newSession(w, name, host(r), ""); err != nil {
		fail("Couldn't sign you in: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Back to where they were going: an /sso/continue link (on to an app) or a page on this domain.
	if strings.HasPrefix(ret, "/") && !strings.HasPrefix(ret, "//") {
		http.Redirect(w, r, ret, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if _, th, ok := s.sessionUser(r); ok {
		_ = s.Store.Update(func(d *state.Data) error { d.DeleteSession(th); return nil })
	}
	clearCookie(w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (s *Server) adminPage(w http.ResponseWriter, r *http.Request) {
	if !s.onMain(w, r) {
		return
	}
	u, _, ok := s.sessionUser(r)
	if !ok {
		http.Redirect(w, r, "/login?return=/admin", http.StatusFound)
		return
	}
	if !u.Admin {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		renderPage(w, "denied", pageData{Title: "Not allowed", User: &u, Main: s.settings().MainDomain, MainAddr: s.address(s.settings().MainDomain)})
		return
	}
	serveAdmin(w, r)
}

// dummyHash is checked against when the user name doesn't exist, so a wrong name takes as long as a wrong password.
var dummyHash = sync.OnceValue(func() string {
	h, _ := auth.HashPassword(auth.NewToken())
	return h
})

// greeting suits the time of day on the server.
func greeting(t time.Time) string {
	switch h := t.Hour(); {
	case h < 5:
		return "Good evening"
	case h < 12:
		return "Good morning"
	case h < 18:
		return "Good afternoon"
	}
	return "Good evening"
}
