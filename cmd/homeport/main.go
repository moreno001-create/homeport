// Command homeport runs the Homeport service (`homeport serve`, started by systemd) and is the admin command line
// for it (`homeport user add …`, `homeport app install …`), talking to the service over a root-only socket.
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
	_ "time/tzdata" // time zones for the world clock, whatever the machine has

	"golang.org/x/term"

	"homeport/internal/apps"
	"homeport/internal/assist"
	"homeport/internal/backup"
	"homeport/internal/certs"
	"homeport/internal/dyndns"
	"homeport/internal/firewall"
	"homeport/internal/mdns"
	"homeport/internal/platform"
	"homeport/internal/proxy"
	"homeport/internal/runner"
	"homeport/internal/server"
	"homeport/internal/shares"
	"homeport/internal/state"
	"homeport/internal/system"
	"homeport/internal/terminal"
)

var version = "dev" // set at build time

// Where Homeport keeps things, by system (see internal/platform).
var (
	plat       = platform.Current()
	stateDir   = plat.StateDir
	socketPath = plat.Socket
	caddyfile  = plat.Caddyfile
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "init":
		err = cmdInit(args)
	case "user", "users":
		err = cmdUser(args)
	case "domain", "domains":
		err = cmdDomain(args)
	case "app", "apps":
		err = cmdApp(args)
	case "status":
		err = cmdStatus()
	case "mcp":
		err = cmdMCP()
	case "approvals":
		err = cmdApprovals()
	case "approve", "deny":
		err = cmdDecide(cmd, args)
	case "audit":
		err = cmdAudit(args)
	case "memory":
		err = cmdMemory()
	case "assistant":
		err = cmdAssistant(args)
	case "terminal":
		err = cmdTerminal(args)
	case "version", "--version":
		fmt.Println("homeport", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command %q.\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Homeport: your own apps, users and domains on one Ubuntu server.

  homeport init --domain example.com --admin alice [--email you@example.com] [--tls internal]
  homeport status

  homeport user list
  homeport user add NAME [--name "Full Name"] [--email E] [--groups a,b] [--admin]
  homeport user passwd NAME
  homeport user groups NAME a,b
  homeport user delete NAME

  homeport domain list | add DOMAIN | remove DOMAIN
  homeport domain main DOMAIN [--tls internal|letsencrypt]   move the portal and admin panel
  homeport domain free desec|duckdns NAME    a free NAME.dedyn.io or NAME.duckdns.org, kept pointed here
  homeport domain home NAME [--keep-main] [--provider desec|cloudflare]
                                             a home name pointed at this server's home address, with a real
                                             certificate for it and every name under it: NAME is myhome
                                             (myhome.dedyn.io) or a name in your own domain on deSEC or Cloudflare
                                             (home.example.com)
  homeport domain cert DOMAIN auto|letsencrypt|internal      where its certificate comes from
  homeport domain cert DOMAIN custom --cert FULLCHAIN.pem --key KEY.pem
  homeport domain certs                      the certificate each site really has

  homeport app catalog
  homeport app list
  homeport app install CATALOG-ID --domain DOMAIN [--public | --groups a,b] [--id ID]
  homeport app install https://github.com/owner/app --domain DOMAIN [--model claude|local] [--yes]
  homeport app update ID
  homeport app start|stop|restart ID
  homeport app access ID (--public | --groups a,b)
  homeport app secrets ID
  homeport app notes ID        its setup notes, passwords Homeport made, and your notes (edit them in the admin panel)
  homeport app remove ID [--purge]

  AI helpers (MCP):
  homeport mcp                 connect an agent (opencode, Claude Code…) over stdin/stdout
  homeport approvals           changes waiting for approval
  homeport approve ID | deny ID
  homeport audit [N]           the last N tool calls
  homeport memory              the agents' shared notes and work in progress
  homeport assistant shell on|off
  homeport assistant local [URL [--model NAME] | off]   the local model for the chat (Ollama, LM Studio…)

  homeport terminal on|off     a terminal in the admin panel (admins, then the Linux login)

  homeport serve      (run by systemd)
`)
}

// MARK: Service

func serve() error {
	// systemd makes this folder on Linux; on a Mac Homeport makes it (only its owner can read it).
	if err := os.MkdirAll(filepath.Join(stateDir, "apps"), 0o700); err != nil {
		return err
	}
	store, err := state.Open(filepath.Join(stateDir, "state.json"))
	if err != nil {
		return err
	}
	sys := runner.System{}
	caddy := proxy.Caddy{Path: caddyfile, Runner: sys, Reload: plat.ReloadCaddy, ReloadFallback: plat.ReloadCaddyFallback}
	var announce *mdns.Publisher // .local names, through Avahi (Linux) or dns-sd (macOS)
	switch plat.Name {
	case "linux":
		host, _ := os.Hostname()
		announce = &mdns.Publisher{OS: "linux", Hostname: host, HostsFile: "/etc/hosts"}
	case "macos":
		// The Mac announces its own Local hostname (System Settings → General → Sharing) itself.
		host, _ := exec.Command("scutil", "--get", "LocalHostName").Output()
		announce = &mdns.Publisher{OS: "macos", Hostname: strings.TrimSpace(string(host))}
	}
	applyProxy := func(ctx context.Context) error {
		var st state.Settings
		var list []state.App
		var names []string  // what Caddy serves: announced on the home network when they're .local
		var home []string   // home-network names the portal also answers on
		var homeName string // the home name with a real certificate for it and every name under it
		own := map[string]proxy.SiteTLS{}
		store.Read(func(d *state.Data) {
			st, list = *d.Settings(), d.Apps()
			home = d.HomeNames()
			homeName = d.HomeName()
			names = append(names, st.MainDomain, d.AIDomain())
			names = append(names, home...)
			for _, a := range list {
				names = append(names, a.Domain)
			}
			for _, x := range d.Domains() {
				if x.Cert != "" {
					cert, key := certs.Paths(plat.CertDir, x.Name)
					own[x.Name] = proxy.SiteTLS{Mode: x.Cert, CertFile: cert, KeyFile: key}
				}
			}
		})
		if st.MainDomain == "" {
			return nil // not set up yet: leave Caddy alone
		}
		if homeName != "" && homeName == st.MainDomain {
			names = append(names, "signin."+homeName) // where people outside sign in (state.SignInName)
		}
		homeCerts(own, names, homeName, plat.CertDir)
		writeLoginBanner(st.MainDomain)
		addrs := localAddrs()
		if announce != nil {
			if err := announce.Publish(ctx, names, addrs); err != nil {
				log.Printf("Announcing .local names: %v", err)
			}
		}
		// The portal also answers on the machine's addresses and its home-network names, with Caddy's own certificates.
		return caddy.Apply(ctx, proxy.CaddyfileFor(st, list, append(slices.Clone(addrs), home...), own))
	}
	manager := &apps.Manager{Store: store, Root: filepath.Join(stateDir, "apps"), Runner: sys, TimeZone: localZone(), Apply: applyProxy,
		SystemCA: plat.SystemCA, CaddyRoot: plat.CaddyRoot, LocalAddrs: localAddrs}
	signer, err := server.LoadSigner(filepath.Join(stateDir, "oidc-signing-key.pem"))
	if err != nil {
		return fmt.Errorf("OIDC signing key: %w", err)
	}
	helper := &assist.Assistant{Store: store, Apps: manager, Runner: sys, Caddyfile: caddyfile, Version: version, ApplyProxy: applyProxy, Platform: plat,
		Audit: &assist.Audit{Path: filepath.Join(stateDir, "audit.log")}, Approvals: &assist.Approvals{},
		Memory: &assist.Memory{Path: filepath.Join(stateDir, "assistant-memory.md")}}
	chat := &assist.Chat{A: helper, KeyFile: filepath.Join(stateDir, "anthropic-api-key"), UsageFile: filepath.Join(stateDir, "assistant-usage.json")}
	srv := &server.Server{Store: store, Apps: manager, Version: version, ApplyProxy: applyProxy, Signer: signer, Assist: helper,
		Chat: chat, LocalAddrs: localAddrs, CertDir: plat.CertDir, HTTPSAddr: "127.0.0.1:" + cmp.Or(os.Getenv("HOMEPORT_HTTPS_PORT"), "443"),
		DynDNS: &dyndns.Updater{Store: store, Dir: filepath.Join(stateDir, "dyndns"), LocalIPv4: homeAddress}}
	srv.System = &system.Manager{Runner: sys, OS: plat.Name, UID: os.Getuid(), Logs: plat.LogsCommand}
	srv.Firewall = &firewall.Manager{Runner: sys, OS: plat.Name}
	srv.Shares = shares.New(sys, plat.Name)
	srv.Backup = &backup.Manager{Store: store, Apps: manager, StateDir: stateDir, Dir: filepath.Join(stateDir, "backup"), Run: backup.RunRestic,
		SystemCA: plat.SystemCA, CaddyRoot: plat.CaddyRoot, SSHDir: backupSSHDir(stateDir)}
	helper.Extra = srv.AssistantTools
	if plat.Name == "linux" {
		srv.SetTerminal = func(ctx context.Context, on bool) error { return terminal.Set(ctx, sys, on) }
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := applyProxy(ctx); err != nil {
		log.Printf("Caddy configuration: %v", err)
	}
	go srv.DynDNS.Run(ctx)
	go followAddress(ctx, applyProxy)
	go srv.Backup.Schedule(ctx)
	go srv.DailyChecks(ctx)
	go srv.FinishInterrupted()
	go srv.KeepRouterPorts(ctx)
	go srv.KeepHomeCerts(ctx)
	web := &http.Server{Addr: proxy.Upstream, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- web.ListenAndServe() }()
	go func() { errs <- srv.ListenLocal(socketPath) }()
	log.Printf("Homeport %s listening on %s and %s", version, proxy.Upstream, socketPath)
	select {
	case <-ctx.Done():
		// Installs and updates under way finish first (the service manager gives Homeport the time).
		srv.StopJobs(30 * time.Minute)
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return web.Shutdown(shut)
	case err := <-errs:
		return err
	}
}

// backupSSHDir is where SFTP backups keep their SSH key: Homeport's own folder, unless its path has a space (a Mac's
// Application Support), then ~/.config/homeport-backup.
func backupSSHDir(stateDir string) string {
	if !strings.Contains(stateDir, " ") {
		return filepath.Join(stateDir, "backup")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "homeport-backup")
}

// followAddress applies the configuration again when this machine's own addresses change (a new address from the
// router): Caddy serves the portal by address, and .local names are announced at it.
func followAddress(ctx context.Context, apply func(context.Context) error) {
	last := strings.Join(localAddrs(), " ")
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if now := strings.Join(localAddrs(), " "); now != last {
			log.Printf("This machine's address changed (%s → %s); applying the configuration again", last, now)
			last = now
			if err := apply(ctx); err != nil {
				log.Printf("Caddy configuration: %v", err)
			}
		}
	}
}

// writeLoginBanner puts the admin panel's address above the console login prompt (Linux), so it's on screen after a
// reboot. \4 is the login prompt's code for this machine's current IPv4 address, so it stays right if it changes.
func writeLoginBanner(domain string) {
	if plat.Name != "linux" {
		return
	}
	if err := os.MkdirAll("/etc/issue.d", 0o755); err != nil {
		return
	}
	text := fmt.Sprintf("Homeport admin panel:  https://\\4/admin   (or https://%s/admin)\n\n", domain)
	path := "/etc/issue.d/homeport.issue"
	if old, err := os.ReadFile(path); err == nil && string(old) == text {
		return
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err == nil {
		_ = os.Chmod(path, 0o644)
	}
}

// localAddrs are this machine's own IPv4 addresses on real network interfaces (not loopback, Docker bridges, VMs or
// VPN tunnels), for reaching the portal and admin panel by IP.
// homeCerts gives every name under the home name its wildcard certificate (Caddy's own until it's there, rather than
// Caddy trying Let's Encrypt itself, which can't reach a home address).
func homeCerts(own map[string]proxy.SiteTLS, names []string, homeName, certDir string) {
	if homeName == "" {
		return
	}
	cert, key := certs.Paths(certDir, homeName)
	t := proxy.SiteTLS{Mode: state.CertInternal}
	if _, err := os.Stat(cert); err == nil {
		t = proxy.SiteTLS{Mode: state.CertCustom, CertFile: cert, KeyFile: key}
	}
	for _, n := range append(names, homeName) {
		if n == homeName || strings.HasSuffix(n, "."+homeName) {
			own[n] = t
		}
	}
}

// homeAddress is this server's address on the home network, for its home name: a private address (192.168…, 10…,
// 172.16-31…) before others (Tailscale's 100.64…).
func homeAddress() string {
	addrs := localAddrs()
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.IsPrivate() {
			return a
		}
	}
	if len(addrs) > 0 {
		return addrs[0]
	}
	return ""
}

func localAddrs() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	skip := []string{"lo", "docker", "br-", "veth", "virbr", "bridge", "utun", "tun", "tap", "wg", "tailscale", "vmnet", "awdl", "llw", "anpi", "ap1"}
	var out []string
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		bad := false
		for _, p := range skip {
			if strings.HasPrefix(i.Name, p) {
				bad = true
				break
			}
		}
		if bad {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() && !ipn.IP.IsLoopback() {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}

func localZone() string {
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		return strings.TrimSpace(string(b))
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			return target[i+len("zoneinfo/"):]
		}
	}
	return "UTC"
}

// MARK: Talking to the service

var client = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
}}}

func call(method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://homeport/api"+path, r)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		if errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "permission denied") {
			return errors.New("run this as root (sudo homeport …), or as the user Homeport runs as on a Mac")
		}
		return fmt.Errorf("can't reach the Homeport service (is it running? %s): %w", plat.ServiceHint, err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s", res.Status)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// follow prints a background job's log as it grows, until it ends.
func follow(path string, body any, method string) error {
	_, err := followResult(path, body, method)
	return err
}

// followResult is follow for jobs that make something (a draft manifest), returning it.
func followResult(path string, body any, method string) (json.RawMessage, error) {
	var j struct {
		ID, Status, Error, Log string
		Result                 json.RawMessage
	}
	if err := call(method, path, body, &j); err != nil {
		return nil, err
	}
	printed := 0
	for {
		if err := call("GET", "/jobs/"+j.ID, nil, &j); err != nil {
			return nil, err
		}
		if len(j.Log) > printed {
			fmt.Print(j.Log[printed:])
			printed = len(j.Log)
		}
		if j.Status != "running" {
			if j.Status == "failed" {
				return nil, errors.New(j.Error)
			}
			return j.Result, nil
		}
		time.Sleep(700 * time.Millisecond)
	}
}

func readPassword(prompt string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}
	fmt.Fprint(os.Stderr, prompt)
	a, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Again: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(a) != string(b) {
		return "", errors.New("the passwords didn't match")
	}
	return string(a), nil
}

// readSecret reads a token without showing it (or a line from stdin when it isn't a terminal).
func readSecret(prompt string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		return strings.TrimSpace(line), err
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	return strings.TrimSpace(string(b)), err
}

func groupsFlag(s string) []string {
	var out []string
	for _, g := range strings.Split(s, ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// parse lets flags come after positional arguments (homeport user add bob --admin).
func parse(fs *flag.FlagSet, args []string) []string {
	var pos []string
	for len(args) > 0 {
		_ = fs.Parse(args)
		args = fs.Args()
		if len(args) > 0 {
			pos = append(pos, args[0])
			args = args[1:]
		}
	}
	return pos
}

func table(header string, rows [][]string) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, header)
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// MARK: Commands

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	domain := fs.String("domain", "", "main domain (portal and admin panel)")
	admin := fs.String("admin", "", "first admin's user name")
	email := fs.String("email", "", "email for Let's Encrypt notices")
	tls := fs.String("tls", "letsencrypt", "letsencrypt, or internal (test machines)")
	parse(fs, args)
	if *domain == "" || *admin == "" {
		return errors.New("usage: homeport init --domain example.com --admin alice")
	}
	pw, err := readPassword("Password for " + *admin + ": ")
	if err != nil {
		return err
	}
	var out map[string]string
	err = call("POST", "/init", map[string]any{"mainDomain": *domain, "acmeEmail": *email, "tls": *tls,
		"admin": map[string]any{"name": *admin, "password": pw}}, &out)
	if err != nil {
		return err
	}
	fmt.Printf("Ready.\n  Portal: %s\n  Admin:  %s\n", out["portal"], out["admin"])
	return nil
}

func cmdStatus() error {
	var o struct {
		Version  string
		Settings state.Settings
		Users    int
		Domains  int
		Apps     int
		Working  []string
	}
	if err := call("GET", "/overview", nil, &o); err != nil {
		return err
	}
	if o.Settings.MainDomain == "" {
		fmt.Println("Not set up yet: run homeport init.")
		return nil
	}
	fmt.Printf("Homeport %s on %s (certificates: %s)\n%d users, %d domains, %d apps\n", o.Version, o.Settings.MainDomain, o.Settings.TLS, o.Users, o.Domains, o.Apps)
	if len(o.Working) > 0 {
		fmt.Printf("Working on: %s\n", strings.Join(o.Working, ", "))
	}
	return nil
}

func cmdUser(args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		var users []struct {
			Name, DisplayName, Email string
			Groups                   []string
			Admin                    bool
		}
		if err := call("GET", "/users", nil, &users); err != nil {
			return err
		}
		var rows [][]string
		for _, u := range users {
			role := ""
			if u.Admin {
				role = "admin"
			}
			rows = append(rows, []string{u.Name, u.DisplayName, u.Email, strings.Join(u.Groups, ","), role})
		}
		table("USER\tNAME\tEMAIL\tGROUPS\t", rows)
		return nil
	case "add":
		fs := flag.NewFlagSet("user add", flag.ExitOnError)
		name := fs.String("name", "", "full name")
		email := fs.String("email", "", "email")
		groups := fs.String("groups", "", "comma-separated groups")
		admin := fs.Bool("admin", false, "can use the admin panel")
		pos := parse(fs, rest)
		if len(pos) != 1 {
			return errors.New("usage: homeport user add NAME [--name …] [--email …] [--groups a,b] [--admin]")
		}
		pw, err := readPassword("Password for " + pos[0] + ": ")
		if err != nil {
			return err
		}
		if err := call("POST", "/users", map[string]any{"name": pos[0], "displayName": *name, "email": *email, "groups": groupsFlag(*groups), "admin": *admin, "password": pw}, nil); err != nil {
			return err
		}
		fmt.Println("Added", pos[0])
		return nil
	case "passwd", "password":
		if len(rest) != 1 {
			return errors.New("usage: homeport user passwd NAME")
		}
		pw, err := readPassword("New password for " + rest[0] + ": ")
		if err != nil {
			return err
		}
		if err := call("PATCH", "/users/"+rest[0], map[string]any{"password": pw}, nil); err != nil {
			return err
		}
		fmt.Println("Changed; they're signed out everywhere.")
		return nil
	case "groups":
		if len(rest) != 2 {
			return errors.New("usage: homeport user groups NAME a,b")
		}
		return call("PATCH", "/users/"+rest[0], map[string]any{"groups": groupsFlag(rest[1])}, nil)
	case "delete", "remove":
		if len(rest) != 1 {
			return errors.New("usage: homeport user delete NAME")
		}
		if err := call("DELETE", "/users/"+rest[0], nil, nil); err != nil {
			return err
		}
		fmt.Println("Deleted", rest[0])
		return nil
	}
	return fmt.Errorf("unknown user command %q", sub)
}

func cmdDomain(args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		var ds []struct {
			Name, App, Cert string
			Main, OwnCert   bool
			DynDNS          *state.DynDNS
		}
		if err := call("GET", "/domains", nil, &ds); err != nil {
			return err
		}
		var rows [][]string
		for _, d := range ds {
			use := d.App
			if d.Main {
				use = "portal and admin panel"
			}
			cert := d.Cert
			if !d.OwnCert {
				cert += " (server setting)"
			}
			free := ""
			if dd := d.DynDNS; dd != nil {
				free = dd.Provider + ": " + cmp.Or(strings.Trim(dd.IPv4+" "+dd.IPv6, " "), "not updated yet")
				if dd.Error != "" {
					free = dd.Provider + ": " + dd.Error
				}
			}
			rows = append(rows, []string{d.Name, use, cert, free})
		}
		table("DOMAIN\tUSED FOR\tCERTIFICATE\tFREE DOMAIN", rows)
		return nil
	case "free":
		if len(args) != 3 {
			return errors.New("usage: homeport domain free desec|duckdns NAME")
		}
		where := map[string]string{"desec": "https://desec.io (Token management)", "duckdns": "https://www.duckdns.org (after making NAME there)"}[args[1]]
		if where == "" {
			return errors.New(`the provider is "desec" or "duckdns"`)
		}
		token, err := readSecret("Token from " + where + ": ")
		if err != nil {
			return err
		}
		var out map[string]string
		if err := call("POST", "/domains/free", map[string]string{"provider": args[1], "name": args[2], "token": token}, &out); err != nil {
			return err
		}
		fmt.Printf("%s points here now, and Homeport keeps it that way. Its apps can use subdomains like cloud.%s.\n", out["name"], out["name"])
		return nil
	case "home":
		fs := flag.NewFlagSet("domain home", flag.ExitOnError)
		keep := fs.Bool("keep-main", false, "keep the main domain as it is")
		provider := fs.String("provider", "desec", "the DNS service: desec or cloudflare")
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: homeport domain home NAME [--keep-main]")
		}
		where := map[string]string{"desec": "Token from https://desec.io (Token management): ",
			"cloudflare": "Cloudflare API token (My Profile → API Tokens → \"Edit zone DNS\" for the domain): "}[*provider]
		if where == "" {
			return errors.New(`--provider is "desec" or "cloudflare"`)
		}
		token, err := readSecret(where)
		if err != nil {
			return err
		}
		return follow("/domains/home", map[string]any{"provider": *provider, "name": pos[0], "token": token, "main": !*keep}, "POST")
	case "cert":
		fs := flag.NewFlagSet("domain cert", flag.ExitOnError)
		certFile := fs.String("cert", "", "the certificate with its chain (PEM)")
		keyFile := fs.String("key", "", "its private key (PEM)")
		pos := parse(fs, args[1:])
		if len(pos) != 2 {
			return errors.New("usage: homeport domain cert DOMAIN auto|letsencrypt|internal|custom [--cert FILE --key FILE]")
		}
		body := map[string]string{"mode": pos[1]}
		if pos[1] == "auto" {
			body["mode"] = ""
		}
		if pos[1] == "custom" {
			if *certFile == "" || *keyFile == "" {
				return errors.New("custom needs --cert FULLCHAIN.pem and --key KEY.pem")
			}
			c, err := os.ReadFile(*certFile)
			if err != nil {
				return err
			}
			k, err := os.ReadFile(*keyFile)
			if err != nil {
				return err
			}
			body["cert"], body["key"] = string(c), string(k)
		}
		if err := call("PUT", "/domains/"+pos[0]+"/certificate", body, nil); err != nil {
			return err
		}
		fmt.Println("Done. Caddy is using it now (a new Let's Encrypt certificate can take a minute).")
		return nil
	case "certs", "certificates":
		var cs []struct {
			Name, Cert string
			DNS        []string
			Live       struct {
				Issuer   string
				NotAfter time.Time
				Trusted  bool
				Error    string
			}
		}
		if err := call("GET", "/certificates", nil, &cs); err != nil {
			return err
		}
		var rows [][]string
		for _, c := range cs {
			got := c.Live.Error
			if got == "" {
				trust := "trusted"
				if !c.Live.Trusted {
					trust = "not trusted by browsers"
				}
				got = fmt.Sprintf("%s, until %s, %s", c.Live.Issuer, c.Live.NotAfter.Format("2 Jan 2006"), trust)
			}
			rows = append(rows, []string{c.Name, c.Cert, cmp.Or(strings.Join(c.DNS, " "), "no DNS"), got})
		}
		table("DOMAIN\tCHOSEN\tDNS\tSERVED", rows)
		return nil
	case "add":
		if len(args) != 2 {
			return errors.New("usage: homeport domain add DOMAIN")
		}
		return call("POST", "/domains", map[string]string{"name": args[1]}, nil)
	case "remove", "delete":
		if len(args) != 2 {
			return errors.New("usage: homeport domain remove DOMAIN")
		}
		return call("DELETE", "/domains/"+args[1], nil, nil)
	case "main":
		fs := flag.NewFlagSet("domain main", flag.ExitOnError)
		tls := fs.String("tls", "", "letsencrypt, or internal (Caddy's own certificates)")
		pos := parse(fs, args[1:])
		if len(pos) != 1 {
			return errors.New("usage: homeport domain main DOMAIN [--tls internal|letsencrypt]")
		}
		body := map[string]any{"mainDomain": pos[0]}
		if *tls != "" {
			body["tls"] = *tls
		}
		if err := call("PUT", "/settings", body, nil); err != nil {
			return err
		}
		fmt.Printf("The portal and admin panel are now at https://%s/ and https://%s/admin\n", pos[0], pos[0])
		return nil
	}
	return fmt.Errorf("unknown domain command %q", args[0])
}

func cmdApp(args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "catalog":
		var cs []struct{ ID, Name, Category, Description string }
		if err := call("GET", "/catalog", nil, &cs); err != nil {
			return err
		}
		var rows [][]string
		for _, c := range cs {
			rows = append(rows, []string{c.ID, c.Name, c.Category, c.Description})
		}
		table("ID\tNAME\tCATEGORY\tWHAT IT IS", rows)
		return nil
	case "list":
		var as []struct {
			ID, Name, Domain, Status string
			Access                   state.Access
		}
		if err := call("GET", "/apps", nil, &as); err != nil {
			return err
		}
		var rows [][]string
		for _, a := range as {
			who := "anyone"
			if !a.Access.Public {
				who = strings.Join(a.Access.Groups, ",")
			}
			rows = append(rows, []string{a.ID, a.Name, "https://" + a.Domain, who, a.Status})
		}
		table("ID\tAPP\tADDRESS\tWHO\tSTATUS", rows)
		return nil
	case "install":
		fs := flag.NewFlagSet("app install", flag.ExitOnError)
		domain := fs.String("domain", "", "where to serve it")
		public := fs.Bool("public", false, "anyone can open it")
		groups := fs.String("groups", "", "only these groups (all = every user)")
		id := fs.String("id", "", "instance id (defaults to the catalog id)")
		use := fs.String("model", "", "for a repository without homeport.yaml: claude or local (default: Claude if there's a key)")
		yes := fs.Bool("yes", false, "for a repository: install without asking, if it passes the checks")
		pos := parse(fs, rest)
		if len(pos) != 1 || *domain == "" {
			return errors.New("usage: homeport app install (CATALOG-ID | https://github.com/owner/app) --domain DOMAIN [--public | --groups a,b]")
		}
		req := map[string]any{"catalog": pos[0], "domain": *domain, "id": *id}
		if strings.Contains(pos[0], "github.com") {
			raw, err := followResult("/apps/draft", map[string]string{"url": pos[0], "use": *use}, "POST")
			if err != nil {
				return err
			}
			var d struct {
				Source   state.Source
				Manifest string
				Review   struct{ Problems, Warnings []string }
			}
			if err := json.Unmarshal(raw, &d); err != nil {
				return err
			}
			fmt.Printf("\n--- Manifest (%s) ---\n%s---\n", d.Source.By, d.Manifest)
			for _, w := range d.Review.Warnings {
				fmt.Println("Good to know:", w)
			}
			if len(d.Review.Problems) > 0 {
				for _, p := range d.Review.Problems {
					fmt.Println("Problem:", p)
				}
				return errors.New("Homeport won't install this as it is (fix it in the admin panel's Install from GitHub)")
			}
			fmt.Println("\nThis app isn't from Homeport's catalog: it was checked, not reviewed. Only install apps you trust.")
			if !*yes {
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return errors.New("add --yes to install it without a terminal to ask on")
				}
				fmt.Print("Install it? [y/N] ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
					return errors.New("not installed")
				}
			}
			req = map[string]any{"manifest": d.Manifest, "source": d.Source, "domain": *domain, "id": *id}
		}
		if *public {
			req["access"] = state.Access{Public: true}
		} else if *groups != "" {
			req["access"] = state.Access{Groups: groupsFlag(*groups)}
		}
		return follow("/apps", req, "POST")
	case "update":
		if len(rest) != 1 {
			return errors.New("usage: homeport app update ID")
		}
		return follow("/apps/"+rest[0]+"/update", nil, "POST")
	case "start", "stop", "restart":
		if len(rest) != 1 {
			return fmt.Errorf("usage: homeport app %s ID", sub)
		}
		if err := call("POST", "/apps/"+rest[0]+"/"+sub, nil, nil); err != nil {
			return err
		}
		fmt.Printf("%s: %s done.\n", rest[0], sub)
		return nil
	case "access":
		fs := flag.NewFlagSet("app access", flag.ExitOnError)
		public := fs.Bool("public", false, "anyone can open it")
		groups := fs.String("groups", "", "only these groups")
		pos := parse(fs, rest)
		if len(pos) != 1 || (!*public && *groups == "") {
			return errors.New("usage: homeport app access ID (--public | --groups a,b)")
		}
		a := state.Access{Public: *public}
		if !*public {
			a.Groups = groupsFlag(*groups)
		}
		return call("PUT", "/apps/"+pos[0]+"/access", a, nil)
	case "secrets":
		if len(rest) != 1 {
			return errors.New("usage: homeport app secrets ID")
		}
		var s map[string]string
		if err := call("GET", "/apps/"+rest[0]+"/secrets", nil, &s); err != nil {
			return err
		}
		for k, v := range s {
			fmt.Printf("%s: %s\n", k, v)
		}
		return nil
	case "notes":
		if len(rest) != 1 {
			return errors.New("usage: homeport app notes ID")
		}
		var n struct {
			Name, Domain, Setup, Notes, Data string
			Secrets                          map[string]string
		}
		if err := call("GET", "/apps/"+rest[0]+"/notes", nil, &n); err != nil {
			return err
		}
		fmt.Printf("%s at https://%s (data in %s)\n", n.Name, n.Domain, n.Data)
		if n.Setup != "" {
			fmt.Printf("\nSetting it up:\n%s\n", n.Setup)
		}
		if len(n.Secrets) > 0 {
			fmt.Println("\nPasswords and keys Homeport made for it:")
			for _, k := range slices.Sorted(maps.Keys(n.Secrets)) {
				fmt.Printf("  %s: %s\n", k, n.Secrets[k])
			}
		}
		if n.Notes != "" {
			fmt.Printf("\nYour notes:\n%s\n", n.Notes)
		}
		return nil
	case "remove", "delete":
		fs := flag.NewFlagSet("app remove", flag.ExitOnError)
		purge := fs.Bool("purge", false, "also delete its data")
		pos := parse(fs, rest)
		if len(pos) != 1 {
			return errors.New("usage: homeport app remove ID [--purge]")
		}
		path := "/apps/" + pos[0]
		if *purge {
			path += "?purge=1"
		}
		return follow(path, nil, "DELETE")
	}
	return fmt.Errorf("unknown app command %q", sub)
}

// MARK: AI helpers

// cmdMCP carries MCP between an agent (stdin/stdout, one JSON-RPC message per line) and the service. Calls run side by
// side, since a change can wait minutes for an admin's approval.
func cmdMCP() error {
	client := "agent"
	var mu sync.Mutex
	out := bufio.NewWriter(os.Stdout)
	reply := func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}
	var wg sync.WaitGroup
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 1<<20), 8<<20)
	for in.Scan() {
		line := append([]byte(nil), in.Bytes()...)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// The agent's name (from initialize) goes in the audit log.
		var peek struct {
			Method string `json:"method"`
			Params struct {
				ClientInfo struct{ Name string } `json:"clientInfo"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &peek) == nil && peek.Method == "initialize" && peek.Params.ClientInfo.Name != "" {
			client = peek.Params.ClientInfo.Name
		}
		name := client
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("POST", "http://homeport/api/mcp", bytes.NewReader(line))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Homeport-Client", name)
			res, err := client0().Do(req)
			if err != nil {
				fmt.Fprintln(os.Stderr, "homeport mcp:", err)
				return
			}
			defer res.Body.Close()
			b, _ := io.ReadAll(res.Body)
			if res.StatusCode == http.StatusOK && len(bytes.TrimSpace(b)) > 0 {
				reply(bytes.TrimSpace(b))
			}
		}()
	}
	wg.Wait()
	return in.Err()
}

// client0 is the socket client without a timeout (approvals can take minutes).
func client0() *http.Client { return client }

func cmdApprovals() error {
	var list []assist.Approval
	if err := call("GET", "/approvals", nil, &list); err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("Nothing is waiting for approval.")
		return nil
	}
	for _, a := range list {
		fmt.Printf("%s  %s asks: %s\n    (%s, waits until %s)  homeport approve %s | homeport deny %s\n",
			a.ID, a.Client, a.Summary, a.Tool, a.Expires.Format("15:04"), a.ID, a.ID)
	}
	return nil
}

func cmdDecide(cmd string, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: homeport %s ID (see homeport approvals)", cmd)
	}
	if err := call("POST", "/approvals/"+args[0]+"/"+cmd, nil, nil); err != nil {
		return err
	}
	if cmd == "approve" {
		fmt.Println("Approved: it runs now.")
	} else {
		fmt.Println("Declined.")
	}
	return nil
}

func cmdAudit(args []string) error {
	n := "30"
	if len(args) == 1 {
		n = args[0]
	}
	var entries []assist.AuditEntry
	if err := call("GET", "/audit?limit="+n, nil, &entries); err != nil {
		return err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		status := "ok"
		if !e.OK {
			status = "error: " + e.Error
		}
		by := ""
		if e.By != "" {
			by = " by " + e.By
		}
		fmt.Printf("%s  %-14s %-6s %-20s %s%s, %s  %s\n", e.Time.Format("01-02 15:04:05"), e.Client, e.Level, e.Tool, e.Decision, by, status, string(e.Args))
	}
	return nil
}

func cmdMemory() error {
	var m map[string]string
	if err := call("GET", "/memory", nil, &m); err != nil {
		return err
	}
	fmt.Print(m["memory"])
	return nil
}

func cmdTerminal(args []string) error {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		return errors.New("usage: homeport terminal on|off")
	}
	on := args[0] == "on"
	var o struct {
		Settings state.Settings `json:"settings"`
	}
	if err := call("PUT", "/settings", map[string]any{"terminal": on}, &o); err != nil {
		return err
	}
	if on {
		fmt.Printf("The terminal is on: https://%s/terminal/ (admins only; then sign in with a Linux account).\n", o.Settings.MainDomain)
	} else {
		fmt.Println("The terminal is off.")
	}
	return nil
}

func cmdAssistant(args []string) error {
	if len(args) > 0 && args[0] == "local" {
		return cmdAssistantLocal(args[1:])
	}
	if len(args) != 2 || args[0] != "shell" || (args[1] != "on" && args[1] != "off") {
		return errors.New("usage: homeport assistant shell on|off, or homeport assistant local [URL [--model NAME] | off]")
	}
	on := args[1] == "on"
	if err := call("PUT", "/settings", map[string]any{"assistantShell": on}, nil); err != nil {
		return err
	}
	if on {
		fmt.Println("AI helpers may now ask to run shell commands (each one still needs approval).")
	} else {
		fmt.Println("Shell commands are off for AI helpers.")
	}
	return nil
}

// cmdAssistantLocal shows or sets the local model behind the assistant chat (an OpenAI-compatible server such as
// Ollama or LM Studio). Setting it checks the server answers and has the model; the other chat settings are kept.
func cmdAssistantLocal(args []string) error {
	fs := flag.NewFlagSet("assistant local", flag.ExitOnError)
	model := fs.String("model", "", "the model to use (default: the server's first chat model)")
	pos := parse(fs, args)
	var cur struct{ Settings state.ChatSettings }
	if err := call("GET", "/chat-settings", nil, &cur); err != nil {
		return err
	}
	st := cur.Settings
	switch {
	case len(pos) == 0:
		if st.LocalURL == "" {
			fmt.Println("No local model is set up.")
		} else {
			fmt.Printf("Local model: %s at %s\n", cmp.Or(st.LocalModel, "the server's first model"), st.LocalURL)
		}
		return nil
	case len(pos) == 1 && pos[0] == "off":
		st.LocalURL, st.LocalModel = "", ""
	case len(pos) == 1:
		var found struct {
			LocalURL string
			Models   []string
		}
		if err := call("POST", "/chat-settings/local-models", map[string]string{"localURL": pos[0]}, &found); err != nil {
			return err
		}
		if len(found.Models) == 0 {
			return fmt.Errorf("%s answers but has no chat model", found.LocalURL)
		}
		if *model == "" {
			*model = found.Models[0]
		} else if !slices.Contains(found.Models, *model) {
			return fmt.Errorf("%s has no model %q (it has: %s)", found.LocalURL, *model, strings.Join(found.Models, ", "))
		}
		st.LocalURL, st.LocalModel = found.LocalURL, *model
	default:
		return errors.New("usage: homeport assistant local [URL [--model NAME] | off]")
	}
	if err := call("PUT", "/chat-settings", st, nil); err != nil {
		return err
	}
	if st.LocalURL == "" {
		fmt.Println("The assistant no longer uses a local model.")
	} else {
		fmt.Printf("The assistant uses %s at %s.\n", st.LocalModel, st.LocalURL)
	}
	return nil
}
