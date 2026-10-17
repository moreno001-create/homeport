package system

import (
	"context"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// MARK: Linux updates

// PackageUpdate is one package with a newer version waiting.
type PackageUpdate struct {
	Name     string `json:"name"`
	From     string `json:"from"`
	To       string `json:"to"`
	Security bool   `json:"security"`
}

// SystemUpdates is what's waiting for the system itself (Ubuntu's packages).
type SystemUpdates struct {
	Supported    bool            `json:"supported"`
	Why          string          `json:"why,omitempty"` // when it isn't
	Packages     []PackageUpdate `json:"packages"`
	Security     int             `json:"security"`
	RebootNeeded bool            `json:"rebootNeeded"`
	RebootFor    []string        `json:"rebootFor,omitempty"` // the packages that want it
	AutoSecurity bool            `json:"autoSecurity"`        // security updates install by themselves (unattended-upgrades)
	Restarts     []string        `json:"restarts,omitempty"`  // services an upgrade restarts that matter here (docker…)
	Checked      time.Time       `json:"checked"`
}

// upgradable: "name/suite[,suite] version arch [upgradable from: old]"
var upgradable = regexp.MustCompile(`^([^/\s]+)/(\S+)\s+(\S+)\s+\S+\s+\[upgradable from: ([^\]]+)\]`)

// Updates lists the system's waiting updates; refresh asks the package servers first (apt-get update).
func (m *Manager) Updates(ctx context.Context, refresh bool) (SystemUpdates, error) {
	u := SystemUpdates{Packages: []PackageUpdate{}}
	if m.OS != "linux" {
		u.Why = "On a Mac, system updates are in System Settings → General → Software Update"
		return u, nil
	}
	u.Supported = true
	if refresh {
		if out, err := m.Runner.Output(ctx, "", "apt-get", "update", "-q"); err != nil {
			return u, errors.New("couldn't ask the package servers: " + lastLine(out, err))
		}
	}
	out, err := m.Runner.Output(ctx, "", "apt", "list", "--upgradable")
	if err != nil && out == "" {
		return u, err
	}
	for _, line := range strings.Split(out, "\n") {
		mt := upgradable.FindStringSubmatch(strings.TrimSpace(line))
		if mt == nil {
			continue
		}
		p := PackageUpdate{Name: mt[1], To: mt[3], From: mt[4], Security: strings.Contains(mt[2], "-security")}
		if p.Security {
			u.Security++
		}
		u.Packages = append(u.Packages, p)
		for _, svc := range []string{"docker", "containerd", "caddy", "ollama", "avahi"} {
			if strings.HasPrefix(p.Name, svc) && !contains(u.Restarts, svc) {
				u.Restarts = append(u.Restarts, svc)
			}
		}
	}
	if _, err := os.Stat("/var/run/reboot-required"); err == nil {
		u.RebootNeeded = true
		if b, err := os.ReadFile("/var/run/reboot-required.pkgs"); err == nil {
			u.RebootFor = strings.Fields(string(b))
		}
	}
	if b, err := os.ReadFile("/etc/apt/apt.conf.d/20auto-upgrades"); err == nil {
		u.AutoSecurity = regexp.MustCompile(`Unattended-Upgrade\s+"1"`).Match(b)
	}
	u.Checked = time.Now()
	return u, nil
}

// Upgrade installs the waiting updates, writing apt's output to w. It runs as its own system job (systemd-run), outside
// Homeport's sandbox, which may not make setuid files (some packages have them). Changed configuration files keep
// the server's own version.
func (m *Manager) Upgrade(ctx context.Context, w io.Writer) error {
	if m.OS != "linux" {
		return errors.New("on a Mac, use System Settings → Software Update")
	}
	return m.Runner.Stream(ctx, "", w, "systemd-run", "--quiet", "--wait", "--pipe", "--collect",
		"--property=Type=exec", "--setenv=DEBIAN_FRONTEND=noninteractive", "--setenv=NEEDRESTART_MODE=a",
		"apt-get", "-y", "-q", "-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold", "upgrade")
}

// Reboot restarts the machine a few seconds from now (so the answer gets out first).
func (m *Manager) Reboot() error {
	if m.OS != "linux" {
		return errors.New("restart a Mac from its Apple menu")
	}
	go func() {
		time.Sleep(3 * time.Second)
		_, _ = m.Runner.Output(context.Background(), "", "systemctl", "reboot")
	}()
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func lastLine(out string, err error) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return l
	}
	return err.Error()
}
