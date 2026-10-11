package assist

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"homeport/internal/apps"
	"homeport/internal/auth"
	"homeport/internal/catalog"
	"homeport/internal/state"
)

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func strList(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// define is every tool, grouped by level.
func (a *Assistant) define() []Tool {
	appID := str("The installed app's id, as list_apps shows it (e.g. \"nextcloud\").")
	return []Tool{
		// MARK: Look

		{Name: "server_overview", Level: Look,
			Description: "What this Homeport server is: main domain, certificate mode, Homeport version, and how many users, domains and apps it has, plus uptime, load, memory and disk. Start here.",
			Run: func(ctx context.Context, _ Args) (string, error) {
				var b strings.Builder
				a.Store.Read(func(d *state.Data) {
					s := d.Settings()
					fmt.Fprintf(&b, "Homeport %s on %s (certificates: %s; shell tools: %v)\n%d users, %d domains, %d apps\n\n",
						a.Version, s.MainDomain, s.TLS, s.AssistantShell, len(d.Users()), len(d.Domains()), len(d.Apps()))
				})
				health := a.Platform.Health
				if len(health) > 3 {
					health = health[:3]
				}
				for _, c := range health {
					out, _ := a.Runner.Output(ctx, "", c[0], c[1:]...)
					fmt.Fprintf(&b, "$ %s\n%s\n", strings.Join(c, " "), strings.TrimSpace(out))
				}
				return b.String(), nil
			}},

		{Name: "list_apps", Level: Look,
			Description: "Installed apps: id, name, address, who may open them, and whether their containers are running.",
			Run: func(ctx context.Context, _ Args) (string, error) {
				var list []state.App
				a.Store.Read(func(d *state.Data) { list = d.Apps() })
				if len(list) == 0 {
					return "No apps are installed.", nil
				}
				var b strings.Builder
				for _, x := range list {
					fmt.Fprintf(&b, "- %s (%s): https://%s, open to %s, %s, installed %s from catalog %q%s\n", x.ID, x.Name, x.Domain,
						accessText(x.Access), a.Apps.Status(ctx, x.ID), x.Installed.Format("2006-01-02"), x.Catalog, oidcText(x))
				}
				return b.String(), nil
			}},

		{Name: "app_details", Level: Look, Required: []string{"id"},
			Params:      map[string]any{"id": appID},
			Description: "One app in depth: its settings, each container's state, and its compose.yaml (secrets hidden).",
			Run: func(ctx context.Context, args Args) (string, error) {
				x, err := a.app(args.Str("id"))
				if err != nil {
					return "", err
				}
				ps, _ := a.Apps.ComposeOutput(ctx, x.ID, "ps", "--all", "--format", "table {{.Service}}\t{{.State}}\t{{.Status}}\t{{.Image}}")
				compose, _ := os.ReadFile(a.Apps.Dir(x.ID) + "/compose.yaml")
				return fmt.Sprintf("%s (%s) at https://%s, local port %d, open to %s%s\nFolder: %s\n\nContainers:\n%s\ncompose.yaml:\n%s",
					x.Name, x.ID, x.Domain, x.Port, accessText(x.Access), oidcText(x), a.Apps.Dir(x.ID), strings.TrimSpace(ps), compose), nil
			}},

		{Name: "app_logs", Level: Look, Required: []string{"id"},
			Params: map[string]any{"id": appID,
				"service": str("One container's service name (from app_details), or leave out for all."),
				"lines":   num("How many recent lines per container (default 200, at most 2000)."),
				"since":   str("Only lines newer than this, like \"30m\", \"2h\" or an RFC 3339 time.")},
			Description: "Recent log lines from an app's containers, the first place to look when an app misbehaves.",
			Run: func(ctx context.Context, args Args) (string, error) {
				x, err := a.app(args.Str("id"))
				if err != nil {
					return "", err
				}
				cmd := []string{"logs", "--no-color", "--timestamps", "--tail", fmt.Sprint(args.Int("lines", 200, 2000))}
				if s := args.Str("since"); s != "" {
					cmd = append(cmd, "--since", s)
				}
				if s := args.Str("service"); s != "" {
					cmd = append(cmd, s)
				}
				return a.Apps.ComposeOutput(ctx, x.ID, cmd...)
			}},

		{Name: "service_logs", Level: Look, Required: []string{"service"},
			Params: map[string]any{"service": map[string]any{"type": "string", "enum": []string{"homeport", "caddy", "docker"},
				"description": "Which system service."}, "lines": num("How many recent lines (default 150, at most 2000).")},
			Description: "Recent log lines from Homeport itself, Caddy (the web front door: certificates, proxying) or Docker.",
			Run: func(ctx context.Context, args Args) (string, error) {
				svc := args.Str("service")
				if svc != "homeport" && svc != "caddy" && svc != "docker" {
					return "", errors.New("service is homeport, caddy or docker")
				}
				if a.Platform.LogsCommand == nil {
					return "", errors.New("service logs aren't available on this system")
				}
				c := a.Platform.LogsCommand(svc, args.Int("lines", 150, 2000))
				return a.Runner.Output(ctx, "", c[0], c[1:]...)
			}},

		{Name: "system_health", Level: Look,
			Description: "The server's health: load, memory, disk, Docker's disk use, and any failed system services.",
			Run: func(ctx context.Context, _ Args) (string, error) {
				var b strings.Builder
				for _, c := range a.Platform.Health {
					out, err := a.Runner.Output(ctx, "", c[0], c[1:]...)
					if err != nil && out == "" {
						out = err.Error()
					}
					if strings.TrimSpace(out) == "" {
						out = "(nothing)"
					}
					fmt.Fprintf(&b, "$ %s\n%s\n\n", strings.Join(c, " "), strings.TrimSpace(out))
				}
				return b.String(), nil
			}},

		{Name: "list_catalog", Level: Look,
			Description: "Apps Homeport can install, with what they are and how they sign people in.",
			Run: func(context.Context, Args) (string, error) {
				all, err := catalog.All()
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, c := range all {
					fmt.Fprintf(&b, "- %s (%s, %s): %s Sign-in: %s.\n", c.ID, c.Name, c.Category, c.Description, c.SignIn)
				}
				return b.String(), nil
			}},

		{Name: "list_users", Level: Look,
			Description: "Homeport's users: name, full name, email, groups, and whether they're admins. (Passwords are never shown.)",
			Run: func(context.Context, Args) (string, error) {
				var b strings.Builder
				a.Store.Read(func(d *state.Data) {
					for _, u := range d.Users() {
						fmt.Fprintf(&b, "- %s (%s) email %q groups %v admin %v\n", u.Name, u.DisplayName, u.Email, u.Groups, u.Admin)
					}
				})
				return b.String(), nil
			}},

		{Name: "list_domains", Level: Look,
			Description: "Domains this server knows, and what each one serves.",
			Run: func(context.Context, Args) (string, error) {
				var b strings.Builder
				a.Store.Read(func(d *state.Data) {
					for _, x := range d.Domains() {
						use := "nothing yet"
						if x.Name == d.Settings().MainDomain {
							use = "portal and admin panel"
						} else if app, ok := d.AppOn(x.Name); ok {
							use = app.Name
						}
						fmt.Fprintf(&b, "- %s: %s\n", x.Name, use)
					}
				})
				return b.String(), nil
			}},

		{Name: "proxy_config", Level: Look,
			Description: "Caddy's current configuration (written by Homeport) and whether Caddy accepts it.",
			Run: func(ctx context.Context, _ Args) (string, error) {
				cfg, err := os.ReadFile(a.Caddyfile)
				if err != nil {
					return "", err
				}
				v, verr := a.Runner.Output(ctx, "", "caddy", "validate", "--config", a.Caddyfile, "--adapter", "caddyfile")
				verdict := "Caddy accepts it."
				if verr != nil {
					verdict = "Caddy rejects it:\n" + lastLines(v, 10)
				}
				return fmt.Sprintf("%s\n\n%s", cfg, verdict), nil
			}},

		{Name: "check_domain", Level: Look, Required: []string{"domain"},
			Params:      map[string]any{"domain": str("The domain to check, e.g. cloud.example.com.")},
			Description: "Whether a domain works from outside: what its DNS points to compared with this server, and what this server's HTTPS answers for it (status and certificate).",
			Run: func(ctx context.Context, args Args) (string, error) {
				domain := strings.ToLower(args.Str("domain"))
				if err := auth.ValidDomain(domain); err != nil {
					return "", err
				}
				return checkDomain(ctx, domain), nil
			}},

		// MARK: Memory (notes only: no approval needed, but logged like everything else)

		{Name: "read_memory", Level: Look,
			Description: "Read the shared memory FIRST, before anything else: work left in progress by the previous agent, notes about the server and each app (what was set up or fixed, quirks, the admin's preferences), and the recent changes made through these tools.",
			Run: func(ctx context.Context, _ Args) (string, error) {
				mem, err := a.Memory.Read()
				if err != nil {
					return "", err
				}
				return mem + "\n## Recent changes through these tools (from Homeport's audit log)\n" + a.recentChanges(25), nil
			}},

		{Name: "remember", Level: Look, Required: []string{"topic", "note"},
			Params: map[string]any{"topic": str("What it's about: \"server\", \"app: <id>\", \"domain: <name>\", \"admin preferences\"…"),
				"note": str("What was done or learned, in a sentence or two: the fact, why, and anything the next agent must know.")},
			Description: "Add a note to the shared memory, so later agents know what was done to what. Note every change you make and anything you learn about an app's setup or quirks. Never write secrets.",
			Run: func(ctx context.Context, args Args) (string, error) {
				return a.Memory.Remember(args.Str("topic"), Redact(a.Store, args.Str("note")), caller(ctx))
			}},

		{Name: "set_handoff", Level: Look, Required: []string{"text"},
			Params:      map[string]any{"text": str("The task, what's been done so far, what's left, and the next step. Empty when the work is finished.")},
			Description: "Record unfinished work so the next agent can pick it up (it's shown first in read_memory). Set it before stopping mid-task or waiting on an admin; clear it with empty text when done.",
			Run: func(ctx context.Context, args Args) (string, error) {
				return a.Memory.SetHandoff(Redact(a.Store, args.Str("text")), caller(ctx))
			}},

		{Name: "rewrite_memory_topic", Level: Look, Required: []string{"topic", "content"},
			Params: map[string]any{"topic": str("The topic to rewrite, exactly as read_memory shows it."),
				"content": str("Its new, condensed notes (keep every fact still true). Empty removes the topic.")},
			Description: "Condense one topic of the shared memory when it gets long or out of date.",
			Run: func(ctx context.Context, args Args) (string, error) {
				return a.Memory.RewriteTopic(args.Str("topic"), Redact(a.Store, args.Str("content")))
			}},

		// MARK: Change

		{Name: "install_app", Level: Change, Required: []string{"catalog", "domain"},
			Params: map[string]any{"catalog": str("The catalog id (from list_catalog)."),
				"domain": str("Where to serve it, e.g. cloud.example.com. Its DNS must point at this server."),
				"public": boolean("Anyone may open it, with no Homeport sign-in in front (default: by the app's sign-in style)."),
				"groups": strList("Or only these groups (\"all\" = every user).")},
			Summary: func(x Args) string {
				return fmt.Sprintf("Install %s at https://%s (open to %s)", x.Str("catalog"), x.Str("domain"), accessText(accessFrom(x)))
			},
			Description: "Install an app from the catalog on a domain. Needs an admin's approval. Takes a few minutes; returns the install log.",
			Run: func(ctx context.Context, args Args) (string, error) {
				req := apps.InstallRequest{Catalog: args.Str("catalog"), Domain: args.Str("domain")}
				if args.Bool("public") || len(args.Strs("groups")) > 0 {
					acc := accessFrom(args)
					req.Access = &acc
				}
				var log bytes.Buffer
				_, err := a.Apps.Install(ctx, req, a.firstAdmin(), &log)
				return log.String(), err
			}},

		{Name: "update_app", Level: Change, Required: []string{"id"},
			Params: map[string]any{"id": appID},
			Summary: func(x Args) string {
				return "Update " + x.Str("id") + " to its newest images and restart it (its data stays)"
			},
			Description: "Download an app's newest images and restart it with them; its data stays. Needs approval.",
			Run: func(ctx context.Context, args Args) (string, error) {
				var log bytes.Buffer
				err := a.Apps.Update(ctx, args.Str("id"), &log)
				return log.String(), err
			}},

		{Name: "restart_app", Level: Change, Required: []string{"id"},
			Params:      map[string]any{"id": appID, "service": str("Only this container's service (leave out for all).")},
			Summary:     func(x Args) string { return strings.TrimSpace("Restart " + x.Str("id") + " " + x.Str("service")) },
			Description: "Restart an app's containers (or one of them). Needs approval.",
			Run: func(ctx context.Context, args Args) (string, error) {
				if _, err := a.app(args.Str("id")); err != nil {
					return "", err
				}
				cmd := []string{"restart"}
				if s := args.Str("service"); s != "" {
					cmd = append(cmd, s)
				}
				return a.Apps.ComposeOutput(ctx, args.Str("id"), cmd...)
			}},

		{Name: "remove_app", Level: Change, Required: []string{"id"},
			Params: map[string]any{"id": appID, "delete_data": boolean("Also delete its data (files, database). Can't be undone.")},
			Summary: func(x Args) string {
				if x.Bool("delete_data") {
					return "Remove " + x.Str("id") + " AND DELETE ALL ITS DATA (can't be undone)"
				}
				return "Remove " + x.Str("id") + " (its data folder is kept)"
			},
			Description: "Stop an app and take it off its domain, keeping its data unless delete_data is true. Needs approval.",
			Run: func(ctx context.Context, args Args) (string, error) {
				var log bytes.Buffer
				err := a.Apps.Remove(ctx, args.Str("id"), args.Bool("delete_data"), &log)
				return log.String(), err
			}},

		{Name: "set_app_access", Level: Change, Required: []string{"id"},
			Params: map[string]any{"id": appID, "public": boolean("Anyone may open it."),
				"groups": strList("Or only these groups (\"all\" = every user).")},
			Summary:     func(x Args) string { return fmt.Sprintf("Make %s open to %s", x.Str("id"), accessText(accessFrom(x))) },
			Description: "Change who may open an app: public, or only some groups. Needs approval.",
			Run: func(ctx context.Context, args Args) (string, error) {
				return "", a.Apps.SetAccess(ctx, args.Str("id"), accessFrom(args))
			}},

		{Name: "add_domain", Level: Change, Required: []string{"domain"},
			Params:      map[string]any{"domain": str("The domain, e.g. example.com.")},
			Summary:     func(x Args) string { return "Add the domain " + x.Str("domain") },
			Description: "Add a domain so apps can be installed on it (or its subdomains). Needs approval.",
			Run: func(ctx context.Context, args Args) (string, error) {
				name := strings.ToLower(args.Str("domain"))
				if err := auth.ValidDomain(name); err != nil {
					return "", err
				}
				return "Added " + name, a.Store.Update(func(d *state.Data) error {
					if d.HasDomain(name) {
						return fmt.Errorf("%s is already added", name)
					}
					d.AddDomain(state.Domain{Name: name, Added: time.Now()})
					return nil
				})
			}},

		{Name: "set_user_groups", Level: Change, Required: []string{"name", "groups"},
			Params:      map[string]any{"name": str("The user name."), "groups": strList("Their groups (replaces the current ones).")},
			Summary:     func(x Args) string { return fmt.Sprintf("Put %s in the groups %v", x.Str("name"), x.Strs("groups")) },
			Description: "Set a user's groups (which decide what apps they can open). Needs approval.",
			Run: func(ctx context.Context, args Args) (string, error) {
				groups := args.Strs("groups")
				for _, g := range groups {
					if err := auth.ValidGroup(g); err != nil || g == "all" || g == "admins" {
						return "", fmt.Errorf("%q can't be used as a group", g)
					}
				}
				return "Done.", a.Store.Update(func(d *state.Data) error {
					u, ok := d.User(args.Str("name"))
					if !ok {
						return state.ErrNotFound
					}
					u.Groups = groups
					return nil
				})
			}},

		{Name: "run_in_app", Level: Change, Required: []string{"id", "service", "command"},
			Params: map[string]any{"id": appID, "service": str("The container's service name (from app_details)."),
				"command": strList("The command and its arguments, e.g. [\"php\",\"occ\",\"status\"]."),
				"user":    str("Run as this user inside the container (e.g. www-data for Nextcloud).")},
			Summary: func(x Args) string {
				return fmt.Sprintf("Run in %s/%s: %s", x.Str("id"), x.Str("service"), strings.Join(x.Strs("command"), " "))
			},
			Description: "Run a command inside one of an app's containers (an app's own admin tool, like Nextcloud's occ). Needs approval.",
			Run: func(ctx context.Context, args Args) (string, error) {
				if _, err := a.app(args.Str("id")); err != nil {
					return "", err
				}
				cmd := []string{"exec", "-T"}
				if u := args.Str("user"); u != "" {
					cmd = append(cmd, "--user", u)
				}
				cmd = append(append(cmd, args.Str("service")), args.Strs("command")...)
				return a.Apps.ComposeOutput(ctx, args.Str("id"), cmd...)
			}},

		// MARK: Shell

		{Name: "run_shell", Level: Shell, Required: []string{"command"},
			Params:      map[string]any{"command": str("A bash command line, run as root on the server.")},
			Summary:     func(x Args) string { return "Run as root: " + x.Str("command") },
			Description: "Run a shell command as root on the server. Only when the other tools can't do it; needs approval; stops after 2 minutes.",
			Run: func(ctx context.Context, args Args) (string, error) {
				ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
				defer cancel()
				return a.Runner.Output(ctx, "/", "bash", "-c", args.Str("command"))
			}},
	}
}

// recentChanges lists the latest changes made through the tools, newest first: Homeport's own record of what was
// done, which the agents can't forget to write.
func (a *Assistant) recentChanges(n int) string {
	entries, err := a.Audit.Recent(2000)
	if err != nil {
		return "(the audit log couldn't be read: " + err.Error() + ")\n"
	}
	var b strings.Builder
	count := 0
	for _, e := range entries {
		if e.Level == Look || e.Level == "" {
			continue
		}
		outcome := e.Decision
		if e.Decision == "approved" {
			outcome = "done"
			if !e.OK {
				outcome = "failed: " + clip(e.Error, 160)
			}
		}
		fmt.Fprintf(&b, "- %s %s %s %s: %s", e.Time.Format("2006-01-02 15:04"), e.Client, e.Tool, string(e.Args), outcome)
		if e.By != "" {
			fmt.Fprintf(&b, " (approved by %s)", e.By)
		}
		b.WriteString("\n")
		if count++; count == n {
			break
		}
	}
	if count == 0 {
		return "None yet.\n"
	}
	return b.String()
}

func (a *Assistant) app(id string) (state.App, error) {
	var x state.App
	found := false
	a.Store.Read(func(d *state.Data) {
		if p, ok := d.App(id); ok {
			x, found = *p, true
		}
	})
	if !found {
		return x, fmt.Errorf("no app called %q is installed (see list_apps)", id)
	}
	return x, nil
}

func (a *Assistant) firstAdmin() state.User {
	var u state.User
	a.Store.Read(func(d *state.Data) {
		for _, x := range d.Users() {
			if x.Admin {
				u = x
				return
			}
		}
	})
	return u
}

func accessFrom(x Args) state.Access {
	if x.Bool("public") {
		return state.Access{Public: true}
	}
	g := x.Strs("groups")
	if len(g) == 0 {
		g = []string{"all"}
	}
	return state.Access{Groups: g}
}

func accessText(a state.Access) string {
	if a.Public {
		return "anyone"
	}
	return "the groups " + strings.Join(a.Groups, ", ")
}

func oidcText(a state.App) string {
	if a.OIDC != nil {
		return ", signs in with Homeport (OIDC)"
	}
	return ""
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// checkDomain compares a domain's DNS with this server's addresses and asks this server's Caddy for it over HTTPS.
func checkDomain(ctx context.Context, domain string) string {
	var b strings.Builder
	ips, err := net.DefaultResolver.LookupHost(ctx, domain)
	if err != nil {
		fmt.Fprintf(&b, "DNS: %s doesn't resolve (%v). Point an A/AAAA record at this server.\n", domain, err)
	} else {
		fmt.Fprintf(&b, "DNS: %s → %s\n", domain, strings.Join(ips, ", "))
	}
	var mine []string
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
				mine = append(mine, ipn.IP.String())
			}
		}
	}
	fmt.Fprintf(&b, "This server's addresses: %s (behind a router, DNS shows the router's public address instead)\n", strings.Join(mine, ", "))

	// Ask this server's own Caddy, whatever DNS says.
	var cert *x509.Certificate
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, "127.0.0.1:443")
		},
		TLSClientConfig: &tls.Config{ServerName: domain, InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) > 0 {
				cert = cs.PeerCertificates[0]
			}
			return nil
		}},
	}
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://"+domain+"/", nil)
	res, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(&b, "HTTPS from this server's Caddy: failed (%v)\n", err)
		return b.String()
	}
	res.Body.Close()
	fmt.Fprintf(&b, "HTTPS from this server's Caddy: %s", res.Status)
	if loc := res.Header.Get("Location"); loc != "" {
		fmt.Fprintf(&b, " → %s", loc)
	}
	b.WriteString("\n")
	if cert != nil {
		_, verr := cert.Verify(x509.VerifyOptions{DNSName: domain})
		trust := "trusted by this server's certificate store"
		if verr != nil {
			trust = "NOT trusted (" + verr.Error() + ")"
		}
		fmt.Fprintf(&b, "Certificate: issued by %q, valid until %s, %s\n", cert.Issuer.CommonName, cert.NotAfter.Format("2006-01-02"), trust)
	}
	return b.String()
}
