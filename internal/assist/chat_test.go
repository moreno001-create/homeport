package assist

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"homeport/internal/state"
)

func TestHard(t *testing.T) {
	for _, s := range []string{"Why can't I open Immich?", "nextcloud is broken", "set up backups", "the certificate is wrong"} {
		if !Hard(s) {
			t.Errorf("%q should go to Claude", s)
		}
	}
	for _, s := range []string{"What's installed?", "list the users", "hi"} {
		if Hard(s) {
			t.Errorf("%q should stay local", s)
		}
	}
}

func TestLocalBase(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.20:1234":           "http://192.168.1.20:1234/v1",
		"http://mac.lan:11434/":       "http://mac.lan:11434/v1",
		"https://llm.example.test/v1": "https://llm.example.test/v1",
		"":                            "",
	} {
		if got, err := LocalBase(in); err != nil || got != want {
			t.Errorf("LocalBase(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := LocalBase("ftp://x"); err == nil {
		t.Error("ftp should be refused")
	}
}

// A conversation the local model started reads correctly for Claude: tool calls become tool_use blocks with safe IDs,
// results go in the next user message, and a user message after results joins it.
func TestClaudeHistory(t *testing.T) {
	turns := []Turn{
		{Role: "user", Text: "what's installed?"},
		{Role: "note", Text: "not sent"},
		{Role: "assistant", Text: "Let me look.", Calls: []ToolCall{{ID: "call:1", Name: "list_apps", Args: Args{}}}},
		{Role: "tools", Results: []ToolResult{{ID: "call:1", Name: "list_apps", Text: "nextcloud"}}},
		{Role: "user", Text: "and why is it slow?"},
	}
	msgs := claudeHistory(turns)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	b, _ := json.Marshal(msgs)
	s := string(b)
	for _, want := range []string{`"tool_use"`, `"id":"call_1"`, `"tool_use_id":"call_1"`, `and why is it slow?`} {
		if !strings.Contains(s, want) {
			t.Errorf("history is missing %s: %s", want, s)
		}
	}
	if strings.Contains(s, "not sent") {
		t.Error("notes must not be sent")
	}
	if msgs[2].Role != anthropic.BetaMessageParamRoleUser || len(msgs[2].Content) != 2 {
		t.Errorf("tool results and the next question should share one user message: %s", s)
	}
}

func newChat(t *testing.T, local string) *Chat {
	a, _ := newAssistant(t)
	t.Setenv("ANTHROPIC_API_KEY", "")
	dir := t.TempDir()
	_ = a.Store.Update(func(d *state.Data) error {
		d.Settings().Chat = state.ChatSettings{LocalURL: local, LocalModel: "small"}
		return nil
	})
	return &Chat{A: a, KeyFile: filepath.Join(dir, "key"), UsageFile: filepath.Join(dir, "usage.json")}
}

func waitIdle(t *testing.T, c *Chat, id string) Conversation {
	t.Helper()
	for i := 0; i < 200; i++ {
		conv, err := c.Get("alice", id)
		if err != nil {
			t.Fatal(err)
		}
		if !conv.Busy {
			return conv
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the chat never finished")
	return Conversation{}
}

// fakeLocal answers like LM Studio: first a tool call, then (after seeing the result) a plain answer.
func fakeLocal(t *testing.T, handOver bool) (*httptest.Server, *[]map[string]any) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		n := len(bodies)
		mu.Unlock()
		msg := map[string]any{"role": "assistant", "content": "<think>hmm</think>Nextcloud is installed."}
		switch {
		case handOver:
			msg = map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": handOverTool, "arguments": `{"reason":"needs diagnosis"}`}}}}
		case n == 1:
			msg = map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "c1", "type": "function",
				"function": map[string]any{"name": "list_apps", "arguments": "{}"}}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg, "finish_reason": "stop"}}})
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func TestLocalChatUsesTools(t *testing.T) {
	srv, bodies := fakeLocal(t, false)
	c := newChat(t, srv.URL+"/v1")
	conv, err := c.Send("alice", "", "What's installed?", "", false)
	if err != nil {
		t.Fatal(err)
	}
	conv = waitIdle(t, c, conv.ID)
	var roles []string
	for _, tu := range conv.Turns {
		roles = append(roles, tu.Role)
	}
	if got := strings.Join(roles, ","); got != "user,assistant,tools,assistant" {
		t.Fatalf("turns = %s (%+v)", got, conv.Turns)
	}
	if !strings.Contains(conv.Turns[2].Results[0].Text, "Nextcloud") {
		t.Errorf("list_apps result: %q", conv.Turns[2].Results[0].Text)
	}
	if conv.Turns[3].Text != "Nextcloud is installed." {
		t.Errorf("answer = %q (thinking should be stripped)", conv.Turns[3].Text)
	}
	if len(*bodies) != 2 {
		t.Fatalf("local model called %d times", len(*bodies))
	}
	// With no Claude key, the local model isn't offered a hand-over tool.
	b, _ := json.Marshal((*bodies)[0]["tools"])
	if strings.Contains(string(b), handOverTool) {
		t.Error("ask_claude offered without a Claude key")
	}
	if _, err := c.Get("bob", conv.ID); err == nil {
		t.Error("another admin could read the conversation")
	}
}

// When the local model hands over and Claude needs consent, the chat stops and asks.
func TestHandOverAsksBeforeCloud(t *testing.T) {
	srv, _ := fakeLocal(t, true)
	c := newChat(t, srv.URL)
	_ = c.A.Store.Update(func(d *state.Data) error { d.Settings().Chat.AskBeforeCloud = true; return nil })
	if err := c.SetAPIKey("sk-ant-test-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if c.KeyHint() != "…cdef" {
		t.Errorf("hint = %q", c.KeyHint())
	}
	conv, _ := c.Send("alice", "", "list the users", "", false)
	conv = waitIdle(t, c, conv.ID)
	if !conv.Consent || !strings.Contains(conv.ConsentWhy, "needs diagnosis") {
		t.Fatalf("expected a consent question, got %+v", conv)
	}
	last := conv.Turns[len(conv.Turns)-1]
	if last.Role != "note" || !strings.Contains(last.Text, "Handed to Claude") {
		t.Errorf("last turn = %+v", last)
	}
}

func TestSpendingCap(t *testing.T) {
	c := newChat(t, "")
	c.addSpend("claude-opus-5-5", anthropic.BetaUsage{InputTokens: 1_000_000, OutputTokens: 100_000})
	if u := c.Spent(); u.USD < 5.99 || u.USD > 6.01 {
		t.Fatalf("spent = %v, want $6", u.USD)
	}
	if err := c.underCap(state.ChatSettings{MonthlyCapUSD: 5}); err == nil {
		t.Error("the cap should stop Claude")
	}
	if err := c.underCap(state.ChatSettings{MonthlyCapUSD: 10}); err != nil {
		t.Error(err)
	}
}

func TestRedactsAnthropicKeys(t *testing.T) {
	a, _ := newAssistant(t)
	if got := Redact(a.Store, "key is sk-ant-api03-abcdefghijklmnop end"); strings.Contains(got, "sk-ant") {
		t.Errorf("key shown: %s", got)
	}
}

// TestLiveLocal talks to a real local model: HOMEPORT_LIVE_LOCAL=http://127.0.0.1:1234 HOMEPORT_LIVE_MODEL=qwen/qwen3.5-9b
func TestLiveLocal(t *testing.T) {
	url := os.Getenv("HOMEPORT_LIVE_LOCAL")
	if url == "" {
		t.Skip("set HOMEPORT_LIVE_LOCAL to try a real local model")
	}
	c := newChat(t, url)
	_ = c.A.Store.Update(func(d *state.Data) error { d.Settings().Chat.LocalModel = os.Getenv("HOMEPORT_LIVE_MODEL"); return nil })
	conv, err := c.Send("alice", "", "Which apps are installed, and on which domains?", "local", false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if conv, _ = c.Get("alice", conv.ID); !conv.Busy {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	for _, tu := range conv.Turns {
		t.Logf("%s %s: %s %v %v", tu.Role, tu.By, tu.Text, tu.Calls, len(tu.Results))
	}
	if last := conv.Turns[len(conv.Turns)-1]; last.Role != "assistant" || !strings.Contains(last.Text, "cloud.example.test") {
		t.Errorf("expected an answer naming cloud.example.test")
	}
}

// fakeClaude streams like the Messages API: a tool call first, then an answer.
func fakeClaude(t *testing.T) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.Header.Get("anthropic-beta")+" "+string(b))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		ev := func(name, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data) }
		ev("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":1000,"output_tokens":1}}}`)
		if n == 1 {
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Let me check."}}`)
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
			ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"app_details","input":{}}}`)
			ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"id\":\"nextcloud\"}"}}`)
			ev("content_block_stop", `{"type":"content_block_stop","index":1}`)
			ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`)
		} else {
			ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Nextcloud is stopped."}}`)
			ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
			ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":10}}`)
		}
		ev("message_stop", `{"type":"message_stop"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func TestClaudeChat(t *testing.T) {
	srv, bodies := fakeClaude(t)
	c := newChat(t, "")
	c.ClaudeURL = srv.URL
	if err := c.SetAPIKey("sk-ant-test-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	conv, err := c.Send("alice", "", "Why is Nextcloud down?", "", false)
	if err != nil {
		t.Fatal(err)
	}
	conv = waitIdle(t, c, conv.ID)
	var roles []string
	for _, tu := range conv.Turns {
		roles = append(roles, tu.Role)
	}
	if got := strings.Join(roles, ","); got != "user,assistant,tools,assistant" {
		t.Fatalf("turns = %s (%+v)", got, conv.Turns)
	}
	if call := conv.Turns[1].Calls[0]; call.Name != "app_details" || call.Args.Str("id") != "nextcloud" {
		t.Errorf("call = %+v", call)
	}
	if conv.Turns[3].Text != "Nextcloud is stopped." || conv.Turns[3].By != "claude-opus-5-5" {
		t.Errorf("answer = %+v", conv.Turns[3])
	}
	if len(*bodies) != 2 {
		t.Fatalf("Claude called %d times", len(*bodies))
	}
	first, second := (*bodies)[0], (*bodies)[1]
	for _, want := range []string{"server-side-fallback-2026-07-01", `"fallbacks":"default"`, `"effort":"medium"`, `"model":"claude-opus-5-5"`, `"cache_control"`, `"stream":true`, `"name":"app_details"`} {
		if !strings.Contains(first, want) {
			t.Errorf("request is missing %s", want)
		}
	}
	if strings.Contains(first, `"thinking"`) {
		t.Error("thinking can't be set on Opus 5.5")
	}
	// The second request repeats Claude's first reply exactly and adds the tool result.
	if !strings.Contains(second, `"tool_use_id":"toolu_1"`) || !strings.Contains(second, `"Let me check."`) {
		t.Errorf("second request: %s", second)
	}
	if u := c.Spent(); u.USD <= 0 || u.Input != 2000 {
		t.Errorf("spending not counted: %+v", u)
	}
}
