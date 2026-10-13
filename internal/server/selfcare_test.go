package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChecksAndRepairs(t *testing.T) {
	r := newRig(t)
	r.install("mealie", "mealie.home.test", nil)
	applied := 0
	r.srv.ApplyProxy = func(context.Context) error { applied++; return nil }
	// Caddy answers the main address, but with an error.
	broken := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer broken.Close()
	r.srv.HTTPSAddr = strings.TrimPrefix(broken.URL, "https://")

	w := r.local(t, "POST", "/api/checks", "")
	var got struct {
		Run   CheckRun `json:"run"`
		Daily bool     `json:"daily"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(w.Body.String())
	}
	if !got.Daily || got.Run.Problems < 1 {
		t.Fatalf("got %+v", got)
	}
	first := got.Run.Checks[0]
	if first.State != "bad" || !strings.Contains(first.Title, "main address") || first.Fix != "proxy" || first.Guide != "caddy-config" {
		t.Errorf("problems should come first, with their repair and guide entry: %+v", first)
	}
	found := false
	for _, c := range got.Run.Checks {
		found = found || (c.Title == "Mealie" && c.State == "ok")
	}
	if !found {
		t.Errorf("the app's check is missing: %+v", got.Run.Checks)
	}
	// The result is kept (for the Overview page and after a restart).
	if _, err := os.Stat(filepath.Join(r.srv.stateDir(), "last-checks.json")); err != nil {
		t.Error(err)
	}
	if w := r.local(t, "GET", "/api/checks", ""); !strings.Contains(w.Body.String(), "main address") {
		t.Errorf("last checks: %s", w.Body.String())
	}

	// Repairs: the one offered works, unknown ones and the router (not allowed) don't.
	if w := r.local(t, "POST", "/api/checks/fix", `{"fix":"proxy"}`); w.Code != http.StatusNoContent || applied != 1 {
		t.Errorf("proxy repair: %d %s (applied %d)", w.Code, w.Body.String(), applied)
	}
	if w := r.local(t, "POST", "/api/checks/fix", `{"fix":"rm -rf"}`); w.Code == http.StatusNoContent {
		t.Error("an unknown repair ran")
	}
	if w := r.local(t, "POST", "/api/checks/fix", `{"fix":"router"}`); !strings.Contains(w.Body.String(), "isn't allowed") {
		t.Errorf("router repair without permission: %s", w.Body.String())
	}

	// The daily check can be turned off.
	r.local(t, "PUT", "/api/settings", `{"dailyCheck":false}`)
	if w := r.local(t, "GET", "/api/checks", ""); !strings.Contains(w.Body.String(), `"daily":false`) {
		t.Errorf("daily check still on: %s", w.Body.String())
	}
}

func TestSetupPage(t *testing.T) {
	r := newRig(t)
	r.install("mealie", "mealie.home.test", nil)
	text := r.srv.SetupText(context.Background())
	for _, want := range []string{"# How this server is set up", "Main address: home.test", "Mealie (mealie): https://mealie.home.test/", "## What the installer did"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	saved, err := os.ReadFile(filepath.Join(r.srv.stateDir(), "server-setup.md"))
	if err != nil || string(saved) != text {
		t.Errorf("server-setup.md not saved (%v)", err)
	}
	if w := r.local(t, "GET", "/api/guide?search=no+space+left", ""); !strings.Contains(w.Body.String(), `"id":"disk-full"`) {
		t.Errorf("guide search: %s", w.Body.String())
	}
}

func TestAssistantTools(t *testing.T) {
	r := newRig(t)
	tools := map[string]bool{}
	for _, tl := range r.srv.AssistantTools() {
		tools[tl.Name] = true
		if tl.Level != "look" && tl.Summary == nil {
			t.Errorf("%s changes things but has no summary for the approval", tl.Name)
		}
	}
	for _, name := range []string{"server_setup", "run_checks", "internet_status", "firewall_status", "backup_status",
		"services_status", "ai_models_status", "restart_service", "rebuild_proxy_config", "reopen_router_ports"} {
		if !tools[name] {
			t.Errorf("no %s tool", name)
		}
	}
	// Look tools answer exactly what the pages show.
	out, err := r.srv.look(context.Background(), r.srv.firewallStatus, "/api/firewall")
	if err != nil || !strings.Contains(out, `"available":false`) {
		t.Errorf("firewall_status: %q %v", out, err)
	}
	for _, tl := range r.srv.AssistantTools() {
		if tl.Name == "restart_service" {
			if _, err := tl.Run(context.Background(), map[string]any{"service": "sshd"}); err == nil {
				t.Error("restart_service restarted something it doesn't look after")
			}
		}
	}
}
