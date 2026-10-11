// Command homeport runs the Homeport service (`homeport serve`, started by systemd) and is the admin command line
// for it (`homeport user add …`, `homeport app install …`), talking to the service over a root-only socket.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"homeport/internal/apps"
	"homeport/internal/proxy"
	"homeport/internal/runner"
	"homeport/internal/server"
	"homeport/internal/state"
)

var version = "dev" // set at build time

const (
	stateDir   = "/var/lib/homeport"
	socketPath = "/run/homeport/homeport.sock"
	caddyfile  = "/etc/caddy/Caddyfile"
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

  homeport app catalog
  homeport app list
  homeport app install CATALOG-ID --domain DOMAIN [--public | --groups a,b] [--id ID]
  homeport app update ID
  homeport app access ID (--public | --groups a,b)
  homeport app secrets ID
  homeport app remove ID [--purge]

  homeport serve      (run by systemd)
`)
}

// MARK: Service

func serve() error {
	store, err := state.Open(filepath.Join(stateDir, "state.json"))
	if err != nil {
		return err
	}
	sys := runner.System{}
	caddy := proxy.Caddy{Path: caddyfile, Runner: sys}
	applyProxy := func(ctx context.Context) error {
		var st state.Settings
		var list []state.App
		store.Read(func(d *state.Data) { st, list = *d.Settings(), d.Apps() })
		if st.MainDomain == "" {
			return nil // not set up yet: leave Caddy alone
		}
		return caddy.Apply(ctx, proxy.Caddyfile(st, list))
	}
	manager := &apps.Manager{Store: store, Root: filepath.Join(stateDir, "apps"), Runner: sys, TimeZone: localZone(), Apply: applyProxy}
	signer, err := server.LoadSigner(filepath.Join(stateDir, "oidc-signing-key.pem"))
	if err != nil {
		return fmt.Errorf("OIDC signing key: %w", err)
	}
	srv := &server.Server{Store: store, Apps: manager, Version: version, ApplyProxy: applyProxy, Signer: signer}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := applyProxy(ctx); err != nil {
		log.Printf("Caddy configuration: %v", err)
	}
	web := &http.Server{Addr: proxy.Upstream, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- web.ListenAndServe() }()
	go func() { errs <- srv.ListenLocal(socketPath) }()
	log.Printf("Homeport %s listening on %s and %s", version, proxy.Upstream, socketPath)
	select {
	case <-ctx.Done():
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return web.Shutdown(shut)
	case err := <-errs:
		return err
	}
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
			return errors.New("run this as root (sudo homeport …)")
		}
		return fmt.Errorf("can't reach the Homeport service (is it running? systemctl status homeport): %w", err)
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
	var j struct{ ID, Status, Error, Log string }
	if err := call(method, path, body, &j); err != nil {
		return err
	}
	printed := 0
	for {
		if err := call("GET", "/jobs/"+j.ID, nil, &j); err != nil {
			return err
		}
		if len(j.Log) > printed {
			fmt.Print(j.Log[printed:])
			printed = len(j.Log)
		}
		if j.Status != "running" {
			if j.Status == "failed" {
				return errors.New(j.Error)
			}
			return nil
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
	}
	if err := call("GET", "/overview", nil, &o); err != nil {
		return err
	}
	if o.Settings.MainDomain == "" {
		fmt.Println("Not set up yet: run homeport init.")
		return nil
	}
	fmt.Printf("Homeport %s on %s (certificates: %s)\n%d users, %d domains, %d apps\n", o.Version, o.Settings.MainDomain, o.Settings.TLS, o.Users, o.Domains, o.Apps)
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
			Name, App string
			Main      bool
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
			rows = append(rows, []string{d.Name, use})
		}
		table("DOMAIN\tUSED FOR", rows)
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
		pos := parse(fs, rest)
		if len(pos) != 1 || *domain == "" {
			return errors.New("usage: homeport app install CATALOG-ID --domain DOMAIN [--public | --groups a,b]")
		}
		req := map[string]any{"catalog": pos[0], "domain": *domain, "id": *id}
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
