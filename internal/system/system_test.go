package system

import (
	"context"
	"io"
	"strings"
	"testing"
)

type fake struct{ calls []string }

func (f *fake) Output(_ context.Context, _, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	if strings.HasPrefix(cmd, "systemctl show") {
		// One block per unit asked for, in order; ollama isn't installed, docker failed.
		return `Id=homeport.service
LoadState=loaded
ActiveState=active
SubState=running
ActiveEnterTimestamp=Sun 2026-10-11 23:17:50 UTC
MemoryCurrent=27000000
Result=success

Id=caddy.service
LoadState=loaded
ActiveState=active
SubState=running
ActiveEnterTimestamp=Sun 2026-10-11 22:37:27 UTC
MemoryCurrent=[not set]
Result=success

Id=docker.service
LoadState=loaded
ActiveState=failed
SubState=failed
ActiveEnterTimestamp=
MemoryCurrent=[not set]
Result=exit-code

Id=ollama.service
LoadState=not-found
ActiveState=inactive
SubState=dead
ActiveEnterTimestamp=
MemoryCurrent=[not set]
Result=success

Id=avahi-daemon.service
LoadState=loaded
ActiveState=active
SubState=running
ActiveEnterTimestamp=Sun 2026-10-11 23:44:32 UTC
MemoryCurrent=1200000
Result=success

Id=homeport-terminal.service
LoadState=not-found
ActiveState=inactive
SubState=dead
ActiveEnterTimestamp=
MemoryCurrent=[not set]
Result=success`, nil
	}
	return "", nil
}
func (f *fake) Stream(context.Context, string, io.Writer, string, ...string) error { return nil }

func TestLinuxServices(t *testing.T) {
	f := &fake{}
	m := &Manager{Runner: f, OS: "linux", Logs: func(s string, n int) []string { return []string{"journalctl", "-u", s} }}
	list := m.List(context.Background())
	var got []string
	for _, s := range list {
		got = append(got, s.ID+":"+s.State)
	}
	if strings.Join(got, " ") != "homeport:running caddy:running docker:failed avahi-daemon:running" {
		t.Fatalf("services: %v", got)
	}
	if list[0].Memory != 27000000 || list[0].Since.IsZero() || list[2].Detail != "exit-code" {
		t.Fatalf("details: %+v", list)
	}
	if err := m.Restart(context.Background(), "caddy"); err != nil || f.calls[len(f.calls)-1] != "systemctl restart --no-block caddy.service" {
		t.Fatalf("restart: %v %v", err, f.calls)
	}
	if err := m.Restart(context.Background(), "sshd"); err == nil {
		t.Error("only the services Homeport looks after can be restarted")
	}
	if _, err := m.RecentLogs(context.Background(), "../../etc/passwd", 10); err == nil {
		t.Error("logs only for known services")
	}
}
