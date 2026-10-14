package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"homeport/internal/assist"
	"homeport/internal/certs"
	"homeport/internal/platform"
	"homeport/internal/router"
	"homeport/internal/state"
	"homeport/internal/sysload"
)

// MARK: Looking after itself
//
// So that Homeport's assistants (and admins) can fix this server without whoever set it up:
//   - server_setup: how this server is set up, written from the machine itself (also saved as server-setup.md);
//   - the checks: what's wrong right now, each with the guide entry that explains it and, when Homeport can, a fix;
//     run from the Overview page, every day, and by assistants (run_checks);
//   - look tools for the pages assistants couldn't see (internet access, firewall, backups, services, AI models);
//   - a few small repairs (restart a service, write Caddy's configuration again, ask the router for the ports again),
//     each approved by an admin like every change.

func (s *Server) selfCareRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/setup", guard(s.setupPage))
	mux.HandleFunc("GET /api/checks", guard(s.lastChecksView))
	mux.HandleFunc("POST /api/checks", guard(s.runChecksNow))
	mux.HandleFunc("POST /api/checks/fix", guard(s.fixCheck))
	mux.HandleFunc("GET /api/guide", guard(s.guidePage))
}

// plat is the system Homeport runs on, as the assistant knows it.
func (s *Server) plat() platform.Platform {
	if s.Assist != nil && s.Assist.Platform.Name != "" {
		return s.Assist.Platform
	}
	p := platform.Current()
	p.StateDir = filepath.Dir(s.Apps.Root)
	return p
}

func (s *Server) platformName() string { return s.plat().Name }

func (s *Server) stateDir() string { return s.plat().StateDir }

// MARK: How this server is set up

// SetupText describes this server: the machine, where Homeport's files are, what runs, and the choices made. It's
// written from the machine itself, so it's never out of date.
func (s *Server) SetupText(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	plat := s.plat()
	var st state.Settings
	var home []string
	var public, ai string
	var apps []state.App
	var bk state.Backup
	s.Store.Read(func(d *state.Data) {
		st, home, public, ai, apps, bk = *d.Settings(), d.HomeNames(), d.PublicBase(), d.AIDomain(), d.Apps(), *d.Backup()
	})
	spec := sysload.ReadSpecs(ctx, s.stateDir())
	exe, _ := os.Executable()
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	w("# How this server is set up")
	w("")
	w("Written by Homeport %s on %s from the machine itself. Homeport writes this again when it starts and whenever an assistant reads it.", s.Version, time.Now().Format("2006-01-02 15:04"))
	w("")
	w("## Machine")
	w("- %s (%s), kernel %s, named %q", spec.OS, spec.Arch, spec.Kernel, spec.Hostname)
	w("- Processor: %s, %d threads; memory %s; disk %s with %s free", spec.CPU, spec.Cores, gib(spec.Memory), gib(spec.Disk.Total), gib(spec.Disk.Free))
	if spec.GPU != "" {
		w("- Graphics: %s", spec.GPU)
	}
	w("- Addresses on the home network: %s", strings.Join(s.localAddrs(), ", "))
	w("- Docker %s, Caddy %s", cmpOr(spec.Docker, "(not found)"), cmpOr(spec.Caddy, "(not found)"))
	w("")
	w("## Homeport")
	w("- Program: %s", exe)
	if plat.Name == "macos" {
		w("- Runs for the signed-in user as a launchd agent (dev.homeport.homeport, in ~/Library/LaunchAgents), like Caddy (dev.homeport.caddy). Logs: %s", filepath.Join(plat.StateDir, "logs"))
	} else {
		w("- Runs as root under systemd: homeport.service (/etc/systemd/system/homeport.service, `homeport serve`). Logs: `journalctl -u homeport`")
	}
	w("- Command line: `%s status`, `%s app …`, `%s assistant …` (it talks to the service over %s)", plat.Admin, plat.Admin, plat.Admin, plat.Socket)
	w("- Its files, in %s:", plat.StateDir)
	w("  - state.json: settings, users, domains, apps (with their passwords): keep it safe")
	w("  - apps/<id>/: each app's compose.yaml and data")
	w("  - assistant-memory.md (assistants' notebook), audit.log (every assistant tool call), server-setup.md (this page)")
	w("  - backup/ (restic settings and logs), dyndns/ (free internet names)")
	w("- Caddy: Homeport writes its configuration to %s and reloads it with `%s`; changes made by hand are replaced. Uploaded certificates: %s. Caddy's own root certificate (for home-only names): %s", plat.Caddyfile, strings.Join(plat.ReloadCaddy, " "), plat.CertDir, plat.CaddyRoot)
	w("- To update or repair Homeport: run the installer again (it keeps everything)")
	w("")
	w("## Services")
	if s.System != nil {
		for _, sv := range s.System.List(ctx) {
			since := ""
			if !sv.Since.IsZero() {
				since = ", since " + sv.Since.Format("2006-01-02 15:04")
			}
			w("- %s (%s): %s%s. %s", sv.Name, sv.ID, sv.State, since, sv.About)
		}
	}
	w("")
	w("## Addresses and certificates")
	certText := "Let's Encrypt for names on the internet"
	if st.TLS == state.TLSInternal {
		certText = "Caddy's own (browsers warn)"
	}
	w("- Main address: %s (certificates: %s)", st.MainDomain, certText)
	if len(home) > 0 {
		w("- Home-network names (work only at home, with Caddy's own certificates): %s", strings.Join(home, ", "))
	}
	var homeName string
	s.Store.Read(func(d *state.Data) { homeName = d.HomeName() })
	if homeName != "" {
		w("- Home name: %s (deSEC) points at this server's home address, it and every name under it, with a Let's Encrypt certificate for *.%s got through DNS (no ports); Homeport renews it. Apps put on the internet keep their name, which then points at the public address.", homeName, homeName)
	}
	if public != "" {
		w("- Internet name: %s (apps put on the internet go under it, with Let's Encrypt certificates)", public)
	} else {
		w("- Internet name: none yet (Internet access page)")
	}
	w("- Router: Homeport keeps ports 80 and 443 forwarded to it (UPnP): %v", st.RouterPorts)
	if ai != "" {
		w("- AI page: %s", ai)
	}
	if plat.Name == "macos" {
		w("- .local names are announced on the home network with dns-sd (one process per name, started by Homeport)")
	} else {
		w("- .local names are announced on the home network with Avahi (avahi-publish, one per name, started by Homeport)")
	}
	w("")
	w("## Apps")
	if len(apps) == 0 {
		w("None yet.")
	}
	for _, a := range apps {
		where := "home network only"
		var online bool
		s.Store.Read(func(d *state.Data) { online = d.OnInternet(a) })
		if online {
			where = "on the internet"
		}
		w("- %s (%s): https://%s/ (%s), folder %s", a.Name, a.ID, a.Domain, where, s.Apps.Dir(a.ID))
	}
	w("")
	w("## Local AI")
	if st.Chat.LocalURL == "" {
		w("No local model server is connected.")
	} else {
		w("- Model server: %s; model: %s; chat mode: %s", st.Chat.LocalURL, cmpOr(st.Chat.LocalModel, "its first model"), cmpOr(st.Chat.Mode, "auto"))
		if root := assist.OllamaRoot(ctx, st.Chat.LocalURL); root != "" {
			w("- It's Ollama (models on the AI models page)")
			if b, err := os.ReadFile("/etc/systemd/system/ollama.service.d/homeport.conf"); err == nil {
				w("- Ollama's settings from the installer (/etc/systemd/system/ollama.service.d/homeport.conf): %s", strings.Join(strings.Fields(string(b)), " "))
			}
		}
	}
	w("- Claude: %s", map[bool]string{true: "an Anthropic API key is saved", false: "no Anthropic API key"}[s.Chat != nil && s.Chat.APIKey() != ""])
	w("- Assistants may ask to run shell commands: %v", st.AssistantShell)
	w("")
	w("## Backups")
	if len(bk.Destinations) == 0 {
		w("Not set up.")
	} else {
		w("- Daily at %s, keeping %d daily, %d weekly, %d monthly; last scheduled run: %s", cmpOr(bk.Time, "(never)"), bk.KeepDaily, bk.KeepWeekly, bk.KeepMonthly, cmpOr(bk.LastScheduled, "not yet"))
		for _, d := range bk.Destinations {
			w("- To %s (%s)", d.Name, d.Kind)
		}
	}
	w("")
	if s.Firewall != nil {
		fw := s.Firewall.Status(ctx)
		w("## Firewall")
		switch {
		case !fw.Available:
			w("Not available: %s", fw.Why)
		case !fw.Active:
			w("Off.")
		default:
			w("On, with %d rules; SSH on port %d stays open.", len(fw.Rules), fw.SSHPort)
		}
		w("")
	}
	w("## What the installer did")
	if plat.Name == "macos" {
		w("- Installed Caddy with Homebrew and checked for a Docker runtime (OrbStack, Colima or Docker Desktop: it has to be open for the apps to run)")
		w("- Put Homeport in place and started it and Caddy as launchd agents for this user; state in %s", plat.StateDir)
		w("- Optionally: a local AI model (Ollama or LM Studio)")
	} else {
		w("- Installed docker.io, docker-compose-v2, avahi-daemon, avahi-utils, libnss-mdns (.local names), restic (backups) and zstd, and Caddy (from Caddy's apt repository, or Ubuntu's own)")
		w("- Put Homeport at /usr/local/bin/homeport, wrote /etc/systemd/system/homeport.service, and enabled docker, caddy and homeport")
		w("- If ufw was on, opened ports 80 and 443")
		w("- Installed Ollama with a model that fits this machine, with /etc/systemd/system/ollama.service.d/homeport.conf (16K context, model kept loaded 30 minutes)")
		w("- Asked for the main address and the first admin")
	}
	w("")
	w("## For helpers")
	w("- Change things through Homeport's tools and pages, not by editing its files: it writes Caddy's configuration and the apps' compose files itself.")
	w("- Problems: run_checks, then troubleshooting_guide.")
	text := assist.Redact(s.Store, b.String())
	_ = os.WriteFile(filepath.Join(s.stateDir(), "server-setup.md"), []byte(text), 0o600)
	return text
}

func gib(n int64) string { return fmt.Sprintf("%.1f GB", float64(n)/(1<<30)) }

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"text": s.SetupText(r.Context())})
}

func (s *Server) guidePage(w http.ResponseWriter, r *http.Request) {
	list := assist.SearchGuide(s.platformName(), r.URL.Query().Get("search"))
	if id := r.URL.Query().Get("id"); id != "" {
		list = slices.DeleteFunc(list, func(e assist.GuideEntry) bool { return e.ID != id })
	}
	if list == nil {
		list = []assist.GuideEntry{}
	}
	writeJSON(w, http.StatusOK, list)
}

// MARK: Checks

// Check is one thing checked, and what to do when it's wrong.
type Check struct {
	Title    string `json:"title"`
	State    string `json:"state"` // ok, warn, bad
	Detail   string `json:"detail"`
	Fix      string `json:"fix,omitempty"` // a repair Homeport can make (fixProblem)
	FixLabel string `json:"fixLabel,omitempty"`
	Guide    string `json:"guide,omitempty"` // the troubleshooting guide entry about it
}

// CheckRun is a whole round of checks.
type CheckRun struct {
	At       time.Time `json:"at"`
	Checks   []Check   `json:"checks"`
	Problems int       `json:"problems"` // bad ones
	Warnings int       `json:"warnings"`
}

type checksState struct {
	mu   sync.Mutex
	last *CheckRun
}

// serviceGuides: which guide entry explains a service being down.
var serviceGuides = map[string]string{"homeport": "homeport-down", "caddy": "caddy-config", "docker": "docker-problems",
	"ollama": "ollama-problems", "avahi-daemon": "local-names"}

// RunChecks checks the server. It takes up to half a minute (the router answers slowly).
func (s *Server) RunChecks(ctx context.Context) CheckRun {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var st state.Settings
	var apps []state.App
	var public string
	var bk state.Backup
	s.Store.Read(func(d *state.Data) { st, apps, public, bk = *d.Settings(), d.Apps(), d.PublicBase(), *d.Backup() })

	var mu sync.Mutex
	var out []Check
	add := func(c ...Check) { mu.Lock(); out = append(out, c...); mu.Unlock() }
	var wg sync.WaitGroup
	do := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	// Services that should run.
	do(func() {
		if s.System == nil {
			return
		}
		localAI := false
		if base, err := assist.LocalBase(st.Chat.LocalURL); err == nil && base != "" {
			localAI = strings.Contains(base, "127.0.0.1") || strings.Contains(base, "localhost")
		}
		var running []string
		for _, sv := range s.System.List(ctx) {
			needed := map[string]bool{"homeport": true, "caddy": true, "docker": true, "ollama": localAI, "avahi-daemon": true,
				"homeport-terminal": st.Terminal}[sv.ID]
			if !needed {
				continue
			}
			if sv.State == "running" {
				running = append(running, sv.Name)
				continue
			}
			add(Check{Title: sv.Name + " isn't running", State: "bad", Detail: strings.TrimSpace(sv.State+" "+sv.Detail) + ". " + sv.About,
				Fix: "restart:" + sv.ID, FixLabel: "Restart " + sv.Name, Guide: serviceGuides[sv.ID]})
		}
		if len(running) > 0 {
			add(Check{Title: "Services", State: "ok", Detail: strings.Join(running, ", ") + " running."})
		}
	})

	// The main address answers.
	do(func() {
		code, err := s.mainAnswers(ctx, st.MainDomain)
		switch {
		case err != nil:
			add(Check{Title: "The main address doesn't answer", State: "bad", Detail: st.MainDomain + ": " + err.Error(),
				Fix: "proxy", FixLabel: "Write Caddy's configuration again", Guide: "caddy-config"})
		case code >= 500:
			add(Check{Title: "The main address answers with an error", State: "bad", Detail: fmt.Sprintf("%s answered %d.", st.MainDomain, code),
				Fix: "proxy", FixLabel: "Write Caddy's configuration again", Guide: "caddy-config"})
		default:
			add(Check{Title: "Main address", State: "ok", Detail: st.MainDomain + " answers."})
		}
	})

	// Disk and memory.
	do(func() {
		spec := sysload.ReadSpecs(ctx, s.stateDir())
		if t := spec.Disk.Total; t > 0 {
			free := spec.Disk.Free
			pct := free * 100 / t
			c := Check{Title: "Disk space", State: "ok", Detail: fmt.Sprintf("%s free of %s (%d%%).", gib(free), gib(t), pct), Guide: "disk-full"}
			switch {
			case pct < 5 || free < 3<<30:
				c.Title, c.State = "The disk is nearly full", "bad"
			case pct < 10 || free < 10<<30:
				c.Title, c.State = "The disk is getting full", "warn"
			}
			if c.State == "ok" {
				c.Guide = ""
			}
			add(c)
		}
		l := s.load.Read(ctx)
		if l.MemTotal > 0 && l.MemUsed*100/l.MemTotal >= 95 {
			add(Check{Title: "Memory is almost all in use", State: "warn", Detail: fmt.Sprintf("%s of %s in use.", gib(l.MemUsed), gib(l.MemTotal)), Guide: "slow-server"})
		}
	})

	// Apps.
	for _, a := range apps {
		do(func() {
			if doing := unfinishedStatus[a.Unfinished]; doing != "" {
				add(Check{Title: a.Name + " is " + doing, State: "warn",
					Detail: "Under way now, or cut off when Homeport stopped: Homeport finishes it when it starts again (or press Update on the Apps page)."})
				return
			}
			switch status := s.Apps.Status(ctx, a.ID); status {
			case "running":
				add(Check{Title: a.Name, State: "ok", Detail: "Running."})
			case "stopped":
				add(Check{Title: a.Name + " is stopped", State: "warn", Detail: "If nobody stopped it on purpose, start it again.",
					Fix: "start:" + a.ID, FixLabel: "Start " + a.Name, Guide: "app-wont-start"})
			case "partly running":
				add(Check{Title: a.Name + " is only partly running", State: "bad", Detail: "Some of its parts stopped; its log usually says why.",
					Fix: "app:" + a.ID, FixLabel: "Restart " + a.Name, Guide: "app-wont-start"})
			default:
				add(Check{Title: a.Name + ": Docker doesn't say how it is", State: "warn", Detail: "Status " + status + ".", Guide: "docker-problems"})
			}
		})
	}

	// The internet, when something should be reachable from it.
	var internetApps bool
	s.Store.Read(func(d *state.Data) {
		internetApps = slices.ContainsFunc(apps, func(a state.App) bool { return d.OnInternet(a) })
	})
	var mainOnline bool
	s.Store.Read(func(d *state.Data) { mainOnline = !state.PrivateName(st.MainDomain) && !d.UnderHome(st.MainDomain) })
	if public != "" || internetApps || mainOnline {
		do(func() { add(s.internetChecks(ctx, st, public)...) })
	}

	// The home name: its certificate, and that devices here find it.
	var home state.Domain
	s.Store.Read(func(d *state.Data) {
		if x, ok := d.Domain(d.HomeName()); ok {
			home = *x
		}
	})
	if home.DynDNS != nil {
		do(func() { add(s.homeNameChecks(ctx, home)...) })
	}

	// Drives things were moved to.
	do(func() {
		missing := s.Apps.Missing()
		if s.platformName() != "macos" {
			missing = append(missing, dockerPlace.Missing(ctx)...)
		}
		if len(missing) > 0 {
			add(Check{Title: "A drive is missing", State: "bad",
				Detail: "These are on a drive that isn't there (unplugged, or not mounted): " + strings.Join(missing, ", ") + ". Plug it back in (or mount it on the Storage page), then restart what depends on it.",
				Guide:  "drive-missing"})
		}
	})

	// The local AI.
	if st.Chat.LocalURL != "" {
		do(func() {
			base, _ := assist.LocalBase(st.Chat.LocalURL)
			models, err := assist.LocalModels(ctx, base)
			local := strings.Contains(base, "127.0.0.1") || strings.Contains(base, "localhost")
			switch {
			case err != nil:
				c := Check{Title: "The local AI model server doesn't answer", State: "bad", Detail: err.Error(), Guide: "ollama-problems"}
				if local {
					c.Fix, c.FixLabel = "restart:ollama", "Restart Ollama"
				}
				add(c)
			case st.Chat.LocalModel != "" && !slices.Contains(models, st.Chat.LocalModel):
				add(Check{Title: "The chosen AI model is gone", State: "bad", Detail: st.Chat.LocalModel + " isn't on the model server any more: choose or download one on the AI models page.", Guide: "ollama-problems"})
			default:
				add(Check{Title: "Local AI", State: "ok", Detail: fmt.Sprintf("%d model(s) ready.", len(models))})
			}
		})
	}

	// Backups.
	if len(bk.Destinations) > 0 && bk.Time != "" && s.Backup != nil {
		do(func() { add(s.backupCheck(bk)) })
	}

	wg.Wait()
	rank := map[string]int{"bad": 0, "warn": 1, "ok": 2}
	sort.SliceStable(out, func(i, j int) bool { return rank[out[i].State] < rank[out[j].State] })
	run := CheckRun{At: time.Now(), Checks: out}
	for _, c := range out {
		switch c.State {
		case "bad":
			run.Problems++
		case "warn":
			run.Warnings++
		}
	}
	return run
}

// homeNameChecks: the home name's certificate is there and has time left, and the name leads here, as devices at
// home look it up (routers that refuse DNS answers with home addresses break it).
func (s *Server) homeNameChecks(ctx context.Context, home state.Domain) []Check {
	var out []Check
	dd := home.DynDNS
	until := certUntil(s.CertDir, home.Name)
	days := int(time.Until(until).Hours() / 24)
	switch {
	case until.IsZero():
		out = append(out, Check{Title: "No certificate for the home name yet", State: "warn",
			Detail: cmpOr(dd.CertError, "Homeport is getting one.") + " Until then browsers warn about " + home.Name + ".", Guide: "home-name"})
	case days < 14:
		out = append(out, Check{Title: "The home name's certificate runs out soon", State: "bad",
			Detail: fmt.Sprintf("In %d days. %s", days, cmpOr(dd.CertError, "Homeport renews it a month ahead; it hasn't managed yet.")), Guide: "home-name"})
	default:
		out = append(out, Check{Title: "Home name certificate", State: "ok", Detail: fmt.Sprintf("*.%s, valid for %d more days.", home.Name, days)})
	}
	if dd.Error != "" {
		out = append(out, Check{Title: "deSEC didn't take the last update for the home name", State: "bad", Detail: dd.Error, Guide: "home-name"})
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, "check."+home.Name)
	switch {
	case dd.IPv4 == "":
	case err != nil || len(addrs) == 0:
		out = append(out, Check{Title: "This network doesn't find the home name", State: "bad",
			Detail: home.Name + " points at " + dd.IPv4 + ", but looking it up here gives nothing. The router (or a DNS filter) probably refuses DNS answers that point at home addresses (\"DNS rebinding protection\").",
			Guide:  "home-name"})
	case !slices.Contains(addrs, dd.IPv4):
		out = append(out, Check{Title: "The home name leads somewhere else", State: "warn",
			Detail: fmt.Sprintf("%s leads to %s here, not to this server (%s). If this server's address just changed, it catches up within minutes.", home.Name, strings.Join(addrs, ", "), dd.IPv4),
			Guide:  "home-name"})
	default:
		out = append(out, Check{Title: "Home name", State: "ok", Detail: home.Name + " and every name under it lead here (" + dd.IPv4 + ")."})
	}
	return out
}

// mainAnswers opens the main address on this machine's HTTPS port, as a browser would.
func (s *Server) mainAnswers(ctx context.Context, domain string) (int, error) {
	addr := cmpOr(s.HTTPSAddr, "127.0.0.1:443")
	client := &http.Client{Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{ServerName: domain, InsecureSkipVerify: true},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, network, addr)
			}}}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://"+domain+"/", nil)
	if err != nil {
		return 0, err
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}

func (s *Server) internetChecks(ctx context.Context, st state.Settings, public string) []Check {
	in := s.readInternet(ctx)
	var out []Check
	elsewhere := slices.ContainsFunc(in.Forwards, func(f router.Forward) bool { return f.To != "" && !f.Mine })
	forwarded := allMine(in.Forwards)
	switch {
	case st.RouterPorts && in.Router.Found && !forwarded && !elsewhere:
		out = append(out, Check{Title: "The router stopped forwarding " + portsText(s.routerPorts()), State: "bad",
			Detail: "Homeport opened them, but the router doesn't send them here now (it may have restarted).",
			Fix:    "router", FixLabel: "Ask the router again", Guide: "internet-access"})
	case len(in.Steps) > 0:
		out = append(out, Check{Title: "Not everything is reachable from the internet", State: "warn",
			Detail: strings.Join(in.Steps, " "), Guide: "internet-access"})
	default:
		out = append(out, Check{Title: "Internet access", State: "ok", Detail: cmpOr(public, st.MainDomain) + " is reachable from the internet."})
	}
	type named struct {
		name string
		cert certs.Status
	}
	var list []named
	if public != "" {
		list = append(list, named{public, in.Cert})
	}
	for _, a := range in.Apps {
		if a.Internet {
			list = append(list, named{a.Domain, a.Cert})
		}
	}
	for _, c := range list {
		days := int(time.Until(c.cert.NotAfter).Hours() / 24)
		switch {
		case !c.cert.Trusted:
			out = append(out, Check{Title: "No trusted certificate for " + c.name + " yet", State: "warn",
				Detail: cmpOr(c.cert.Error, "Caddy hasn't got one from Let's Encrypt yet (the name must point here and the ports be forwarded)."),
				Fix:    "proxy", FixLabel: "Ask Caddy to try again", Guide: "certificate-failed"})
		case days < 7:
			out = append(out, Check{Title: "The certificate for " + c.name + " runs out soon", State: "bad",
				Detail: fmt.Sprintf("In %d days; Caddy should have renewed it.", days), Fix: "proxy", FixLabel: "Ask Caddy to try again", Guide: "certificate-failed"})
		}
	}
	return out
}

var backupFailed = regexp.MustCompile(`(?im)^(Fatal|error|.*\bfailed\b)`)

func (s *Server) backupCheck(bk state.Backup) Check {
	logText, _ := os.ReadFile(filepath.Join(s.Backup.Dir, "last-backup.log"))
	tail := string(logText)
	if len(tail) > 4000 {
		tail = tail[len(tail)-4000:]
	}
	if m := backupFailed.FindString(tail); m != "" {
		return Check{Title: "The last backup failed", State: "bad", Detail: strings.TrimSpace(m), Guide: "backups-failing"}
	}
	if last, err := time.ParseInLocation("2006-01-02", bk.LastScheduled, time.Local); bk.LastScheduled == "" || err == nil && time.Since(last) > 50*time.Hour {
		return Check{Title: "The daily backup hasn't run lately", State: "warn",
			Detail: "Last scheduled run: " + cmpOr(bk.LastScheduled, "never") + ".", Guide: "backups-failing"}
	}
	return Check{Title: "Backups", State: "ok", Detail: "Last daily backup: " + bk.LastScheduled + "."}
}

// fixProblem makes one of the repairs the checks offer.
func (s *Server) fixProblem(ctx context.Context, action string) error {
	kind, arg, _ := strings.Cut(action, ":")
	switch kind {
	case "restart":
		if s.System == nil {
			return errors.New("services can't be restarted here")
		}
		return s.System.Restart(ctx, arg)
	case "proxy":
		if s.ApplyProxy == nil {
			return errors.New("Caddy's configuration can't be written here")
		}
		return s.ApplyProxy(ctx)
	case "router":
		return s.reopenRouter(ctx)
	case "app":
		return s.Apps.AppAction(ctx, arg, "restart")
	case "start":
		return s.Apps.AppAction(ctx, arg, "start")
	}
	return fmt.Errorf("Homeport has no repair called %q", action)
}

// reopenRouter asks the router again for its forwardings (80 and 443, or the home name's port), when Homeport was allowed to keep them open.
func (s *Server) reopenRouter(ctx context.Context) error {
	var on bool
	s.Store.Read(func(d *state.Data) { on = d.Settings().RouterPorts })
	if !on {
		return errors.New("Homeport isn't allowed to open router ports: an admin can do that on the Internet access page")
	}
	rt, err := router.Find(ctx)
	if err != nil {
		return err
	}
	return openRouterPorts(ctx, rt, s.routerPorts())
}

func (s *Server) lastChecks() *CheckRun {
	s.checks.mu.Lock()
	defer s.checks.mu.Unlock()
	if s.checks.last == nil {
		var run CheckRun
		if b, err := os.ReadFile(filepath.Join(s.stateDir(), "last-checks.json")); err == nil && json.Unmarshal(b, &run) == nil {
			s.checks.last = &run
		}
	}
	return s.checks.last
}

func (s *Server) keepChecks(run CheckRun) {
	s.checks.mu.Lock()
	s.checks.last = &run
	s.checks.mu.Unlock()
	if b, err := json.Marshal(run); err == nil {
		_ = os.WriteFile(filepath.Join(s.stateDir(), "last-checks.json"), b, 0o600)
	}
}

func (s *Server) lastChecksView(w http.ResponseWriter, r *http.Request) {
	run := s.lastChecks()
	if run == nil {
		writeJSON(w, http.StatusOK, map[string]any{"daily": !s.settings().NoDailyCheck})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "daily": !s.settings().NoDailyCheck})
}

func (s *Server) runChecksNow(w http.ResponseWriter, r *http.Request) {
	run := s.RunChecks(r.Context())
	s.keepChecks(run)
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "daily": !s.settings().NoDailyCheck})
}

func (s *Server) fixCheck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Fix string `json:"fix"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	log.Printf("Checks: %s asked for the repair %q", actor(r).Name, in.Fix)
	if err := s.fixProblem(ctx, in.Fix); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DailyChecks checks the server a few minutes after Homeport starts, then every day (unless an admin turned that
// off), and writes any problems into the assistants' notebook so the next one sees them.
func (s *Server) DailyChecks(ctx context.Context) {
	setupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	s.SetupText(setupCtx)
	cancel()
	wait := 3 * time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 24 * time.Hour
		if s.settings().NoDailyCheck {
			continue
		}
		run := s.RunChecks(ctx)
		s.keepChecks(run)
		var lines []string
		for _, c := range run.Checks {
			if c.State != "ok" {
				lines = append(lines, fmt.Sprintf("- %s: %s (%s)", c.Title, c.Detail, c.State))
			}
		}
		note := "All fine on " + run.At.Format("2006-01-02 15:04") + "."
		if len(lines) > 0 {
			note = "Found on " + run.At.Format("2006-01-02 15:04") + ":\n" + strings.Join(lines, "\n")
			log.Printf("Daily check: %d problem(s), %d warning(s)", run.Problems, run.Warnings)
		}
		if s.Assist != nil && s.Assist.Memory != nil {
			_, _ = s.Assist.Memory.RewriteTopic("Daily check", note)
		}
	}
}

// MARK: Tools for assistants

// capture keeps what one of the admin panel's handlers answers.
type capture struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (c *capture) Header() http.Header {
	if c.header == nil {
		c.header = http.Header{}
	}
	return c.header
}
func (c *capture) WriteHeader(code int) { c.code = code }
func (c *capture) Write(b []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	return c.body.Write(b)
}

// look runs one of the admin panel's own GET handlers for an assistant, so tools see exactly what the pages show.
func (s *Server) look(ctx context.Context, h http.HandlerFunc, target string) (string, error) {
	r, err := http.NewRequestWithContext(ctx, "GET", "http://homeport"+target, nil)
	if err != nil {
		return "", err
	}
	r = r.WithContext(context.WithValue(ctx, actorKey{}, state.User{Name: "assistant", Admin: true}))
	var c capture
	h(&c, r)
	if c.code/100 != 2 {
		return "", errors.New(strings.TrimSpace(c.body.String()))
	}
	return c.body.String(), nil
}

// AssistantTools are the tools that need the server: the setup page, the checks, the newer pages, and small repairs.
func (s *Server) AssistantTools() []assist.Tool {
	lookTool := func(name, desc string, h http.HandlerFunc, target string) assist.Tool {
		return assist.Tool{Name: name, Level: assist.Look, Description: desc,
			Run: func(ctx context.Context, _ assist.Args) (string, error) { return s.look(ctx, h, target) }}
	}
	services := []string{"homeport", "caddy", "docker", "ollama", "avahi-daemon"}
	if s.platformName() == "macos" {
		services = []string{"homeport", "caddy", "ollama"}
	}
	return []assist.Tool{
		{Name: "server_setup", Level: assist.Look,
			Description: "How this server is set up: the machine, where Homeport's files are, what runs and how, addresses and certificates, apps, local AI, backups, firewall, and what the installer did. Read it before changing anything.",
			Run:         func(ctx context.Context, _ assist.Args) (string, error) { return s.SetupText(ctx), nil }},
		{Name: "run_checks", Level: assist.Look,
			Description: "Checks the whole server now (services, the main address, disk, apps, internet access and certificates, the local AI, backups) and lists what's wrong, each with the guide entry about it and the repair Homeport can make. Takes up to a minute.",
			Run: func(ctx context.Context, _ assist.Args) (string, error) {
				run := s.RunChecks(ctx)
				s.keepChecks(run)
				var b strings.Builder
				fmt.Fprintf(&b, "%d problem(s), %d warning(s).\n", run.Problems, run.Warnings)
				for _, c := range run.Checks {
					fmt.Fprintf(&b, "- [%s] %s: %s", c.State, c.Title, c.Detail)
					if c.Fix != "" {
						fmt.Fprintf(&b, " Repair: %s.", fixTool(c.Fix))
					}
					if c.Guide != "" {
						fmt.Fprintf(&b, " Guide: %q.", c.Guide)
					}
					b.WriteString("\n")
				}
				return b.String(), nil
			}},
		lookTool("internet_status", "Every link between the internet and this server: its public address, the router (and where it sends ports 80/443), the internet name and where it points, certificates, which apps are on the internet, and what's left to do.", s.internetStatus, "/api/internet"),
		lookTool("firewall_status", "The firewall: on or off, the open ports and who may use them, and ports apps publish themselves.", s.firewallStatus, "/api/firewall"),
		lookTool("backup_status", "Backups: schedule, destinations (without their keys), what's skipped, whether one is running, and the last backup's log.", s.backupOverview, "/api/backup"),
		lookTool("services_status", "Every app's containers (state, memory, processor) and the system services Homeport depends on (Homeport, Caddy, Docker, Ollama, Avahi).", s.listServices, "/api/services"),
		lookTool("ai_models_status", "The local AI: the model server, installed models with sizes and speeds, the one in use, this machine's hardware, and downloads in progress.", s.aiModels, "/api/ai-models"),

		{Name: "restart_service", Level: assist.Change,
			Description: "Restart one of the system services Homeport depends on, when it has stopped or misbehaves: " + strings.Join(services, ", ") + ".",
			Params:      map[string]any{"service": map[string]any{"type": "string", "enum": services, "description": "Which service."}},
			Required:    []string{"service"},
			Summary:     func(a assist.Args) string { return "Restart the " + a.Str("service") + " service" },
			Run: func(ctx context.Context, a assist.Args) (string, error) {
				if !slices.Contains(services, a.Str("service")) {
					return "", fmt.Errorf("%q isn't one of: %s", a.Str("service"), strings.Join(services, ", "))
				}
				if err := s.fixProblem(ctx, "restart:"+a.Str("service")); err != nil {
					return "", err
				}
				return "Restarting " + a.Str("service") + ". Check services_status in a few seconds.", nil
			}},
		{Name: "rebuild_proxy_config", Level: assist.Change,
			Description: "Write Caddy's configuration again from Homeport's settings and reload Caddy: fixes a configuration that went wrong, and makes Caddy try again for certificates.",
			Summary:     func(assist.Args) string { return "Write Caddy's configuration again and reload Caddy" },
			Run: func(ctx context.Context, _ assist.Args) (string, error) {
				if err := s.fixProblem(ctx, "proxy"); err != nil {
					return "", err
				}
				return "Caddy's configuration was written again and reloaded.", nil
			}},
		{Name: "reopen_router_ports", Level: assist.Change,
			Description: "Ask the home router again to forward ports 80 and 443 here (UPnP), when Homeport was allowed to keep them open and the router forgot (after a restart, say). Never takes over ports the router sends to another device.",
			Summary:     func(assist.Args) string { return "Ask the router again to forward ports 80 and 443 to this server" },
			Run: func(ctx context.Context, _ assist.Args) (string, error) {
				if err := s.fixProblem(ctx, "router"); err != nil {
					return "", err
				}
				return "The router forwards 80 and 443 here again.", nil
			}},
	}
}

// fixTool says which tool makes a check's repair.
func fixTool(fix string) string {
	kind, arg, _ := strings.Cut(fix, ":")
	switch kind {
	case "restart":
		return "restart_service " + arg
	case "proxy":
		return "rebuild_proxy_config"
	case "router":
		return "reopen_router_ports"
	case "app":
		return "restart_app " + arg
	case "start":
		return "the Start button on the Apps page (or restart_app " + arg + ")"
	}
	return fix
}
