package server

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"homeport/internal/state"
)

const ncRedirect = "https://cloud.example.test/apps/user_oidc/code"

func authorizeURL(client, redirect, challenge string) string {
	v := url.Values{"response_type": {"code"}, "client_id": {client}, "redirect_uri": {redirect},
		"scope": {"openid profile email groups"}, "state": {"st8"}, "nonce": {"n0nce"}}
	if challenge != "" {
		v.Set("code_challenge", challenge)
		v.Set("code_challenge_method", "S256")
	}
	return "https://home.test/oidc/authorize?" + v.Encode()
}

func (b *browser) tokenRequest(form url.Values, id, secret string) (int, map[string]any) {
	hdr := map[string]string{}
	if id != "" {
		hdr["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(id)+":"+url.QueryEscape(secret)))
	}
	w := b.do("POST", "https://home.test/oidc/token", form, hdr)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestOIDCSignInFlow(t *testing.T) {
	r := newRig(t)
	app := r.install("nextcloud", "cloud.example.test", nil)
	if app.OIDC == nil || app.OIDC.RedirectURIs[0] != ncRedirect || app.Secrets["oidc_client_secret"] == "" {
		t.Fatalf("installing an OIDC app registers it as a client: %+v", app)
	}
	secret := app.Secrets["oidc_client_secret"]
	_ = r.srv.Store.Update(func(d *state.Data) error { u, _ := d.User("bob"); u.Email = "bob@example.test"; return nil })
	bob := r.browser()

	// Discovery.
	var disc map[string]any
	_ = json.Unmarshal(bob.do("GET", "https://home.test/.well-known/openid-configuration", nil, nil).Body.Bytes(), &disc)
	if disc["issuer"] != "https://home.test" || disc["token_endpoint"] != "https://home.test/oidc/token" {
		t.Fatalf("discovery: %v", disc)
	}

	verifier := "a-long-random-pkce-verifier-0123456789-abcdefghij"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// Not signed in: off to the sign-in page, which comes back to authorize.
	w := bob.do("GET", authorizeURL("nextcloud", ncRedirect, challenge), nil, nil)
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "/login?return=%2Foidc%2Fauthorize") {
		t.Fatalf("signed out: want the login page, got %d %s", w.Code, w.Header().Get("Location"))
	}
	bob.do("POST", "https://home.test/login", url.Values{"name": {"bob"}, "password": {"battery staple"}}, nil)

	// An address the app didn't register is never redirected to.
	if w := bob.do("GET", authorizeURL("nextcloud", "https://evil.test/cb", challenge), nil, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("unregistered redirect: want 400, got %d %s", w.Code, w.Header().Get("Location"))
	}
	// An app without OIDC isn't a client.
	r.install("whoami", "who.example.test", nil)
	if w := bob.do("GET", authorizeURL("whoami", "https://who.example.test/cb", ""), nil, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("not a client: want 400, got %d", w.Code)
	}

	w = bob.do("GET", authorizeURL("nextcloud", ncRedirect, challenge), nil, nil)
	loc, _ := url.Parse(w.Header().Get("Location"))
	code := loc.Query().Get("code")
	if w.Code != http.StatusFound || !strings.HasPrefix(loc.String(), ncRedirect+"?") || code == "" || loc.Query().Get("state") != "st8" {
		t.Fatalf("want a code sent to the app, got %d %s", w.Code, loc)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {ncRedirect}, "code_verifier": {verifier}}

	if status, _ := bob.tokenRequest(form, "nextcloud", "wrong"); status != http.StatusUnauthorized {
		t.Fatalf("wrong client secret: want 401, got %d", status)
	}
	bad := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {ncRedirect}, "code_verifier": {"nope"}}
	if status, _ := bob.tokenRequest(bad, "nextcloud", secret); status != http.StatusBadRequest {
		t.Fatalf("wrong PKCE verifier: want 400, got %d", status)
	}
	// That failed attempt used the code up: get a fresh one.
	w = bob.do("GET", authorizeURL("nextcloud", ncRedirect, challenge), nil, nil)
	loc, _ = url.Parse(w.Header().Get("Location"))
	form.Set("code", loc.Query().Get("code"))

	status, tok := bob.tokenRequest(form, "nextcloud", secret)
	if status != http.StatusOK {
		t.Fatalf("token: %d %v", status, tok)
	}
	claims, err := r.srv.Signer.Verify(tok["id_token"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if claims["sub"] != "bob" || claims["aud"] != "nextcloud" || claims["nonce"] != "n0nce" || claims["iss"] != "https://home.test" ||
		claims["email"] != "bob@example.test" || claims["preferred_username"] != "bob" {
		t.Fatalf("id token claims: %v", claims)
	}
	if g, _ := claims["groups"].([]any); len(g) != 1 || g[0] != "family" {
		t.Fatalf("groups claim: %v", claims["groups"])
	}
	if status, _ := bob.tokenRequest(form, "nextcloud", secret); status != http.StatusBadRequest {
		t.Fatalf("a code works once: want 400, got %d", status)
	}

	// Userinfo with the access token.
	ui := bob.do("GET", "https://home.test/oidc/userinfo", nil, map[string]string{"Authorization": "Bearer " + tok["access_token"].(string)})
	if ui.Code != http.StatusOK || !strings.Contains(ui.Body.String(), `"sub":"bob"`) {
		t.Fatalf("userinfo: %d %s", ui.Code, ui.Body)
	}
	if ui := bob.do("GET", "https://home.test/oidc/userinfo", nil, map[string]string{"Authorization": "Bearer " + tok["id_token"].(string)}); ui.Code != http.StatusUnauthorized {
		t.Fatalf("an id token isn't an access token: want 401, got %d", ui.Code)
	}

	// Refresh: works once, and gives a new refresh token.
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tok["refresh_token"].(string)}}
	status, tok2 := bob.tokenRequest(refresh, "nextcloud", secret)
	if status != http.StatusOK || tok2["refresh_token"] == tok["refresh_token"] {
		t.Fatalf("refresh: %d %v", status, tok2)
	}
	if status, _ := bob.tokenRequest(refresh, "nextcloud", secret); status != http.StatusBadRequest {
		t.Fatalf("a refresh token works once: want 400, got %d", status)
	}

	// Taking Bob's access away stops both new sign-ins and refreshes.
	if err := r.srv.Apps.SetAccess(t.Context(), "nextcloud", state.Access{Groups: []string{"work"}}); err != nil {
		t.Fatal(err)
	}
	w = bob.do("GET", authorizeURL("nextcloud", ncRedirect, challenge), nil, nil)
	if loc, _ := url.Parse(w.Header().Get("Location")); loc.Query().Get("error") != "access_denied" {
		t.Fatalf("outside the app's groups: want access_denied, got %s", w.Header().Get("Location"))
	}
	refresh.Set("refresh_token", tok2["refresh_token"].(string))
	if status, _ := bob.tokenRequest(refresh, "nextcloud", secret); status != http.StatusBadRequest {
		t.Fatalf("a refresh after losing access: want 400, got %d", status)
	}
}
