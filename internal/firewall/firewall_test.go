package firewall

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
)

type fake struct {
	calls  []string
	active bool
	added  string
}

func (f *fake) Output(_ context.Context, _, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	switch {
	case cmd == "sshd -T":
		return "port 2222\naddressfamily any\n", nil
	case cmd == "ufw status":
		if f.active {
			return "Status: active\n", nil
		}
		return "Status: inactive\n", nil
	case cmd == "ufw show added":
		return f.added, nil
	case strings.HasPrefix(cmd, "docker ps"):
		return "hp-syncthing-app-1\t127.0.0.1:18004->8384/tcp, 0.0.0.0:22000->22000/tcp, [::]:22000->22000/tcp, 0.0.0.0:21027->21027/udp\nhp-wiki-app-1\t127.0.0.1:18005->3000/tcp\n", nil
	}
	return "", nil
}
func (f *fake) Stream(context.Context, string, io.Writer, string, ...string) error { return nil }

func manager(f *fake) *Manager {
	lookPath = func(string) (string, error) { return "/usr/sbin/ufw", nil }
	return &Manager{Runner: f, OS: "linux"}
}

func TestStatus(t *testing.T) {
	f := &fake{active: true, added: `Added user rules (see 'ufw status' for running firewall):
ufw allow 2222/tcp comment 'Homeport: SSH'
ufw allow 80/tcp
ufw allow OpenSSH
ufw allow 22000 comment 'Homeport: Syncthing'
ufw allow from 192.168.0.0/24 to any port 8080 proto tcp comment 'Homeport: test'
ufw deny 25/tcp
`}
	st := manager(f).Status(context.Background())
	if !st.Available || !st.Active || st.SSHPort != 2222 || len(st.Rules) != 5 {
		t.Fatalf("status: %+v", st)
	}
	r := st.Rules
	if !r[0].Protected || r[0].Comment != "Homeport: SSH" || !r[1].Protected || !r[2].Protected || r[3].Protected || r[3].Proto != "" {
		t.Fatalf("protected: %+v", r)
	}
	if r[4].From != "192.168.0.0/24" || r[4].Port != "8080" || r[4].Proto != "tcp" || r[4].Comment != "Homeport: test" {
		t.Fatalf("a rule from one network: %+v", r[4])
	}
	if len(st.Published) != 2 || st.Published[0].Port != "22000/tcp" || st.Published[1].Port != "21027/udp" {
		t.Fatalf("published by apps (not 127.0.0.1, no repeats): %+v", st.Published)
	}
}

func TestOpenAndClose(t *testing.T) {
	f := &fake{added: "ufw allow 80/tcp\nufw allow 22000 comment 'Homeport: Syncthing'\n"}
	m, ctx := manager(f), context.Background()
	if err := m.Open(ctx, "22000", "", "", "Sync'thing"); err != nil {
		t.Fatal(err)
	}
	if last := f.calls[len(f.calls)-1]; last != "ufw allow 22000 comment Homeport: Syncthing" {
		t.Fatalf("open: %s", last)
	}
	if nets := LocalNetworks(); len(nets) > 0 {
		if err := m.Open(ctx, "6000:6010", "udp", nets[0], "game"); err != nil {
			t.Fatal(err)
		}
		if last := f.calls[len(f.calls)-1]; last != "ufw allow from "+nets[0]+" to any port 6000:6010 proto udp comment Homeport: game" {
			t.Fatalf("open from a network: %s", last)
		}
	}
	for _, bad := range [][3]string{{"0", "tcp", ""}, {"70000", "", ""}, {"80;rm", "", ""}, {"6000:6010", "", ""}, {"8080", "icmp", ""}, {"8080", "tcp", "0.0.0.0/0"}} {
		if err := m.Open(ctx, bad[0], bad[1], bad[2], ""); err == nil {
			t.Errorf("open %v should be refused", bad)
		}
	}
	if err := m.Close(ctx, "80", "tcp", ""); err == nil || !strings.Contains(err.Error(), "stay open") {
		t.Errorf("80 must stay open: %v", err)
	}
	if err := m.Close(ctx, "22000", "", ""); err != nil {
		t.Fatal(err)
	}
	if last := f.calls[len(f.calls)-1]; last != "ufw delete allow 22000" {
		t.Fatalf("close: %s", last)
	}
}

func TestTurnOnAllowsTheWayInFirst(t *testing.T) {
	f := &fake{}
	if err := manager(f).TurnOn(context.Background()); err != nil {
		t.Fatal(err)
	}
	i := slices.Index(f.calls, "ufw allow 2222/tcp comment Homeport: SSH")
	j := slices.Index(f.calls, "ufw --force enable")
	if i < 0 || j < 0 || i > j || !slices.Contains(f.calls, "ufw allow 443/tcp comment Homeport: Websites (HTTPS)") {
		t.Fatalf("SSH (on its real port) and the web ports are allowed before the firewall goes on: %v", f.calls)
	}
}

func TestNotOnAMac(t *testing.T) {
	st := (&Manager{Runner: &fake{}, OS: "macos"}).Status(context.Background())
	if st.Available || !strings.Contains(st.Why, "macOS") {
		t.Fatalf("%+v", st)
	}
}
