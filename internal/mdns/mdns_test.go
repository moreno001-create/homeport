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
