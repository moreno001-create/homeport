package mdns

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLocal(t *testing.T) {
	got := Local([]string{"homeport.local", "Cloud.Homeport.local", "example.com", "homeport.local", ""})
	if !slices.Equal(got, []string{"cloud.homeport.local", "homeport.local"}) {
		t.Fatal(got)
	}
}

// fakePublish is an avahi-publish that notes its arguments, then stays running (or ends at once with "exit").
func fakePublish(t *testing.T, then string) (cmd, calls string) {
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	cmd = filepath.Join(dir, "avahi-publish")
	if err := os.WriteFile(cmd, []byte("#!/bin/sh\necho \"$@\" >> "+calls+"\n"+then+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return cmd, calls
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 200 {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for " + what)
}

func TestPublishStartsAndStopsNames(t *testing.T) {
	cmd, calls := fakePublish(t, "exec sleep 30")
	p := &Publisher{Command: cmd, Hostname: "mor1"}
	defer p.Close()
	ctx := context.Background()
	names := []string{"homeport.local", "cloud.homeport.local", "mor1.local", "example.com"}
	if err := p.Publish(ctx, names, []string{"192.168.0.141"}); err != nil {
		t.Fatal(err)
	}
	if got := p.Announced(); !slices.Equal(got, []string{"cloud.homeport.local 192.168.0.141", "homeport.local 192.168.0.141"}) {
		t.Fatalf("announced %v (the hostname is Avahi's own, and example.com isn't .local)", got)
	}
	waitFor(t, "both names", func() bool { b, _ := os.ReadFile(calls); return strings.Count(string(b), "\n") == 2 })
	b, _ := os.ReadFile(calls)
	if !strings.Contains(string(b), "--address --no-reverse homeport.local 192.168.0.141") {
		t.Fatalf("calls:\n%s", b)
	}
	// Publishing the same again starts nothing new; a new address replaces the names.
	_ = p.Publish(ctx, names, []string{"192.168.0.141"})
	_ = p.Publish(ctx, []string{"homeport.local"}, []string{"192.168.0.150"})
	if got := p.Announced(); !slices.Equal(got, []string{"homeport.local 192.168.0.150"}) {
		t.Fatalf("after the address changed: %v", got)
	}
	waitFor(t, "the new address", func() bool { b, _ := os.ReadFile(calls); return strings.Count(string(b), "\n") == 3 })
	_ = p.Publish(ctx, []string{"example.com"}, []string{"192.168.0.150"})
	if len(p.Announced()) != 0 {
		t.Fatal("no .local names left: nothing should be announced")
	}
}

func TestPublishStartsAgainWhenItEnds(t *testing.T) {
	cmd, calls := fakePublish(t, "exit 1")
	p := &Publisher{Command: cmd, Retry: 20 * time.Millisecond}
	defer p.Close()
	_ = p.Publish(context.Background(), []string{"homeport.local"}, []string{"10.0.0.2"})
	waitFor(t, "a second start", func() bool { b, _ := os.ReadFile(calls); return strings.Count(string(b), "\n") >= 2 })
}

func TestPublishWithoutAvahi(t *testing.T) {
	p := &Publisher{Command: filepath.Join(t.TempDir(), "missing")}
	if err := p.Publish(context.Background(), []string{"homeport.local"}, []string{"10.0.0.2"}); err != nil || len(p.Announced()) != 0 {
		t.Fatalf("without Avahi: %v, %v", err, p.Announced())
	}
}

func TestPublishOnAMac(t *testing.T) {
	cmd, calls := fakePublish(t, "exec sleep 30")
	p := &Publisher{OS: "macos", Command: cmd, Hostname: "homeport"}
	defer p.Close()
	_ = p.Publish(context.Background(), []string{"homeport.local", "cloud.homeport.local"}, []string{"192.168.0.139"})
	waitFor(t, "dns-sd", func() bool { b, _ := os.ReadFile(calls); return strings.Count(string(b), "\n") == 1 })
	b, _ := os.ReadFile(calls)
	if strings.TrimSpace(string(b)) != "-P Homeport cloud.homeport _http._tcp local 443 cloud.homeport.local 192.168.0.139" {
		t.Fatalf("the Mac announces its own name itself; the app's goes through dns-sd -P: %s", b)
	}
}

func TestHostsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	orig := "127.0.0.1\tlocalhost\n127.0.1.1\tmemorydelta\n\n# The following lines are desirable for IPv6 capable hosts\n::1 ip6-localhost\n"
	_ = os.WriteFile(path, []byte(orig), 0o644)
	if err := writeHosts(path, []string{"ai.homeport.local", "homeport.local", "librespeed.homeport.local"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := orig + "\n" + hostsStart + "\n127.0.0.1\tai.homeport.local homeport.local librespeed.homeport.local\n" + hostsEnd + "\n"
	if string(b) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b, want)
	}
	// Names change: the block is replaced, the rest kept; no names: it's gone.
	_ = writeHosts(path, []string{"homeport.local"})
	b, _ = os.ReadFile(path)
	if strings.Count(string(b), hostsStart) != 1 || strings.Contains(string(b), "librespeed") || !strings.HasPrefix(string(b), orig) {
		t.Errorf("got:\n%s", b)
	}
	_ = writeHosts(path, nil)
	if b, _ = os.ReadFile(path); string(b) != orig {
		t.Errorf("got:\n%q\nwant:\n%q", b, orig)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
}
