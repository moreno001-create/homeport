package assist

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"homeport/internal/apps"
	"homeport/internal/state"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeRunner) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	f.mu.Unlock()
	if name == "docker" && strings.Contains(strings.Join(args, " "), " logs ") {
		return "app-1 | started\napp-1 | DB_PASSWORD=Sup3rS3cretValue connecting\napp-1 | Authorization: Bearer abcdefghijklmnopqrstuvwxyz\n", nil
	}
	return "ok", nil
}

func (f *fakeRunner) Stream(ctx context.Context, dir string, w io.Writer, name string, args ...string) error {
	f.mu.Lock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	f.mu.Unlock()
	return nil
}

func (f *fakeRunner) ran(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func newAssistant(t *testing.T) (*Assistant, *fakeRunner) {
	dir := t.TempDir()
	store, _ := state.Open(filepath.Join(dir, "state.json"))
	_ = store.Update(func(d *state.Data) error {
		*d.Settings() = state.Settings{MainDomain: "home.test", ApprovalMinutes: 1}
		d.AddUser(state.User{Name: "alice", Admin: true})
		d.AddApp(state.App{ID: "nextcloud", Name: "Nextcloud", Domain: "cloud.example.test", Port: 18000,
			Access: state.Access{Public: true}, Secrets: map[string]string{"db_password": "Sup3rS3cretValue"}})
		return nil
	})
	r := &fakeRunner{}
	m := &apps.Manager{Store: store, Root: filepath.Join(dir, "apps"), Runner: r}
	return &Assistant{Store: store, Apps: m, Runner: r, Version: "test", Caddyfile: filepath.Join(dir, "Caddyfile"),
		Audit: &Audit{Path: filepath.Join(dir, "audit.log")}, Approvals: &Approvals{},
		Memory: &Memory{Path: filepath.Join(dir, "memory.md")}}, r
}

func TestRedactHidesSecrets(t *testing.T) {
	a, _ := newAssistant(t)
	in := "db Sup3rS3cretValue\nPOSTGRES_PASSWORD: hunter2hunter2\napi_key=abc123def456\nAuthorization: Bearer abcdefghijklmnopqrstuvwxyz\n" +
		"-----BEGIN PRIVATE KEY-----\nMIIEv...\n-----END PRIVATE KEY-----\nharmless line"
	out := Redact(a.Store, in)
	for _, leak := range []string{"Sup3rS3cretValue", "hunter2hunter2", "abc123def456", "abcdefghijklmnopqrstuvwxyz", "MIIEv"} {
		if strings.Contains(out, leak) {
			t.Errorf("%q leaked:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "harmless line") || !strings.Contains(out, "POSTGRES_PASSWORD") {
		t.Errorf("redaction removed too much:\n%s", out)
	}
}

func TestLookToolsRunAndHideSecrets(t *testing.T) {
	a, r := newAssistant(t)
	res := a.Call(context.Background(), "test", "app_logs", Args{"id": "nextcloud", "lines": float64(50)})
	if res.IsError || !strings.Contains(res.Text, "started") || strings.Contains(res.Text, "Sup3rS3cretValue") || strings.Contains(res.Text, "abcdefghijklmnop") {
		t.Fatalf("app_logs: %+v", res)
	}
	if !r.ran("logs --no-color --timestamps --tail 50") {
		t.Fatal("docker compose logs wasn't asked for 50 lines")
	}
	if res := a.Call(context.Background(), "test", "app_logs", Args{}); !res.IsError {
		t.Fatal("a missing argument must be an error")
	}
}

func TestChangesWaitForApproval(t *testing.T) {
	a, r := newAssistant(t)
	call := func() chan Result {
		ch := make(chan Result, 1)
		go func() { ch <- a.Call(context.Background(), "mcp:opencode", "restart_app", Args{"id": "nextcloud"}) }()
		return ch
	}
	waitPending := func() Approval {
		for i := 0; i < 200; i++ {
			if p := a.Approvals.Pending(); len(p) == 1 {
				return p[0]
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("no approval request appeared")
		return Approval{}
	}

	// Declined: nothing runs.
	ch := call()
	p := waitPending()
	if !strings.Contains(p.Summary, "Restart nextcloud") || p.Client != "mcp:opencode" {
		t.Fatalf("approval request: %+v", p)
	}
	if r.ran("restart") {
		t.Fatal("ran before approval")
	}
	_ = a.Approvals.Decide(p.ID, false, "alice")
	if res := <-ch; !res.IsError || r.ran("restart") {
		t.Fatalf("declined: %+v", res)
	}

	// Approved: it runs.
	ch = call()
	p = waitPending()
	_ = a.Approvals.Decide(p.ID, true, "alice")
	if res := <-ch; res.IsError || !r.ran("compose -p hp-nextcloud -f compose.yaml restart") {
		t.Fatalf("approved: %+v", res)
	}
	if err := a.Approvals.Decide(p.ID, true, "alice"); err == nil {
		t.Fatal("deciding twice must fail")
	}

	// Nobody answers: it times out as a no.
	ok, _, timedOut := a.Approvals.Request(nil, Approval{Tool: "x"}, 20*time.Millisecond)
	if ok || !timedOut {
		t.Fatal("an unanswered request must time out as declined")
	}

	entries, _ := a.Audit.Recent(10)
	var decisions []string
	for _, e := range entries {
		decisions = append(decisions, e.Decision)
	}
	if strings.Join(decisions, ",") != "approved,denied" || entries[0].By != "alice" {
		t.Fatalf("audit log: %v", decisions)
	}
}

func TestShellIsOffUntilTurnedOn(t *testing.T) {
	a, r := newAssistant(t)
	for _, tl := range a.Tools() {
		if tl.Level == Shell {
			t.Fatal("shell tools must not be listed while they're off")
		}
	}
	if res := a.Call(context.Background(), "test", "run_shell", Args{"command": "id"}); !res.IsError || r.ran("bash") {
		t.Fatalf("run_shell while off: %+v", res)
	}
	_ = a.Store.Update(func(d *state.Data) error { d.Settings().AssistantShell = true; return nil })
	found := false
	for _, tl := range a.Tools() {
		found = found || tl.Name == "run_shell"
	}
	if !found {
		t.Fatal("run_shell should be listed once turned on")
	}
}

func TestMCP(t *testing.T) {
	a, _ := newAssistant(t)
	ctx := context.Background()
	var init struct {
		Result struct {
			ProtocolVersion string
			Instructions    string
			ServerInfo      struct{ Name string }
		}
	}
	_ = json.Unmarshal(a.HandleMCP(ctx, "t", []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"opencode"}}}`)), &init)
	if init.Result.ProtocolVersion != "2025-03-26" || init.Result.ServerInfo.Name != "homeport" || !strings.Contains(init.Result.Instructions, "read_memory") {
		t.Fatalf("initialize: %+v", init)
	}
	if a.HandleMCP(ctx, "t", []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)) != nil {
		t.Fatal("notifications get no reply")
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name        string
				Annotations struct{ ReadOnlyHint bool }
				InputSchema struct{ Required []string }
			}
		}
	}
	_ = json.Unmarshal(a.HandleMCP(ctx, "t", []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)), &list)
	names := map[string]bool{}
	for _, tl := range list.Result.Tools {
		names[tl.Name] = true
		if tl.Name == "install_app" && (tl.Annotations.ReadOnlyHint || len(tl.InputSchema.Required) != 2) {
			t.Errorf("install_app schema: %+v", tl)
		}
	}
	for _, want := range []string{"read_memory", "remember", "set_handoff", "list_apps", "app_logs", "install_app"} {
		if !names[want] {
			t.Errorf("tools/list misses %s", want)
		}
	}
	var callRes struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool
		}
	}
	_ = json.Unmarshal(a.HandleMCP(ctx, "t", []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_apps","arguments":{}}}`)), &callRes)
	if callRes.Result.IsError || !strings.Contains(callRes.Result.Content[0].Text, "nextcloud") {
		t.Fatalf("tools/call: %+v", callRes)
	}
	if !strings.Contains(string(a.HandleMCP(ctx, "t", []byte(`{"jsonrpc":"2.0","id":4,"method":"nope"}`))), "-32601") {
		t.Fatal("unknown methods are -32601")
	}
}

func TestMemoryCarriesWorkBetweenAgents(t *testing.T) {
	a, _ := newAssistant(t)
	ctx := context.Background()
	// The first agent notes what it did, and leaves work half done.
	a.Call(ctx, "mcp:opencode", "remember", Args{"topic": "app: nextcloud", "note": "Raised PHP memory to 1G; uploads failed. DB password is Sup3rS3cretValue"})
	a.Call(ctx, "mcp:opencode", "set_handoff", Args{"text": "Fixing slow Nextcloud: memory raised; next, check the cron container logs."})
	// Homeport records the changes itself, whatever the agent writes.
	_ = a.Audit.Write(AuditEntry{Time: time.Now(), Client: "mcp:opencode", Tool: "restart_app", Level: Change, Args: json.RawMessage(`{"id":"nextcloud"}`), Decision: "approved", By: "alice", OK: true})

	// The next agent picks it up.
	res := a.Call(ctx, "assistant (claude)", "read_memory", Args{})
	mem := res.Text
	for _, want := range []string{"## In progress", "Left by mcp:opencode", "check the cron container logs", "## app: nextcloud", "Raised PHP memory", "restart_app", "approved by alice"} {
		if !strings.Contains(mem, want) {
			t.Errorf("memory misses %q:\n%s", want, mem)
		}
	}
	if strings.Contains(mem, "Sup3rS3cretValue") {
		t.Fatal("a secret was written into the memory")
	}
	if strings.Index(mem, "## In progress") > strings.Index(mem, "## app: nextcloud") {
		t.Error("work in progress must come first")
	}
	// Done: clear the hand-off, condense a topic.
	a.Call(ctx, "assistant (claude)", "set_handoff", Args{"text": ""})
	a.Call(ctx, "assistant (claude)", "rewrite_memory_topic", Args{"topic": "app: nextcloud", "content": "- PHP memory is 1G (uploads needed it)."})
	mem, _ = a.Memory.Read()
	if !strings.Contains(mem, "## In progress\nNothing.") || strings.Contains(mem, "Raised PHP memory") || !strings.Contains(mem, "PHP memory is 1G") {
		t.Fatalf("after clearing and condensing:\n%s", mem)
	}
}
