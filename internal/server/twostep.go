package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/skip2/go-qrcode"

	"homeport/internal/auth"
	"homeport/internal/state"
)

// Two-step sign-in. Each person can add an authenticator app (6-digit codes, with recovery codes for a lost phone)
// and passkeys (Face ID, Touch ID, a security key). Then signing in with a password asks for a second step, and a
// passkey alone signs in (it is something you have and something you are). Turning these on or off needs the
// password. An admin can turn them off for someone who lost everything.

const (
	twoStepCookie = "homeport_2step"
	twoStepLife   = 5 * time.Minute
)

type pendingSignIn struct {
	user     string
	ret      string
	host     string
	expires  time.Time
	tries    int
	webauthn *webauthn.SessionData
}

type twoStep struct {
	mu           sync.Mutex
	pending      map[string]*pendingSignIn        // token hash → a password-checked sign-in waiting for its second step
	setup        map[string]string                // user → an authenticator secret being set up
	register     map[string]*webauthn.SessionData // user → a passkey being added
	passwordless map[string]*webauthn.SessionData // token hash → a passkey sign-in started on the sign-in page
	usedStep     map[string]int64                 // user → the last code step used (a code works once)
}

func (s *Server) ts() *twoStep {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.twoStep == nil {
		s.twoStep = &twoStep{pending: map[string]*pendingSignIn{}, setup: map[string]string{}, register: map[string]*webauthn.SessionData{},
			passwordless: map[string]*webauthn.SessionData{}, usedStep: map[string]int64{}}
	}
	return s.twoStep
}

// --- authenticator codes (RFC 6238: 30-second steps, 6 digits, SHA-1)

func totpAt(secret string, step int64) string {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil {
		return ""
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", n%1_000_000)
}

// totpStep finds the step a code is from (now, or one step either side for clocks a little off), or -1.
func totpStep(secret, code string, now time.Time) int64 {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return -1
	}
	t := now.Unix() / 30
	for _, step := range []int64{t, t - 1, t + 1} {
		if subtle.ConstantTimeCompare([]byte(totpAt(secret, step)), []byte(code)) == 1 {
			return step
		}
	}
	return -1
}

func newTOTPSecret() string {
	b := make([]byte, 20)
	rand.Read(b)
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}

// recoveryCodes makes ten codes (shown once) and their hashes (kept).
func recoveryCodes() (codes, hashes []string) {
	const letters = "abcdefghjkmnpqrstuvwxyz23456789"
	for i := 0; i < 10; i++ {
		b := make([]byte, 10)
		rand.Read(b)
		var c strings.Builder
		for j, x := range b {
			if j == 5 {
				c.WriteByte('-')
			}
			c.WriteByte(letters[int(x)%len(letters)])
		}
		codes = append(codes, c.String())
		hashes = append(hashes, auth.TokenHash(c.String()))
	}
	return
}

func normalizeRecovery(code string) string {
	c := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(code), " ", ""), "-", ""))
	if len(c) == 10 {
		c = c[:5] + "-" + c[5:]
	}
	return c
}

// --- passkeys (WebAuthn), for the address being used

type passkeyUser struct{ u state.User }

func (p passkeyUser) WebAuthnID() []byte          { return p.u.WebAuthnID }
func (p passkeyUser) WebAuthnName() string        { return p.u.Name }
func (p passkeyUser) WebAuthnDisplayName() string { return firstNonEmpty(p.u.DisplayName, p.u.Name) }
func (p passkeyUser) WebAuthnCredentials() []webauthn.Credential {
	var out []webauthn.Credential
	for _, k := range p.u.Passkeys {
		var c webauthn.Credential
		if json.Unmarshal(k.Credential, &c) == nil {
			out = append(out, c)
		}
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// relyingParty is WebAuthn for the address this request came to (a passkey belongs to one address).
func (s *Server) relyingParty(r *http.Request) (*webauthn.WebAuthn, error) {
	h := host(r)
	if h == "" || net.ParseIP(h) != nil {
		return nil, errors.New("passkeys need the server's name, not its number: open it at its name")
	}
	origins := []string{"https://" + h}
	if p := s.settings().InternetPort; p != 0 && p != 443 {
		origins = append(origins, "https://"+h+":"+strconv.Itoa(p))
	}
	return webauthn.New(&webauthn.Config{RPID: h, RPDisplayName: s.settings().MainDomain, RPOrigins: origins})
}

// --- the settings, from the portal

type securityView struct {
	TOTP         bool          `json:"totp"`
	RecoveryLeft int           `json:"recoveryLeft"`
	Passkeys     []passkeyView `json:"passkeys"`
	Here         string        `json:"here"`
}

type passkeyView struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Host     string    `json:"host"`
	Added    time.Time `json:"added"`
	LastUsed time.Time `json:"lastUsed"`
}

func (s *Server) twoStepRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me/security", s.browserUser(s.mySecurity))
	mux.HandleFunc("POST /api/me/totp/start", s.browserUser(s.totpStart))
	mux.HandleFunc("POST /api/me/totp/confirm", s.browserUser(s.totpConfirm))
	mux.HandleFunc("POST /api/me/totp/off", s.browserUser(s.totpOff))
	mux.HandleFunc("POST /api/me/recovery", s.browserUser(s.newRecovery))
	mux.HandleFunc("POST /api/me/passkeys/begin", s.browserUser(s.passkeyBegin))
	mux.HandleFunc("POST /api/me/passkeys/finish", s.browserUser(s.passkeyFinish))
	mux.HandleFunc("POST /api/me/passkeys/remove", s.browserUser(s.passkeyRemove))
	mux.HandleFunc("POST /login/code", s.loginCode)
	mux.HandleFunc("POST /login/passkey/begin", s.loginPasskeyBegin)
	mux.HandleFunc("POST /login/passkey/finish", s.loginPasskeyFinish)
}

func (s *Server) mySecurity(w http.ResponseWriter, r *http.Request) {
	u := s.fresh(actor(r).Name)
	v := securityView{TOTP: u.TOTP != "", RecoveryLeft: len(u.Recovery), Passkeys: []passkeyView{}, Here: host(r)}
	for _, k := range u.Passkeys {
		v.Passkeys = append(v.Passkeys, passkeyView{k.ID, k.Name, k.Host, k.Added, k.LastUsed})
	}
	writeJSON(w, http.StatusOK, v)
}

// fresh is the user as stored now (the actor in the request may be from before a change).
func (s *Server) fresh(name string) state.User {
	var u state.User
	s.Store.Read(func(d *state.Data) {
		if x, ok := d.User(name); ok {
			u = *x
		}
	})
	return u
}

// checkPassword is the password check that guards changes to two-step sign-in (wrong ones count like sign-ins).
func (s *Server) checkPassword(w http.ResponseWriter, r *http.Request, u state.User, password string) bool {
	ip := clientIP(r)
	if s.tooManyBadLogins(ip) {
		writeError(w, http.StatusTooManyRequests, errors.New("too many wrong passwords from this address; wait a few minutes"))
		return false
	}
	if !auth.CheckPassword(u.Password, password) {
		s.noteBadLogin(ip)
		writeError(w, http.StatusForbidden, errors.New("that isn't your password"))
		return false
	}
	return true
}

func (s *Server) totpStart(w http.ResponseWriter, r *http.Request) {
	u := actor(r)
	secret := newTOTPSecret()
	t := s.ts()
	t.mu.Lock()
	t.setup[u.Name] = secret
	t.mu.Unlock()
	main := s.settings().MainDomain
	uri := "otpauth://totp/" + url.PathEscape(main+":"+u.Name) + "?secret=" + secret + "&issuer=" + url.QueryEscape(main) + "&algorithm=SHA1&digits=6&period=30"
	png, err := qrcode.Encode(uri, qrcode.Medium, 320)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "uri": uri, "qr": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)})
}

func (s *Server) totpConfirm(w http.ResponseWriter, r *http.Request) {
	u := s.fresh(actor(r).Name)
	var in struct{ Password, Code string }
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !s.checkPassword(w, r, u, in.Password) {
		return
	}
	t := s.ts()
	t.mu.Lock()
	secret := t.setup[u.Name]
	t.mu.Unlock()
	step := totpStep(secret, in.Code, time.Now())
	if secret == "" || step < 0 {
		writeError(w, http.StatusBadRequest, errors.New("that code doesn't match: check the time on your phone, and type the newest code"))
		return
	}
	codes, hashes := recoveryCodes()
	err := s.Store.Update(func(d *state.Data) error {
		me, ok := d.User(u.Name)
		if !ok {
			return state.ErrNotFound
		}
		me.TOTP, me.Recovery = secret, hashes
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	t.mu.Lock()
	delete(t.setup, u.Name)
	t.usedStep[u.Name] = step
	t.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"recovery": codes})
}

func (s *Server) totpOff(w http.ResponseWriter, r *http.Request) {
	u := s.fresh(actor(r).Name)
	var in struct{ Password string }
	decode(r, &in)
	if !s.checkPassword(w, r, u, in.Password) {
		return
	}
	s.Store.Update(func(d *state.Data) error {
		if me, ok := d.User(u.Name); ok {
			me.TOTP, me.Recovery = "", nil
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) newRecovery(w http.ResponseWriter, r *http.Request) {
	u := s.fresh(actor(r).Name)
	var in struct{ Password string }
	decode(r, &in)
	if !s.checkPassword(w, r, u, in.Password) {
		return
	}
	if !u.TwoStep() {
		writeError(w, http.StatusBadRequest, errors.New("turn on two-step sign-in first"))
		return
	}
	codes, hashes := recoveryCodes()
	s.Store.Update(func(d *state.Data) error {
		if me, ok := d.User(u.Name); ok {
			me.Recovery = hashes
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]any{"recovery": codes})
}

func (s *Server) passkeyBegin(w http.ResponseWriter, r *http.Request) {
	u := s.fresh(actor(r).Name)
	var in struct{ Password string }
	decode(r, &in)
	if !s.checkPassword(w, r, u, in.Password) {
		return
	}
	rp, err := s.relyingParty(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(u.WebAuthnID) == 0 {
		id := make([]byte, 32)
		rand.Read(id)
		s.Store.Update(func(d *state.Data) error {
			if me, ok := d.User(u.Name); ok && len(me.WebAuthnID) == 0 {
				me.WebAuthnID = id
			}
			return nil
		})
		u = s.fresh(u.Name)
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range (passkeyUser{u}).WebAuthnCredentials() {
		exclude = append(exclude, c.Descriptor())
	}
	creation, session, err := rp.BeginRegistration(passkeyUser{u}, webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired),
		webauthn.WithExclusions(exclude), webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey: protocol.ResidentKeyRequirementRequired, RequireResidentKey: protocol.ResidentKeyRequired(), UserVerification: protocol.VerificationRequired}))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	t := s.ts()
	t.mu.Lock()
	t.register[u.Name] = session
	t.mu.Unlock()
	writeJSON(w, http.StatusOK, creation)
}

func (s *Server) passkeyFinish(w http.ResponseWriter, r *http.Request) {
	u := s.fresh(actor(r).Name)
	t := s.ts()
	t.mu.Lock()
	session := t.register[u.Name]
	delete(t.register, u.Name)
	t.mu.Unlock()
	rp, err := s.relyingParty(r)
	if session == nil || err != nil {
		writeError(w, http.StatusBadRequest, errors.New("start adding the passkey again"))
		return
	}
	cred, err := rp.FinishRegistration(passkeyUser{u}, *session, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the passkey wasn't accepted: %v", err))
		return
	}
	raw, _ := json.Marshal(cred)
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "Passkey"
	}
	if len(name) > 60 {
		name = name[:60]
	}
	key := state.Passkey{ID: base64.RawURLEncoding.EncodeToString(cred.ID), Name: name, Host: host(r), Credential: raw, Added: time.Now()}
	s.Store.Update(func(d *state.Data) error {
		if me, ok := d.User(u.Name); ok {
			me.Passkeys = append(me.Passkeys, key)
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) passkeyRemove(w http.ResponseWriter, r *http.Request) {
	u := s.fresh(actor(r).Name)
	var in struct{ Password, ID string }
	decode(r, &in)
	if !s.checkPassword(w, r, u, in.Password) {
		return
	}
	s.Store.Update(func(d *state.Data) error {
		if me, ok := d.User(u.Name); ok {
			keep := me.Passkeys[:0]
			for _, k := range me.Passkeys {
				if k.ID != in.ID {
					keep = append(keep, k)
				}
			}
			me.Passkeys = keep
			if !me.TwoStep() {
				me.Recovery = nil
			}
		}
		return nil
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- signing in

// startSecondStep is called after the password was right, for someone with two-step sign-in: it remembers the sign-in
// for five minutes and shows the second step.
func (s *Server) startSecondStep(w http.ResponseWriter, r *http.Request, u state.User, ret string) {
	token := auth.NewToken()
	t := s.ts()
	t.mu.Lock()
	for k, p := range t.pending {
		if time.Now().After(p.expires) {
			delete(t.pending, k)
		}
	}
	t.pending[auth.TokenHash(token)] = &pendingSignIn{user: u.Name, ret: ret, host: host(r), expires: time.Now().Add(twoStepLife)}
	t.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: twoStepCookie, Value: token, Path: "/login", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(twoStepLife.Seconds())})
	s.renderSecondStep(w, u, "")
}

func (s *Server) renderSecondStep(w http.ResponseWriter, u state.User, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if msg != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	renderPage(w, "secondstep", pageData{Title: "Sign in", Main: s.settings().MainDomain, Error: msg, Name: u.Name,
		TOTP: u.TOTP != "", PasskeysHere: len(u.Passkeys) > 0})
}

// pendingOf is the sign-in waiting for its second step (with the lock held by the caller), or nil.
func (s *Server) pendingOf(r *http.Request) (string, *pendingSignIn) {
	c, err := r.Cookie(twoStepCookie)
	if err != nil || c.Value == "" {
		return "", nil
	}
	th := auth.TokenHash(c.Value)
	p := s.ts().pending[th]
	if p == nil || time.Now().After(p.expires) || p.host != host(r) {
		return "", nil
	}
	return th, p
}

func (s *Server) finishSignIn(w http.ResponseWriter, r *http.Request, user, ret string) {
	http.SetCookie(w, &http.Cookie{Name: twoStepCookie, Value: "", Path: "/login", HttpOnly: true, Secure: true, MaxAge: -1})
	if _, err := s.newSession(w, user, host(r), ""); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if strings.HasPrefix(ret, "/") && !strings.HasPrefix(ret, "//") {
		http.Redirect(w, r, ret, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// loginCode takes the second step: a code from the authenticator app, or a recovery code.
func (s *Server) loginCode(w http.ResponseWriter, r *http.Request) {
	if !s.onMain(w, r) {
		return
	}
	ip := clientIP(r)
	t := s.ts()
	t.mu.Lock()
	th, p := s.pendingOf(r)
	if p == nil {
		t.mu.Unlock()
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	p.tries++
	tooMany := p.tries > 5
	if tooMany {
		delete(t.pending, th)
	}
	user, ret := p.user, p.ret
	t.mu.Unlock()
	u := s.fresh(user)
	if tooMany || s.tooManyBadLogins(ip) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		renderPage(w, "login", pageData{Title: "Sign in", Main: s.settings().MainDomain, Error: "Too many wrong codes. Sign in again in a few minutes."})
		return
	}
	code := r.FormValue("code")
	ok := false
	if u.TOTP != "" {
		if step := totpStep(u.TOTP, code, time.Now()); step >= 0 {
			t.mu.Lock()
			if step > t.usedStep[u.Name] { // a code works once
				t.usedStep[u.Name] = step
				ok = true
			}
			t.mu.Unlock()
		}
	}
	if !ok && len(u.Recovery) > 0 {
		h := auth.TokenHash(normalizeRecovery(code))
		s.Store.Update(func(d *state.Data) error {
			me, found := d.User(u.Name)
			if !found {
				return nil
			}
			for i, x := range me.Recovery {
				if subtle.ConstantTimeCompare([]byte(x), []byte(h)) == 1 {
					me.Recovery = append(me.Recovery[:i:i], me.Recovery[i+1:]...)
					ok = true
					break
				}
			}
			return nil
		})
	}
	if !ok {
		s.noteBadLogin(ip)
		s.renderSecondStep(w, u, "That code didn't work. Type the newest code from your authenticator app, or a recovery code.")
		return
	}
	t.mu.Lock()
	delete(t.pending, th)
	t.mu.Unlock()
	s.finishSignIn(w, r, u.Name, ret)
}

// loginPasskeyBegin starts a passkey sign-in: as the second step after a password, or alone from the sign-in page.
func (s *Server) loginPasskeyBegin(w http.ResponseWriter, r *http.Request) {
	if !s.onMain(w, r) {
		return
	}
	rp, err := s.relyingParty(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t := s.ts()
	t.mu.Lock()
	_, p := s.pendingOf(r)
	t.mu.Unlock()
	if p != nil { // the second step: one of this person's passkeys
		u := s.fresh(p.user)
		assertion, session, err := rp.BeginLogin(passkeyUser{u}, webauthn.WithUserVerification(protocol.VerificationRequired))
		if err != nil {
			writeError(w, http.StatusBadRequest, errors.New("there's no passkey for this account at this address"))
			return
		}
		t.mu.Lock()
		p.webauthn = session
		t.mu.Unlock()
		writeJSON(w, http.StatusOK, assertion)
		return
	}
	assertion, session, err := rp.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	token := auth.NewToken()
	t.mu.Lock()
	t.passwordless[auth.TokenHash(token)] = session
	t.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: twoStepCookie, Value: token, Path: "/login", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
		MaxAge: int(twoStepLife.Seconds())})
	writeJSON(w, http.StatusOK, assertion)
}

func (s *Server) loginPasskeyFinish(w http.ResponseWriter, r *http.Request) {
	if !s.onMain(w, r) {
		return
	}
	ip := clientIP(r)
	if s.tooManyBadLogins(ip) {
		writeError(w, http.StatusTooManyRequests, errors.New("too many tries from this address; wait a few minutes"))
		return
	}
	rp, err := s.relyingParty(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t := s.ts()
	t.mu.Lock()
	th, p := s.pendingOf(r)
	var session *webauthn.SessionData
	ret := r.URL.Query().Get("return")
	if p != nil {
		session, ret = p.webauthn, p.ret
	} else if c, err := r.Cookie(twoStepCookie); err == nil {
		th = auth.TokenHash(c.Value)
		session = t.passwordless[th]
		delete(t.passwordless, th)
	}
	t.mu.Unlock()
	if session == nil {
		writeError(w, http.StatusBadRequest, errors.New("start again"))
		return
	}
	var who string
	var used []byte
	if p != nil {
		u := s.fresh(p.user)
		cred, err := rp.FinishLogin(passkeyUser{u}, *session, r)
		if err != nil {
			s.noteBadLogin(ip)
			writeError(w, http.StatusForbidden, errors.New("that passkey didn't work"))
			return
		}
		who, used = u.Name, cred.ID
		t.mu.Lock()
		delete(t.pending, th)
		t.mu.Unlock()
	} else {
		found, cred, err := rp.FinishPasskeyLogin(func(rawID, handle []byte) (webauthn.User, error) {
			var match *state.User
			s.Store.Read(func(d *state.Data) {
				for _, x := range d.Users() {
					if len(x.WebAuthnID) > 0 && subtle.ConstantTimeCompare(x.WebAuthnID, handle) == 1 {
						c := x
						match = &c
					}
				}
			})
			if match == nil {
				return nil, errors.New("no such account")
			}
			return passkeyUser{*match}, nil
		}, *session, r)
		if err != nil {
			s.noteBadLogin(ip)
			writeError(w, http.StatusForbidden, errors.New("that passkey didn't work"))
			return
		}
		who, used = found.(passkeyUser).u.Name, cred.ID
	}
	id := base64.RawURLEncoding.EncodeToString(used)
	s.Store.Update(func(d *state.Data) error {
		if me, ok := d.User(who); ok {
			for i := range me.Passkeys {
				if me.Passkeys[i].ID == id {
					me.Passkeys[i].LastUsed = time.Now()
				}
			}
		}
		return nil
	})
	http.SetCookie(w, &http.Cookie{Name: twoStepCookie, Value: "", Path: "/login", HttpOnly: true, Secure: true, MaxAge: -1})
	if _, err := s.newSession(w, who, host(r), ""); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !strings.HasPrefix(ret, "/") || strings.HasPrefix(ret, "//") {
		ret = "/"
	}
	writeJSON(w, http.StatusOK, map[string]string{"go": ret})
}
