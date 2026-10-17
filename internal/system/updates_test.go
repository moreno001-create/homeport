package system

import (
	"context"
	"io"
	"strings"
	"testing"
)

type aptFake struct{ calls []string }

func (f *aptFake) Output(_ context.Context, _, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if name == "apt" {
		return `WARNING: apt does not have a stable CLI interface. Use with caution in scripts.

Listing...
docker-ce/noble 5:28.5.1-1~ubuntu.24.04~noble amd64 [upgradable from: 5:28.5.0-1~ubuntu.24.04~noble]
openssl/noble-updates,noble-security 3.0.13-0ubuntu3.6 amd64 [upgradable from: 3.0.13-0ubuntu3.5]
vim/noble-updates 2:9.1.0016-1ubuntu7.9 amd64 [upgradable from: 2:9.1.0016-1ubuntu7.8]
`, nil
	}
	return "", nil
}
func (f *aptFake) Stream(context.Context, string, io.Writer, string, ...string) error { return nil }

func TestUpdates(t *testing.T) {
	f := &aptFake{}
	m := &Manager{Runner: f, OS: "linux"}
	u, err := m.Updates(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls[0] != "apt-get update -q" {
		t.Fatalf("refresh should ask the package servers first: %v", f.calls)
	}
	if len(u.Packages) != 3 || u.Security != 1 || !u.Packages[1].Security || u.Packages[1].From != "3.0.13-0ubuntu3.5" || u.Packages[0].To != "5:28.5.1-1~ubuntu.24.04~noble" {
		t.Fatalf("got %+v", u)
	}
	if len(u.Restarts) != 1 || u.Restarts[0] != "docker" {
		t.Fatalf("a Docker update restarts the apps: %v", u.Restarts)
	}
	mac := &Manager{Runner: f, OS: "darwin"}
	if u, _ := mac.Updates(context.Background(), false); u.Supported || u.Why == "" {
		t.Fatalf("a Mac says where its updates are: %+v", u)
	}
}
