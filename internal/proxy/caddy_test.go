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
	if n := strings.Count(c, "request_header -Remote-User"); n != 3 {
		t.Errorf("identity headers stripped on %d sites, want 3", n)
	}
	if !strings.Contains(Caddyfile(state.Settings{MainDomain: "a.test", TLS: state.TLSInternal}, nil), "local_certs") {
		t.Error("internal TLS should use Caddy's own certificates")
	}
}

func TestLocalAddressesServeThePortal(t *testing.T) {
	c := CaddyfileWith(state.Settings{MainDomain: "home.test", TLS: state.TLSLetsEncrypt}, nil, []string{"192.168.4.27"})
	if !strings.Contains(c, "192.168.4.27 {\n\ttls internal\n") || strings.Count(c, "reverse_proxy 127.0.0.1:8090") != 2 {
		t.Fatalf("the portal should also answer on the machine's address, with Caddy's own certificate:\n%s", c)
	}
}
