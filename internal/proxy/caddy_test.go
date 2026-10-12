package proxy

import (
	"strings"
	"testing"

	"homeport/internal/state"
)

func TestCaddyfile(t *testing.T) {
	s := state.Settings{MainDomain: "home.example.com", ACMEEmail: "me@example.com", TLS: state.TLSLetsEncrypt}
	apps := []state.App{
		{ID: "cloud", Name: "Nextcloud", Domain: "cloud.example.com", Port: 18000, Access: state.Access{Public: true}},
		{ID: "kuma", Name: "Uptime Kuma", Domain: "up.example.com", Port: 18001, Access: state.Access{Groups: []string{"all"}}},
	}
	c := Caddyfile(s, apps)
	for _, want := range []string{
		"email me@example.com",
		"home.example.com {",
		"reverse_proxy 127.0.0.1:8090",
		"cloud.example.com {",
		"reverse_proxy 127.0.0.1:18000",
		"up.example.com {",
		"uri /auth/verify?app=kuma",
		"copy_headers Remote-User Remote-Name Remote-Email Remote-Groups",
		"reverse_proxy 127.0.0.1:18001",
	} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
	// The public app gets no sign-in check; the gated one does.
	cloud := c[strings.Index(c, "cloud.example.com {"):strings.Index(c, "up.example.com {")]
	if strings.Contains(cloud, "forward_auth") {
		t.Error("a public app must not be behind the sign-in check")
	}
	// Every site strips identity headers a browser might send.
	if n := strings.Count(c, "request_header -Remote-User"); n != 4 {
		t.Errorf("identity headers stripped on %d sites (main, AI page, two apps), want 4", n)
	}
	if !strings.Contains(Caddyfile(state.Settings{MainDomain: "a.test", TLS: state.TLSInternal}, nil), "a.test {\n\ttls internal\n") {
		t.Error("internal TLS should use Caddy's own certificates")
	}
}

func TestCertificatePerDomain(t *testing.T) {
	s := state.Settings{MainDomain: "home.example.org", TLS: state.TLSInternal}
	apps := []state.App{
		{ID: "a", Name: "A", Domain: "a.example.com", Port: 18000},
		{ID: "b", Name: "B", Domain: "b.example.com", Port: 18001},
		{ID: "c", Name: "C", Domain: "c.home.example.org", Port: 18002},
	}
	c := CaddyfileFor(s, apps, nil, map[string]SiteTLS{
		"a.example.com": {Mode: state.CertLetsEncrypt},
		"b.example.com": {Mode: state.CertCustom, CertFile: "/etc/caddy/homeport-certs/b.example.com/cert.pem", KeyFile: "/etc/caddy/homeport-certs/b.example.com/key.pem"},
	})
	site := func(d string) string {
		i := strings.Index(c, d+" {")
		j := strings.Index(c[i:], "\n}\n")
		return c[i : i+j]
	}
	if strings.Contains(site("a.example.com"), "tls ") {
		t.Errorf("Let's Encrypt for one domain on an internal server needs no tls line:\n%s", site("a.example.com"))
	}
	if !strings.Contains(site("b.example.com"), `tls "/etc/caddy/homeport-certs/b.example.com/cert.pem" "/etc/caddy/homeport-certs/b.example.com/key.pem"`) {
		t.Errorf("uploaded certificate not used:\n%s", site("b.example.com"))
	}
	for _, d := range []string{"home.example.org", "c.home.example.org"} {
		if !strings.Contains(site(d), "tls internal") {
			t.Errorf("%s should follow the server's setting:\n%s", d, site(d))
		}
	}
	if strings.Contains(c, "local_certs") {
		t.Error("local_certs would override the per-domain choice")
	}
	// A Let's Encrypt server: only the domain that asks gets Caddy's own certificate.
	s.TLS = state.TLSLetsEncrypt
	c = CaddyfileFor(s, apps, nil, map[string]SiteTLS{"a.example.com": {Mode: state.CertInternal}})
	if n := strings.Count(c, "tls internal"); n != 1 || !strings.Contains(site("a.example.com"), "tls internal") {
		t.Errorf("want tls internal on a.example.com only:\n%s", c)
	}
}

func TestLocalAddressesServeThePortal(t *testing.T) {
	c := CaddyfileWith(state.Settings{MainDomain: "home.test", TLS: state.TLSLetsEncrypt}, nil, []string{"192.168.4.27"})
	if !strings.Contains(c, "192.168.4.27 {\n\ttls internal\n") || strings.Count(c, "reverse_proxy 127.0.0.1:8090") != 3 {
		t.Fatalf("the portal should also answer on the machine's address, with Caddy's own certificate:\n%s", c)
	}
}

func TestTerminalRoute(t *testing.T) {
	s := state.Settings{MainDomain: "home.example.com", TLS: state.TLSInternal}
	if c := CaddyfileWith(s, nil, []string{"192.168.4.27"}); strings.Contains(c, "/terminal") {
		t.Errorf("terminal routed while off:\n%s", c)
	}
	s.Terminal = true
	c := CaddyfileWith(s, nil, []string{"192.168.4.27"})
	if n := strings.Count(c, "uri /auth/admin"); n != 2 {
		t.Errorf("want the admin check on the main domain and the IP site, got %d:\n%s", n, c)
	}
	for _, want := range []string{"redir /terminal /terminal/", "handle /terminal/* {", "reverse_proxy 127.0.0.1:7682"} {
		if !strings.Contains(c, want) {
			t.Errorf("missing %q in:\n%s", want, c)
		}
	}
}

func TestAIPage(t *testing.T) {
	s := state.Settings{MainDomain: "homeport.local", TLS: state.TLSInternal}
	c := Caddyfile(s, nil)
	if !strings.Contains(c, "ai.homeport.local {\n\ttls internal\n") {
		t.Fatalf("the AI page should have its own site, served by Homeport:\n%s", c)
	}
	// An app that has the name keeps it.
	c = Caddyfile(s, []state.App{{ID: "chat", Name: "Chat", Domain: "ai.homeport.local", Port: 18009}})
	if strings.Contains(c, "# The AI page") || !strings.Contains(c, "reverse_proxy 127.0.0.1:18009") {
		t.Fatalf("an app on ai.homeport.local should win:\n%s", c)
	}
}

func TestPrivateNamesGetCaddysOwnCertificate(t *testing.T) {
	s := state.Settings{MainDomain: "wh91.duckdns.org", TLS: state.TLSLetsEncrypt}
	apps := []state.App{
		{ID: "speed", Name: "Speed", Domain: "librespeed.wh91.duckdns.org", Port: 18000},
		{ID: "cloud", Name: "Cloud", Domain: "cloud.homeport.local", Port: 18001},
	}
	c := Caddyfile(s, apps)
	if strings.Contains(c, "wh91.duckdns.org {\n\ttls") || strings.Contains(c, "librespeed.wh91.duckdns.org {\n\ttls") {
		t.Errorf("public names follow the server's Let's Encrypt setting:\n%s", c)
	}
	if !strings.Contains(c, "cloud.homeport.local {\n\ttls internal") {
		t.Errorf(".local names can't have Let's Encrypt: Caddy's own, whatever the server's setting:\n%s", c)
	}
}
