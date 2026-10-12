package apps

import (
	"context"
	"io"
	"strings"
	"testing"
)

// psRunner answers docker compose ps, docker stats and docker inspect like Docker would.
type psRunner struct{ calls []string }

func (p *psRunner) Output(_ context.Context, _, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	p.calls = append(p.calls, cmd)
	switch {
	case strings.Contains(cmd, " ps --all"):
		return `{"Service":"db","Name":"hp-x-db-1","Image":"postgres:17-alpine","State":"running","Status":"Up 2 hours","ExitCode":0}
{"Service":"app","Name":"hp-x-app-1","Image":"example/x:1","State":"restarting","Status":"Restarting (1) 3 seconds ago","ExitCode":1}
{"Service":"init","Name":"hp-x-init-1","Image":"busybox:1.37","State":"exited","Status":"Exited (0) 2 hours ago","ExitCode":0}`, nil
	case strings.HasPrefix(cmd, "docker stats"):
		return `{"Name":"hp-x-db-1","CPUPerc":"0.52%","MemUsage":"41MiB / 11GiB"}`, nil
	case strings.HasPrefix(cmd, "docker inspect"):
		return "/hp-x-db-1 0\n/hp-x-app-1 7\n/hp-x-init-1 0\n", nil
	}
	return "", nil
}
func (p *psRunner) Stream(context.Context, string, io.Writer, string, ...string) error { return nil }

func TestContainers(t *testing.T) {
	r := &psRunner{}
	m := &Manager{Root: t.TempDir(), Runner: r}
	ctx := context.Background()
	list, err := m.Containers(ctx, "x")
	if err != nil || len(list) != 3 {
		t.Fatalf("%v %+v", err, list)
	}
	if list[0].Service != "app" || list[1].Service != "db" || !list[2].Setup || list[0].Setup {
		t.Fatalf("sorted by service, with the finished setup container marked: %+v", list)
	}
	use := m.Usage(ctx, []string{"hp-x-db-1", "hp-x-app-1", "hp-x-init-1"})
	if use["hp-x-db-1"].CPU != "0.52%" || use["hp-x-app-1"].Restarts != 7 {
		t.Fatalf("usage: %+v", use)
	}
	if err := m.ContainerAction(ctx, "x", "app", "restart"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.calls[len(r.calls)-1], "compose -p hp-x -f compose.yaml restart app") {
		t.Fatalf("last call: %s", r.calls[len(r.calls)-1])
	}
	if err := m.ContainerAction(ctx, "x", "other", "stop"); err == nil {
		t.Error("a service the app doesn't have must be refused")
	}
	if err := m.ContainerAction(ctx, "x", "app", "rm"); err == nil {
		t.Error("only restart, stop and start")
	}
}
