// Package system lists, restarts and reads the logs of the server's own services, for the Services page: on Linux the
// systemd units Homeport relies on, on a Mac Homeport's launchd agents. Only these known services, never anything else.
package system

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"homeport/internal/runner"
)

// Service is one of the server's services.
type Service struct {
	ID         string    `json:"id"`   // systemd unit or agent name
	Name       string    `json:"name"` // for people
	About      string    `json:"about"`
	State      string    `json:"state"` // running, stopped, failed, starting, unknown
	Detail     string    `json:"detail,omitempty"`
	Since      time.Time `json:"since,omitzero"`
	Memory     int64     `json:"memory,omitempty"` // bytes
	Restart    string    `json:"restart"`          // what restarting it does
	CanRestart bool      `json:"canRestart"`
	HasLogs    bool      `json:"hasLogs"`
}

type known struct{ id, name, about, restart string }

var linuxServices = []known{
	{"homeport", "Homeport", "The portal, admin panel, sign-in and this page.", "The admin panel comes back after a few seconds."},
	{"caddy", "Caddy", "The web front door: every site's address and certificate.", "Sites are unreachable for a moment."},
	{"docker", "Docker", "Runs the apps.", "Every app stops and starts again."},
	{"ollama", "Ollama", "Runs the local AI model.", "The model loads again with the next question."},
	{"avahi-daemon", "Avahi", "Announces .local names (homeport.local) on the home network.", ".local names vanish for a few seconds."},
	{"homeport-terminal", "Web terminal", "The terminal in the admin panel.", "Open terminals are disconnected."},
}

var macServices = []known{
	{"homeport", "Homeport", "The portal, admin panel, sign-in and this page.", "The admin panel comes back after a few seconds."},
	{"caddy", "Caddy", "The web front door: every site's address and certificate.", "Sites are unreachable for a moment."},
	{"ollama", "Ollama", "Runs the local AI model (when Homeport started it).", "The model loads again with the next question."},
}

// Manager reads and restarts the services.
type Manager struct {
	Runner runner.Runner
	OS     string // "linux" or "macos"
	UID    int    // the Mac user whose launchd agents these are
	// Logs is the platform's command for a service's recent log lines.
	Logs func(service string, lines int) []string
}

func (m *Manager) known() []known {
	if m.OS == "linux" {
		return linuxServices
	}
	return macServices
}

func (m *Manager) find(id string) (known, bool) {
	for _, k := range m.known() {
		if k.id == id {
			return k, true
		}
	}
	return known{}, false
}

// List is the services this server has (ones not installed are left out).
func (m *Manager) List(ctx context.Context) []Service {
	if m.OS == "linux" {
		return m.listLinux(ctx)
	}
	return m.listMac(ctx)
}

func (m *Manager) listLinux(ctx context.Context) []Service {
	args := []string{"show", "--property=Id,LoadState,ActiveState,SubState,ActiveEnterTimestamp,MemoryCurrent,Result"}
	for _, k := range linuxServices {
		args = append(args, k.id+".service")
	}
	out, _ := m.Runner.Output(ctx, "", "systemctl", args...)
	blocks := strings.Split(strings.TrimSpace(out), "\n\n")
	var list []Service
	for i, k := range linuxServices {
		if i >= len(blocks) {
			break
		}
		p := map[string]string{}
		for _, line := range strings.Split(blocks[i], "\n") {
			if key, val, ok := strings.Cut(line, "="); ok {
				p[key] = val
			}
		}
		if p["LoadState"] != "loaded" {
			continue
		}
		s := Service{ID: k.id, Name: k.name, About: k.about, Restart: k.restart, CanRestart: true, HasLogs: true}
		switch p["ActiveState"] {
		case "active":
			s.State = "running"
		case "failed":
			s.State, s.Detail = "failed", p["Result"]
		case "activating", "reloading":
			s.State = "starting"
		default:
			s.State = "stopped"
		}
		if s.Detail == "" && p["SubState"] != "" && p["SubState"] != "running" {
			s.Detail = p["SubState"]
		}
		if t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", p["ActiveEnterTimestamp"]); err == nil && s.State == "running" {
			s.Since = t
		}
		if n, err := strconv.ParseInt(p["MemoryCurrent"], 10, 64); err == nil {
			s.Memory = n
		}
		list = append(list, s)
	}
	return list
}

func (m *Manager) listMac(ctx context.Context) []Service {
	var list []Service
	for _, k := range macServices {
		out, err := m.Runner.Output(ctx, "", "launchctl", "print", m.label(k.id))
		if err != nil {
			continue // not set up on this Mac
		}
		s := Service{ID: k.id, Name: k.name, About: k.about, Restart: k.restart, CanRestart: true, HasLogs: true, State: "stopped"}
		for _, line := range strings.Split(out, "\n") {
			key, val, ok := strings.Cut(strings.TrimSpace(line), " = ")
			if !ok {
				continue
			}
			switch key {
			case "state":
				if val == "running" {
					s.State = "running"
				}
			case "last exit code":
				if val != "0" && val != "(never exited)" && s.Detail == "" {
					s.Detail = "last exit code " + val
				}
			}
		}
		list = append(list, s)
	}
	return list
}

func (m *Manager) label(id string) string { return fmt.Sprintf("gui/%d/dev.homeport.%s", m.UID, id) }

// Restart restarts one of the known services. It returns once the restart is under way (Homeport restarting itself
// would otherwise cut its own answer off).
func (m *Manager) Restart(ctx context.Context, id string) error {
	if _, ok := m.find(id); !ok {
		return errors.New("that isn't one of the services Homeport looks after")
	}
	var cmd []string
	if m.OS == "linux" {
		cmd = []string{"systemctl", "restart", "--no-block", id + ".service"}
	} else {
		cmd = []string{"launchctl", "kickstart", "-k", m.label(id)}
		if id == "homeport" {
			// kickstart -k waits for the old process to end: that's this one. Let the answer go out first.
			go func() {
				time.Sleep(500 * time.Millisecond)
				_, _ = m.Runner.Output(context.Background(), "", cmd[0], cmd[1:]...)
			}()
			return nil
		}
	}
	if out, err := m.Runner.Output(ctx, "", cmd[0], cmd[1:]...); err != nil {
		return fmt.Errorf("couldn't restart %s: %v %s", id, err, strings.TrimSpace(out))
	}
	return nil
}

// RecentLogs is a service's last lines of log.
func (m *Manager) RecentLogs(ctx context.Context, id string, lines int) (string, error) {
	if _, ok := m.find(id); !ok {
		return "", errors.New("that isn't one of the services Homeport looks after")
	}
	if m.Logs == nil {
		return "", errors.New("logs aren't available on this system")
	}
	c := m.Logs(id, lines)
	return m.Runner.Output(ctx, "", c[0], c[1:]...)
}
