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
)

// Upstream is where Caddy reaches Homeport itself.
const Upstream = "127.0.0.1:8090"

// The headers Homeport sets for signed-in users. Anything a browser sends with these names is removed first, so an app
// that trusts them can't be fooled.
var identityHeaders = []string{"Remote-User", "Remote-Name", "Remote-Email", "Remote-Groups"}

// Caddyfile builds the whole configuration.
func Caddyfile(s state.Settings, apps []state.App) string {
	var b strings.Builder
	b.WriteString("# Written by Homeport. Changes here are replaced: use `homeport` or the admin panel instead.\n{\n\tadmin localhost:2019\n")
	switch {
	case s.TLS == state.TLSInternal:
		b.WriteString("\tlocal_certs\n")
	case s.ACMEEmail != "":
		fmt.Fprintf(&b, "\temail %s\n", s.ACMEEmail)
	}
	b.WriteString("}\n")

	if s.MainDomain != "" {
		fmt.Fprintf(&b, "\n%s {\n", s.MainDomain)
		writeStripIdentity(&b)
		fmt.Fprintf(&b, "\treverse_proxy %s\n}\n", Upstream)
	}

	for _, a := range apps {
		if a.Domain == "" || a.Domain == s.MainDomain {
			continue
		}
		fmt.Fprintf(&b, "\n# %s (%s)\n%s {\n", a.Name, a.ID, a.Domain)
		writeStripIdentity(&b)
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

func writeStripIdentity(b *strings.Builder) {
	for _, h := range identityHeaders {
		fmt.Fprintf(b, "\trequest_header -%s\n", h)
	}
	// Browsers should only ever use HTTPS here (unless the app sets its own policy).
	b.WriteString("\theader ?Strict-Transport-Security \"max-age=31536000\"\n")
}

// Caddy owns /etc/caddy/Caddyfile; Homeport replaces it and reloads Caddy.
type Caddy struct {
	Path   string // /etc/caddy/Caddyfile
	Runner runner.Runner
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
	if out, err := c.Runner.Output(ctx, "", "systemctl", "reload", "caddy"); err != nil {
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
