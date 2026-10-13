// Package proxy writes Caddy's configuration: the main domain goes to Homeport (portal and admin panel), each app's
// domain goes to the app, behind Homeport's sign-in check unless the app is public.
package proxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"homeport/internal/runner"
	"homeport/internal/state"
	"homeport/internal/terminal"
)

// Upstream is where Caddy reaches Homeport itself.
const Upstream = "127.0.0.1:8090"

// The headers Homeport sets for signed-in users. Anything a browser sends with these names is removed first, so an app
// that trusts them can't be fooled.
var identityHeaders = []string{"Remote-User", "Remote-Name", "Remote-Email", "Remote-Groups"}

// Caddyfile builds the whole configuration.
func Caddyfile(s state.Settings, apps []state.App) string { return CaddyfileWith(s, apps, nil) }

// CaddyfileWith also serves the portal and admin panel on this machine's own addresses (localAddrs), with Caddy's
// own certificates, so they can be reached by IP before DNS is set up.
func CaddyfileWith(s state.Settings, apps []state.App, localAddrs []string) string {
	return CaddyfileFor(s, apps, localAddrs, nil)
}

// SiteTLS is one domain's certificate, when it doesn't follow the server's setting.
type SiteTLS struct {
	Mode              state.CertMode
	CertFile, KeyFile string // an uploaded certificate (Mode custom)
}

// CaddyfileFor also gives domains their own certificates (by domain name).
func CaddyfileFor(s state.Settings, apps []state.App, localAddrs []string, certs map[string]SiteTLS) string {
	var b strings.Builder
	b.WriteString("# Written by Homeport. Changes here are replaced: use `homeport` or the admin panel instead.\n{\n\tadmin localhost:2019\n")
	// Other ports, for a test instance beside something else that holds 80 and 443.
	if p := os.Getenv("HOMEPORT_HTTP_PORT"); p != "" {
		fmt.Fprintf(&b, "\thttp_port %s\n", p)
	}
	if p := os.Getenv("HOMEPORT_HTTPS_PORT"); p != "" {
		fmt.Fprintf(&b, "\thttps_port %s\n", p)
	}
	// With Caddy's own certificates for the server, each site says so itself (rather than local_certs for all), so
	// one domain can still have Let's Encrypt.
	if s.ACMEEmail != "" {
		fmt.Fprintf(&b, "\temail %s\n", s.ACMEEmail)
	}
	b.WriteString("}\n")

	if s.MainDomain != "" {
		fmt.Fprintf(&b, "\n%s {\n", s.MainDomain)
		real := writeTLS(&b, s, s.MainDomain, certs[s.MainDomain])
		writeHomeport(&b, s, real)
		if len(localAddrs) > 0 {
			fmt.Fprintf(&b, "\n# This machine's own addresses: the portal and admin panel, by IP\n%s {\n\ttls internal\n", strings.Join(localAddrs, ", "))
			writeHomeport(&b, s, false)
		}
		// The AI page (Homeport serves it and checks who's signed in), unless an app has its name.
		if ai := AIDomain(s.MainDomain, apps); ai != "" {
			fmt.Fprintf(&b, "\n# The AI page\n%s {\n", ai)
			real := writeTLS(&b, s, ai, certs[ai])
			writeStripIdentity(&b, real)
			fmt.Fprintf(&b, "\treverse_proxy %s\n}\n", Upstream)
		}
	}

	for _, a := range apps {
		if a.Domain == "" || a.Domain == s.MainDomain {
			continue
		}
		fmt.Fprintf(&b, "\n# %s (%s)\n%s {\n", a.Name, a.ID, a.Domain)
		real := writeTLS(&b, s, a.Domain, certs[a.Domain])
		writeStripIdentity(&b, real)
		// Homeport answers its own sign-in hand-off on every app domain, so sign-in works across unrelated domains.
		fmt.Fprintf(&b, "\thandle /.homeport/* {\n\t\treverse_proxy %s\n\t}\n", Upstream)
		b.WriteString("\thandle {\n")
		if !a.Access.Public {
			fmt.Fprintf(&b, "\t\tforward_auth %s {\n\t\t\turi /auth/verify?app=%s\n\t\t\tcopy_headers %s\n\t\t}\n",
				Upstream, a.ID, strings.Join(identityHeaders, " "))
		}
		fmt.Fprintf(&b, "\t\treverse_proxy 127.0.0.1:%d\n\t}\n}\n", a.Port)
	}
	return b.String()
}

// AIDomain is the AI page's name (ai.<main domain>), or "" when an app has it.
func AIDomain(main string, apps []state.App) string {
	name := "ai." + main
	for _, a := range apps {
		if a.Domain == name {
			return ""
		}
	}
	return name
}

// writeTLS says where a site's certificate comes from; Let's Encrypt is Caddy's own default, so it needs nothing.
// It says whether browsers trust that certificate (not Caddy's own).
func writeTLS(b *strings.Builder, s state.Settings, domain string, t SiteTLS) bool {
	switch state.CertFor(s, domain, t.Mode) {
	case state.CertInternal:
		b.WriteString("\ttls internal\n")
		return false
	case state.CertCustom:
		fmt.Fprintf(b, "\ttls %q %q\n", t.CertFile, t.KeyFile)
	}
	return true
}

// writeHomeport finishes a site for the portal and admin panel, with the web terminal when it's on (admins only; Homeport
// checks before Caddy lets anyone through to it).
func writeHomeport(b *strings.Builder, s state.Settings, hsts bool) {
	writeStripIdentity(b, hsts)
	if s.Terminal {
		fmt.Fprintf(b, "\tredir %s %s/\n\thandle %s/* {\n\t\tforward_auth %s {\n\t\t\turi /auth/admin\n\t\t}\n\t\treverse_proxy 127.0.0.1:%d\n\t}\n",
			terminal.Path, terminal.Path, terminal.Path, Upstream, terminal.Port)
		fmt.Fprintf(b, "\thandle {\n\t\treverse_proxy %s\n\t}\n}\n", Upstream)
		return
	}
	fmt.Fprintf(b, "\treverse_proxy %s\n}\n", Upstream)
}

// writeStripIdentity removes identity headers a browser might send, and with a certificate browsers trust (hsts), tells
// them to only ever use HTTPS there (unless the app sets its own policy). Never with Caddy's own certificates: a browser
// that remembered it would refuse, with no way past, the next certificate for that name (a reinstall, or another
// server taking homeport.local).
func writeStripIdentity(b *strings.Builder, hsts bool) {
	for _, h := range identityHeaders {
		fmt.Fprintf(b, "\trequest_header -%s\n", h)
	}
	if hsts {
		b.WriteString("\theader ?Strict-Transport-Security \"max-age=31536000\"\n")
	}
}

// Caddy owns /etc/caddy/Caddyfile; Homeport replaces it and reloads Caddy.
type Caddy struct {
	Path   string // /etc/caddy/Caddyfile on Linux
	Runner runner.Runner
	// Reload makes Caddy load the new file (caddy reload, straight to Caddy's admin address).
	Reload []string
	// ReloadFallback is tried when Reload fails.
	ReloadFallback []string
}

// Apply checks the new configuration with Caddy, puts it in place and reloads. A configuration Caddy rejects never
// replaces a working one.
func (c Caddy) Apply(ctx context.Context, config string) error {
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return err
	}
	tmp := c.Path + ".homeport-new"
	if err := os.WriteFile(tmp, []byte(config), 0o644); err != nil {
		return err
	}
	defer os.Remove(tmp)
	// Caddy runs as its own user; Homeport's strict umask would otherwise leave the file readable only by root.
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	if out, err := c.Runner.Output(ctx, "", "caddy", "validate", "--config", tmp, "--adapter", "caddyfile"); err != nil {
		return fmt.Errorf("Caddy rejected the new configuration: %v\n%s", err, lastLines(out, 8))
	}
	if err := os.Rename(tmp, c.Path); err != nil {
		return err
	}
	reload := c.Reload
	if len(reload) == 0 {
		reload = []string{"systemctl", "reload", "caddy"}
	}
	out, err := c.Runner.Output(ctx, "", reload[0], reload[1:]...)
	if err != nil && len(c.ReloadFallback) > 0 {
		if _, err2 := c.Runner.Output(ctx, "", c.ReloadFallback[0], c.ReloadFallback[1:]...); err2 == nil {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("couldn't reload Caddy: %v\n%s", err, lastLines(out, 8))
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
