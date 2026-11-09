package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"homeport/internal/auth"
	"homeport/internal/state"
)

// RFC 6238's own examples (SHA-1, the 20-byte key "12345678901234567890"), as 6-digit codes.
func TestTOTPVectors(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	for at, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037"} {
		if got := totpAt(secret, at/30); got != want {
			t.Errorf("at %d: %s, want %s", at, got, want)
		}
	}
	if step := totpStep(secret, "287082", time.Unix(59+30, 0)); step != 1 {
		t.Errorf("a code one step old: %d", step)
	}
	if step := totpStep(secret, "287082", time.Unix(59+90, 0)); step != -1 {
		t.Errorf("a code three steps old still worked")
	}
}

// jar is a browser that keeps every cookie.
type jar struct {
	r *rig
	c map[string]string
}

func (j *jar) do(method, path string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, "https://home.test"+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, "https://home.test"+path, nil)
	}
	for k, v := range j.c {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	j.r.h.ServeHTTP(w, req)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(j.c, c.Name)
		} else {
			j.c[c.Name] = c.Value
		}
	}
	return w
}

func (j *jar) json(method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "https://home.test"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Homeport", "1")
	for k, v := range j.c {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	j.r.h.ServeHTTP(w, req)
	return w
}

func (j *jar) signedIn() bool { return j.do("GET", "/api/me/portal", nil, nil).Code == http.StatusOK }

func TestTwoStepSignIn(t *testing.T) {
	r := newRig(t)
	login := func(pass string) *jar {
		j := &jar{r: r, c: map[string]string{}}
		j.do("POST", "/login", url.Values{"name": {"bob"}, "password": {pass}, "return": {"/admin"}}, nil)
		return j
	}
	me := login("battery staple")
	// setting it up: a code from the new secret, and the password
	var start struct{ Secret, URI, QR string }
	json.Unmarshal(me.json("POST", "/api/me/totp/start", "").Body.Bytes(), &start)
	if start.Secret == "" || !strings.HasPrefix(start.URI, "otpauth://totp/") || !strings.HasPrefix(start.QR, "data:image/png;base64,") {
		t.Fatalf("start: %+v", start)
	}
	code := totpAt(start.Secret, time.Now().Unix()/30)
	if w := me.json("POST", "/api/me/totp/confirm", `{"password":"wrong","code":"`+code+`"}`); w.Code != http.StatusForbidden {
		t.Errorf("turned on without the password: %d", w.Code)
	}
	if w := me.json("POST", "/api/me/totp/confirm", `{"password":"battery staple","code":"000000"}`); w.Code != http.StatusBadRequest {
		t.Errorf("turned on with a wrong code: %d", w.Code)
	}
	w := me.json("POST", "/api/me/totp/confirm", `{"password":"battery staple","code":"`+code+`"}`)
	var conf struct{ Recovery []string }
	json.Unmarshal(w.Body.Bytes(), &conf)
	if w.Code != http.StatusOK || len(conf.Recovery) != 10 {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	var sec securityView
	json.Unmarshal(me.json("GET", "/api/me/security", "").Body.Bytes(), &sec)
	if !sec.TOTP || sec.RecoveryLeft != 10 {
		t.Errorf("security: %+v", sec)
	}
	// the admin list shows it, and never the secret
	var users []map[string]any
	alice := &jar{r: r, c: map[string]string{}}
	alice.do("POST", "/login", url.Values{"name": {"alice"}, "password": {"correct horse"}}, nil)
	json.Unmarshal(alice.json("GET", "/api/users", "").Body.Bytes(), &users)
	for _, u := range users {
		if u["name"] == "bob" && u["twoStep"] != true {
			t.Errorf("the admin list: %v", u)
		}
		if _, leak := u["totp"]; leak {
			t.Error("the secret is in the admin list")
		}
	}

	// signing in: the password alone isn't enough
	j := login("battery staple")
	if j.signedIn() {
		t.Fatal("signed in with the password alone")
	}
	if page := j.do("GET", "/login", nil, nil); page.Code != http.StatusOK {
		t.Fatal(page.Code)
	}
	if w := j.do("POST", "/login/code", url.Values{"code": {"123456"}}, nil); w.Code != http.StatusUnauthorized || j.signedIn() {
		t.Errorf("a wrong code: %d", w.Code)
	}
	// the code used to turn it on can't be used again
	if w := j.do("POST", "/login/code", url.Values{"code": {code}}, nil); w.Code == http.StatusFound && j.signedIn() {
		t.Error("the same code worked twice")
	}
	// a recovery code works once (with or without its dash, any case)
	j = login("battery staple")
	rc := strings.ToUpper(strings.ReplaceAll(conf.Recovery[0], "-", ""))
	if w := j.do("POST", "/login/code", url.Values{"code": {rc}}, nil); w.Code != http.StatusFound || w.Header().Get("Location") != "/admin" || !j.signedIn() {
		t.Fatalf("a recovery code: %d %s", w.Code, w.Header().Get("Location"))
	}
	j = login("battery staple")
	if j.do("POST", "/login/code", url.Values{"code": {conf.Recovery[0]}}, nil); j.signedIn() {
		t.Error("a recovery code worked twice")
	}
	// the next code from the app works
	j = login("battery staple")
	r.srv.ts().mu.Lock()
	r.srv.ts().usedStep["bob"] = 0 // as if the code above were from long ago
	r.srv.ts().mu.Unlock()
	if j.do("POST", "/login/code", url.Values{"code": {totpAt(start.Secret, time.Now().Unix()/30)}}, nil); !j.signedIn() {
		t.Error("the authenticator code didn't sign in")
	}
	// the second step can't be skipped: no pending sign-in, no session
	stranger := &jar{r: r, c: map[string]string{}}
	if w := stranger.do("POST", "/login/code", url.Values{"code": {"000000"}}, nil); w.Code != http.StatusFound || stranger.signedIn() {
		t.Errorf("a code without a password: %d", w.Code)
	}
	// guessing codes: after five, start over
	j = login("battery staple")
	for i := 0; i < 6; i++ {
		w = j.do("POST", "/login/code", url.Values{"code": {"111111"}}, nil)
	}
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("the sixth wrong code: %d", w.Code)
	}

	// an admin turns it off for someone who lost their phone
	if w := alice.json("PATCH", "/api/users/bob", `{"resetTwoStep":true}`); w.Code != http.StatusOK {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	var bob state.User
	r.srv.Store.Read(func(d *state.Data) { u, _ := d.User("bob"); bob = *u })
	if bob.TwoStep() || len(bob.Recovery) != 0 {
		t.Errorf("still on: %+v", bob)
	}
	r.srv.mu.Lock()
	r.srv.bad = nil // the wrong codes above locked this address out for a while
	r.srv.mu.Unlock()
	if !login("battery staple").signedIn() {
		t.Error("the password alone doesn't work after the reset")
	}
}

func TestPasskeyStartsOnlyByName(t *testing.T) {
	r := newRig(t)
	j := &jar{r: r, c: map[string]string{}}
	j.do("POST", "/login", url.Values{"name": {"bob"}, "password": {"battery staple"}}, nil)
	if w := j.json("POST", "/api/me/passkeys/begin", `{"password":"wrong"}`); w.Code != http.StatusForbidden {
		t.Errorf("began without the password: %d", w.Code)
	}
	w := j.json("POST", "/api/me/passkeys/begin", `{"password":"battery staple"}`)
	var opts struct {
		PublicKey struct {
			RP                     struct{ ID string } `json:"rp"`
			AuthenticatorSelection struct {
				ResidentKey, UserVerification string
			} `json:"authenticatorSelection"`
		} `json:"publicKey"`
	}
	json.Unmarshal(w.Body.Bytes(), &opts)
	if w.Code != http.StatusOK || opts.PublicKey.RP.ID != "home.test" || opts.PublicKey.AuthenticatorSelection.UserVerification != "required" {
		t.Errorf("passkey options: %d %s", w.Code, w.Body)
	}
	// a passkey sign-in from the sign-in page starts without a password (it's the passkey that proves it)
	anon := &jar{r: r, c: map[string]string{}}
	if w := anon.do("POST", "/login/passkey/begin", nil, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"challenge"`) {
		t.Errorf("passwordless begin: %d %s", w.Code, w.Body)
	}
	if w := anon.json("POST", "/login/passkey/finish", `{"id":"x","rawId":"eA","type":"public-key","response":{}}`); w.Code == http.StatusOK || anon.signedIn() {
		t.Errorf("a made-up passkey signed in: %d", w.Code)
	}
	_ = auth.TokenHash
}
