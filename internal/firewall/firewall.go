// Package firewall is the Firewall page's side of ufw (Ubuntu's firewall): turning it on and off, and opening and
// closing ports. It keeps the ports that reach the server itself open whatever happens: SSH (on the port sshd really
// uses), and 80 and 443 for every site. Turning the firewall on allows those first, and they can't be closed here, so
// nobody locks themselves out from the admin panel.
package firewall

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"homeport/internal/runner"
)

// Rule is one "allow" rule.
type Rule struct {
	Port      string `json:"port"`            // "8080", or "6000:6010"
	Proto     string `json:"proto,omitempty"` // tcp, udp, or "" for both
	From      string `json:"from,omitempty"`  // an address range, or "" for anywhere
	Comment   string `json:"comment,omitempty"`
	Protected bool   `json:"protected,omitempty"` // SSH, 80 and 443: always open
	spec      []string
}

// Published is a port an app's container publishes on the network by itself (Docker opens these whatever ufw says).
type Published struct {
	Container string `json:"container"`
	Port      string `json:"port"`
}

type Status struct {
	Available bool        `json:"available"`
	Why       string      `json:"why,omitempty"` // when it isn't
	Active    bool        `json:"active"`
	SSHPort   int         `json:"sshPort"`
	Rules     []Rule      `json:"rules"`
	Networks  []string    `json:"networks"`  // this machine's local networks, for "from this network only"
	Published []Published `json:"published"` // by apps, directly
}

type Manager struct {
	Runner runner.Runner
	OS     string
}

// SSHPort is the port sshd listens on (22 unless it was changed).
func (m *Manager) SSHPort(ctx context.Context) int {
	out, _ := m.Runner.Output(ctx, "", "sshd", "-T")
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "port" {
			if n, err := strconv.Atoi(f[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return 22
}

func (m *Manager) protected(ssh int) []Rule {
	return []Rule{
		{Port: strconv.Itoa(ssh), Proto: "tcp", Comment: "SSH", Protected: true},
		{Port: "80", Proto: "tcp", Comment: "Websites (HTTP, and certificates)", Protected: true},
		{Port: "443", Proto: "tcp", Comment: "Websites (HTTPS)", Protected: true},
		{Port: "443", Proto: "udp", Comment: "Websites (HTTP/3)", Protected: true},
	}
}

func (m *Manager) Status(ctx context.Context) Status {
	st := Status{Rules: []Rule{}, Networks: LocalNetworks(), Published: m.published(ctx)}
	if m.OS != "linux" {
		st.Why = "macOS has its own firewall: System Settings → Network → Firewall."
		return st
	}
	if _, err := lookPath("ufw"); err != nil {
		st.Why = "ufw (Ubuntu's firewall) isn't installed: sudo apt-get install ufw"
		return st
	}
	st.Available, st.SSHPort = true, m.SSHPort(ctx)
	if out, _ := m.Runner.Output(ctx, "", "ufw", "status"); strings.Contains(out, "Status: active") {
		st.Active = true
	}
	out, _ := m.Runner.Output(ctx, "", "ufw", "show", "added")
	protected := m.protected(st.SSHPort)
	for _, line := range strings.Split(out, "\n") {
		r, ok := parseAdded(line)
		if !ok {
			continue
		}
		if r.Port == "OpenSSH" { // Ubuntu's application profile for SSH
			r.Protected, r.Comment = true, cmp.Or(r.Comment, "SSH")
		}
		for _, p := range protected {
			if r.Port == p.Port && (r.Proto == p.Proto || r.Proto == "") && r.From == "" {
				r.Protected = true
				if r.Comment == "" {
					r.Comment = p.Comment
				}
			}
		}
		st.Rules = append(st.Rules, r)
	}
	return st
}

// parseAdded reads one line of "ufw show added": ufw allow 80/tcp, ufw allow from 192.168.0.0/24 to any port 8080
// proto tcp comment 'Homeport: x'. Only allow rules are Homeport's business; others are left alone (and not shown).
var commentRe = regexp.MustCompile(` comment '([^']*)'$`)

func parseAdded(line string) (Rule, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "ufw allow ") {
		return Rule{}, false
	}
	var r Rule
	if m := commentRe.FindStringSubmatch(line); m != nil {
		r.Comment = m[1]
		line = strings.TrimSuffix(line, m[0])
	}
	f := strings.Fields(strings.TrimPrefix(line, "ufw "))
	r.spec = f
	args := f[1:] // after "allow"
	switch {
	case len(args) == 1: // 80/tcp, 22, OpenSSH
		r.Port, r.Proto, _ = strings.Cut(args[0], "/")
	default:
		for i := 0; i+1 < len(args); i++ {
			switch args[i] {
			case "from":
				if args[i+1] != "any" {
					r.From = args[i+1]
				}
			case "port":
				r.Port = args[i+1]
			case "proto":
				r.Proto = args[i+1]
			}
		}
	}
	if r.Port == "" {
		return Rule{}, false
	}
	return r, true
}

var portRe = regexp.MustCompile(`^[0-9]{1,5}(:[0-9]{1,5})?$`)

// Open allows a port (or range "6000:6010") for tcp, udp or both (""), from anywhere or one of this machine's local
// networks.
func (m *Manager) Open(ctx context.Context, port, proto, from, label string) error {
	if err := m.usable(); err != nil {
		return err
	}
	if !portRe.MatchString(port) {
		return errors.New("a port is a number from 1 to 65535, or a range like 6000:6010")
	}
	for _, p := range strings.Split(port, ":") {
		if n, _ := strconv.Atoi(p); n < 1 || n > 65535 {
			return errors.New("ports go from 1 to 65535")
		}
	}
	if proto != "" && proto != "tcp" && proto != "udp" {
		return errors.New(`proto is "tcp", "udp" or "" for both`)
	}
	if strings.Contains(port, ":") && proto == "" {
		return errors.New("a range of ports needs tcp or udp")
	}
	if from != "" && !slices.Contains(LocalNetworks(), from) {
		return errors.New("only from anywhere, or from one of this server's own networks")
	}
	label = strings.Map(func(r rune) rune {
		if r == '\'' || r < 0x20 {
			return -1
		}
		return r
	}, strings.TrimSpace(label))
	if len(label) > 60 {
		label = label[:60]
	}
	var args []string
	if from == "" {
		spec := port
		if proto != "" {
			spec += "/" + proto
		}
		args = []string{"allow", spec}
	} else {
		args = []string{"allow", "from", from, "to", "any", "port", port}
		if proto != "" {
			args = append(args, "proto", proto)
		}
	}
	args = append(args, "comment", "Homeport: "+cmp.Or(label, "opened in the admin panel"))
	return m.ufw(ctx, args...)
}

// Close removes an allow rule, unless it's one of the protected ones.
func (m *Manager) Close(ctx context.Context, port, proto, from string) error {
	if err := m.usable(); err != nil {
		return err
	}
	st := m.Status(ctx)
	for _, r := range st.Rules {
		if r.Port == port && r.Proto == proto && r.From == from {
			if r.Protected {
				return errors.New("SSH, 80 and 443 stay open: closing them could lock you out, or take every site down")
			}
			return m.ufw(ctx, append([]string{"delete"}, r.spec...)...)
		}
	}
	return errors.New("there's no such rule")
}

// TurnOn allows the protected ports first, then turns the firewall on (everything else coming in is refused).
func (m *Manager) TurnOn(ctx context.Context) error {
	if err := m.usable(); err != nil {
		return err
	}
	for _, p := range m.protected(m.SSHPort(ctx)) {
		if err := m.ufw(ctx, "allow", p.Port+"/"+p.Proto, "comment", "Homeport: "+p.Comment); err != nil {
			return err
		}
	}
	if err := m.ufw(ctx, "default", "deny", "incoming"); err != nil {
		return err
	}
	return m.ufw(ctx, "--force", "enable")
}

func (m *Manager) TurnOff(ctx context.Context) error {
	if err := m.usable(); err != nil {
		return err
	}
	return m.ufw(ctx, "disable")
}

func (m *Manager) usable() error {
	if m.OS != "linux" {
		return errors.New("the firewall page is for Linux servers (macOS has its own firewall)")
	}
	if _, err := lookPath("ufw"); err != nil {
		return errors.New("ufw isn't installed")
	}
	return nil
}

func (m *Manager) ufw(ctx context.Context, args ...string) error {
	if out, err := m.Runner.Output(ctx, "", "ufw", args...); err != nil {
		return fmt.Errorf("ufw %s: %v %s", strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return nil
}

// published is ports apps' containers publish on the network themselves (not just on 127.0.0.1).
func (m *Manager) published(ctx context.Context) []Published {
	out, _ := m.Runner.Output(ctx, "", "docker", "ps", "--format", "{{.Names}}\t{{.Ports}}")
	list := []Published{}
	for _, line := range strings.Split(out, "\n") {
		name, ports, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		seen := map[string]bool{}
		for _, p := range strings.Split(ports, ", ") {
			// 0.0.0.0:22000->22000/tcp, [::]:22000->22000/tcp, 127.0.0.1:18001->80/tcp
			host, _, ok := strings.Cut(p, "->")
			if !ok || strings.HasPrefix(host, "127.0.0.1:") || strings.HasPrefix(host, "[::1]:") {
				continue
			}
			i := strings.LastIndex(host, ":")
			proto := p[strings.LastIndex(p, "/")+1:]
			port := host[i+1:] + "/" + proto
			if !seen[port] {
				seen[port] = true
				list = append(list, Published{Container: name, Port: port})
			}
		}
	}
	return list
}

// LocalNetworks are this machine's own IPv4 networks (192.168.0.0/24, say), not Docker's or VPNs'.
func LocalNetworks() []string {
	ifs, _ := net.Interfaces()
	skip := []string{"lo", "docker", "br-", "veth", "virbr", "bridge", "utun", "tun", "tap", "wg", "tailscale", "zt", "vmenet", "awdl", "llw", "anpi", "ap1"}
	out := []string{}
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 || slices.ContainsFunc(skip, func(p string) bool { return strings.HasPrefix(i.Name, p) }) {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				network := &net.IPNet{IP: n.IP.Mask(n.Mask), Mask: n.Mask}
				if !slices.Contains(out, network.String()) {
					out = append(out, network.String())
				}
			}
		}
	}
	return out
}

// lookPath finds a program (tests stand in for it).
var lookPath = exec.LookPath
