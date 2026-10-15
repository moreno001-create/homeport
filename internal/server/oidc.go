package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"homeport/internal/auth"
	"homeport/internal/state"
)

// Homeport is an OpenID Connect provider for its apps, on the main domain: an app sends people here to sign in and
// gets back who they are. An app only gets a user who may open it (its access groups).

const (
	codeLife    = 60 * time.Second
	accessLife  = time.Hour
	idLife      = time.Hour
	refreshLife = 30 * 24 * time.Hour
)

type authCode struct {
	client, user, redirect, scope, nonce string
	challenge, method                    string
	authTime, expires                    time.Time
}

func (s *Server) issuer() string { return "https://" + s.address(s.settings().MainDomain) }

func (s *Server) oidcRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/openid-configuration", s.discovery)
	mux.HandleFunc("GET /oidc/jwks", s.jwks)
	mux.HandleFunc("GET /oidc/authorize", s.authorize)
	mux.HandleFunc("POST /oidc/token", s.token)
	mux.HandleFunc("GET /oidc/userinfo", s.userinfo)
	mux.HandleFunc("POST /oidc/userinfo", s.userinfo)
	mux.HandleFunc("GET /oidc/logout", s.oidcLogout)
}

func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	if !s.onMain(w, r) {
		return
	}
	iss := s.issuer()
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                iss,
		"authorization_endpoint":                iss + "/oidc/authorize",
		"token_endpoint":                        iss + "/oidc/token",
		"userinfo_endpoint":                     iss + "/oidc/userinfo",
		"jwks_uri":                              iss + "/oidc/jwks",
		"end_session_endpoint":                  iss + "/oidc/logout",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "profile", "email", "groups", "offline_access"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"claims_supported":                      []string{"sub", "name", "preferred_username", "email", "email_verified", "groups"},
		"code_challenge_methods_supported":      []string{"S256", "plain"},
	})
}

func (s *Server) jwks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Signer.JWKS())
}

// client finds an app registered for OIDC.
func (s *Server) client(id string) (state.App, bool) {
	var app state.App
	ok := false
	s.Store.Read(func(d *state.Data) {
		if a, found := d.App(id); found && a.OIDC != nil {
			app, ok = *a, true
		}
	})
	return app, ok
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	if !s.onMain(w, r) {
		return
	}
	q := r.URL.Query()
	app, ok := s.client(q.Get("client_id"))
	redirect := q.Get("redirect_uri")
	// Without a known client and one of its own redirect addresses, nothing is sent anywhere: show an error here.
	if !ok || !slices.Contains(app.OIDC.RedirectURIs, redirect) {
		http.Error(w, "This app isn't registered to sign in with this server (unknown client or redirect address).", http.StatusBadRequest)
		return
	}
	fail := func(code, desc string) {
		u, _ := url.Parse(redirect)
		v := u.Query()
		v.Set("error", code)
		v.Set("error_description", desc)
		if st := q.Get("state"); st != "" {
			v.Set("state", st)
		}
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusFound)
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only the authorization code flow is supported")
		return
	}
	scopes := strings.Fields(q.Get("scope"))
	if !slices.Contains(scopes, "openid") {
		fail("invalid_scope", "the openid scope is required")
		return
	}
	method := q.Get("code_challenge_method")
	if q.Get("code_challenge") != "" && method != "" && method != "S256" && method != "plain" {
		fail("invalid_request", "code_challenge_method must be S256 or plain")
		return
	}
	u, _, signedIn := s.sessionUser(r)
	if !signedIn {
		if q.Get("prompt") == "none" {
			fail("login_required", "not signed in")
			return
		}
		http.Redirect(w, r, "/login?return="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	if !canOpen(u, app) {
		fail("access_denied", "this account may not use "+app.Name)
		return
	}
	code := auth.NewToken()
	s.mu.Lock()
	if s.codes == nil {
		s.codes = map[string]authCode{}
	}
	now := time.Now()
	for k, c := range s.codes {
		if c.expires.Before(now) {
			delete(s.codes, k)
		}
	}
	s.codes[auth.TokenHash(code)] = authCode{client: app.ID, user: u.Name, redirect: redirect, scope: strings.Join(scopes, " "),
		nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), method: method, authTime: now, expires: now.Add(codeLife)}
	s.mu.Unlock()
	dest, _ := url.Parse(redirect)
	v := dest.Query()
	v.Set("code", code)
	if st := q.Get("state"); st != "" {
		v.Set("state", st)
	}
	v.Set("iss", s.issuer())
	dest.RawQuery = v.Encode()
	http.Redirect(w, r, dest.String(), http.StatusFound)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	te := func(status int, code, desc string) {
		writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
	}
	if err := r.ParseForm(); err != nil {
		te(http.StatusBadRequest, "invalid_request", "bad form")
		return
	}
	// The app proves who it is with its secret (Basic auth, or in the form).
	id, secret, basic := r.BasicAuth()
	if basic {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	app, ok := s.client(id)
	want := app.Secrets["oidc_client_secret"]
	if !ok || want == "" || subtle.ConstantTimeCompare([]byte(secret), []byte(want)) != 1 {
		te(http.StatusUnauthorized, "invalid_client", "unknown app or wrong secret")
		return
	}
	var user, scope, nonce string
	var authTime time.Time
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.mu.Lock()
		k := auth.TokenHash(r.PostForm.Get("code"))
		c, found := s.codes[k]
		delete(s.codes, k) // a code works once
		s.mu.Unlock()
		if !found || c.expires.Before(time.Now()) || c.client != app.ID || c.redirect != r.PostForm.Get("redirect_uri") {
			te(http.StatusBadRequest, "invalid_grant", "the code is unknown, used, expired, or for another app")
			return
		}
		if c.challenge != "" && !pkceOK(c.challenge, c.method, r.PostForm.Get("code_verifier")) {
			te(http.StatusBadRequest, "invalid_grant", "code_verifier doesn't match")
			return
		}
		user, scope, nonce, authTime = c.user, c.scope, c.nonce, c.authTime
	case "refresh_token":
		var rt state.RefreshToken
		var found bool
		_ = s.Store.Update(func(d *state.Data) error {
			rt, found = d.TakeRefresh(auth.TokenHash(r.PostForm.Get("refresh_token")))
			return nil
		})
		if !found || rt.Client != app.ID {
			te(http.StatusBadRequest, "invalid_grant", "the refresh token is unknown, used or expired")
			return
		}
		user, scope, authTime = rt.User, rt.Scope, rt.AuthTime
	default:
		te(http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}
	// The person must still exist and still be allowed to use the app.
	var u state.User
	var exists bool
	s.Store.Read(func(d *state.Data) {
		if x, ok := d.User(user); ok {
			u, exists = *x, true
		}
	})
	if !exists || !canOpen(u, app) {
		te(http.StatusBadRequest, "invalid_grant", "this account may no longer use the app")
		return
	}
	now := time.Now()
	access, err := s.Signer.Sign(map[string]any{"iss": s.issuer(), "sub": u.Name, "aud": app.ID, "client_id": app.ID,
		"scope": scope, "iat": now.Unix(), "exp": now.Add(accessLife).Unix(), "typ": "access"})
	if err != nil {
		te(http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	idc := s.claims(u, scope)
	idc["iss"], idc["aud"], idc["iat"], idc["exp"], idc["auth_time"] = s.issuer(), app.ID, now.Unix(), now.Add(idLife).Unix(), authTime.Unix()
	idc["azp"] = app.ID
	if nonce != "" {
		idc["nonce"] = nonce
	}
	idToken, err := s.Signer.Sign(idc)
	if err != nil {
		te(http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	refresh := auth.NewToken()
	err = s.Store.Update(func(d *state.Data) error {
		d.AddRefresh(state.RefreshToken{Hash: auth.TokenHash(refresh), User: u.Name, Client: app.ID, Scope: scope, AuthTime: authTime, Expires: now.Add(refreshLife)})
		return nil
	})
	if err != nil {
		te(http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": int(accessLife.Seconds()),
		"id_token": idToken, "refresh_token": refresh, "scope": scope})
}

func pkceOK(challenge, method, verifier string) bool {
	if verifier == "" {
		return false
	}
	if method == "S256" {
		sum := sha256.Sum256([]byte(verifier))
		return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
	}
	return subtle.ConstantTimeCompare([]byte(verifier), []byte(challenge)) == 1
}

// claims are what an app learns about a person, by the scopes it asked for.
func (s *Server) claims(u state.User, scope string) map[string]any {
	c := map[string]any{"sub": u.Name}
	sc := strings.Fields(scope)
	if slices.Contains(sc, "profile") {
		c["name"], c["preferred_username"] = u.DisplayName, u.Name
	}
	if slices.Contains(sc, "email") && u.Email != "" {
		// Emails are set by admins in Homeport, not by the person, so apps may trust them.
		c["email"], c["email_verified"] = u.Email, true
	}
	if slices.Contains(sc, "groups") {
		c["groups"] = groupsOf(u)
	}
	return c
}

func (s *Server) userinfo(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok == "" || tok == r.Header.Get("Authorization") {
		_ = r.ParseForm()
		tok = r.PostForm.Get("access_token")
	}
	claims, err := s.Signer.Verify(tok)
	if err != nil || claims["typ"] != "access" {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		writeError(w, http.StatusUnauthorized, errors.New("invalid or expired access token"))
		return
	}
	name, _ := claims["sub"].(string)
	scope, _ := claims["scope"].(string)
	var u state.User
	var ok bool
	s.Store.Read(func(d *state.Data) {
		if x, found := d.User(name); found {
			u, ok = *x, true
		}
	})
	if !ok {
		writeError(w, http.StatusUnauthorized, errors.New("this account no longer exists"))
		return
	}
	writeJSON(w, http.StatusOK, s.claims(u, scope))
}

// oidcLogout signs out of the portal (and every app domain it reached), then goes back to the app if it asked to.
func (s *Server) oidcLogout(w http.ResponseWriter, r *http.Request) {
	if _, th, ok := s.sessionUser(r); ok {
		_ = s.Store.Update(func(d *state.Data) error { d.DeleteSession(th); return nil })
	}
	clearCookie(w)
	back := r.URL.Query().Get("post_logout_redirect_uri")
	if back != "" {
		if app, ok := s.client(r.URL.Query().Get("client_id")); ok {
			if u, err := url.Parse(back); err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), app.Domain) {
				http.Redirect(w, r, back, http.StatusFound)
				return
			}
		}
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}
