package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"homeport/internal/assist"
	"homeport/internal/state"
)

// fakeLocalModel is an OpenAI-compatible server with two models; it notes whether each request offered tools.
type fakeLocalModel struct {
	mu        sync.Mutex
	withTools []bool
	models    []string
}

func (f *fakeLocalModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		_, _ = io.WriteString(w, `{"data":[{"id":"small:2b"},{"id":"big:9b"}]}`)
	case "/v1/chat/completions":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		_, tools := body["tools"]
		f.withTools = append(f.withTools, tools)
		f.models = append(f.models, body["model"].(string))
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Hello from the model."},"finish_reason":"stop"}]}`)
	default:
		http.NotFound(w, r)
	}
}

func aiRig(t *testing.T) (*rig, *fakeLocalModel) {
	r := newRig(t)
	fake := &fakeLocalModel{}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	_ = r.srv.Store.Update(func(d *state.Data) error {
		d.Settings().Chat = state.ChatSettings{LocalURL: ts.URL + "/v1", LocalModel: "small:2b"}
		return nil
	})
	r.srv.Chat = &assist.Chat{A: &assist.Assistant{Store: r.srv.Store, Approvals: &assist.Approvals{}}}
	r.srv.Assist = r.srv.Chat.A
	return r, fake
}

// signInOnAI signs a browser in on the portal and follows the hand-off to the AI page, as a browser would.
func (b *browser) signInOnAI(t *testing.T, name, password string) {
	t.Helper()
	w := b.do("GET", "https://ai.home.test/", nil, nil)
	want := "https://home.test/sso/continue?return=" + url.QueryEscape("https://ai.home.test/")
	if w.Code != http.StatusFound || w.Header().Get("Location") != want {
		t.Fatalf("signed out, the AI page should send the browser to sign in: %d %s", w.Code, w.Header().Get("Location"))
	}
	b.do("POST", "https://home.test/login", url.Values{"name": {name}, "password": {password}}, nil)
	w = b.do("GET", want, nil, nil)
	cb := w.Header().Get("Location")
	if !strings.HasPrefix(cb, "https://ai.home.test/.homeport/callback?code=") {
		t.Fatalf("want the hand-off to the AI page, got %d %s", w.Code, cb)
	}
	b.do("GET", cb, nil, nil)
	if w := b.do("GET", "https://ai.home.test/", nil, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ai.js") {
		t.Fatalf("signed in, the AI page should open: %d", w.Code)
	}
}

func (b *browser) json(t *testing.T, method, path string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		j, _ := json.Marshal(body)
		rd = strings.NewReader(string(j))
	}
	req := httptest.NewRequest(method, path, rd)
	req.Host = "ai.home.test"
	req.Header.Set("X-Homeport", "1")
	req.Header.Set("Content-Type", "application/json")
	if c, ok := b.cookies["ai.home.test"]; ok {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: c})
	}
	w := httptest.NewRecorder()
	b.r.h.ServeHTTP(w, req)
	if out != nil {
		_ = json.Unmarshal(w.Body.Bytes(), out)
	}
	return w.Code
}

func waitAnswered(t *testing.T, b *browser, id string) assist.Conversation {
	t.Helper()
	var conv assist.Conversation
	for range 200 {
		b.json(t, "GET", "/api/ai/chat/"+id, nil, &conv)
		if !conv.Busy {
			return conv
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no answer")
	return conv
}

func TestAIPageForSomeoneWhoIsntAnAdmin(t *testing.T) {
	r, fake := aiRig(t)
	bob := r.browser()
	bob.signInOnAI(t, "bob", "battery staple")

	var info struct {
		Configured bool
		Default    string
		Choices    []struct{ ID, Name string }
		Claude     bool
		Name       string
	}
	if bob.json(t, "GET", "/api/ai/model", nil, &info); !info.Configured || info.Default != "small:2b" || len(info.Choices) != 2 || info.Claude || info.Name != "Small 2B" {
		t.Fatalf("model info for bob: %+v", info)
	}
	// A plain chat: the chosen local model, and no tools offered.
	var conv assist.Conversation
	if code := bob.json(t, "POST", "/api/ai/chat", map[string]string{"text": "Hi!", "model": "big:9b"}, &conv); code != http.StatusOK {
		t.Fatalf("send: %d", code)
	}
	conv = waitAnswered(t, bob, conv.ID)
	if !conv.Plain || len(conv.Turns) != 2 || conv.Turns[1].Text != "Hello from the model." {
		t.Fatalf("conversation: %+v", conv)
	}
	if len(fake.withTools) != 1 || fake.withTools[0] || fake.models[0] != "big:9b" {
		t.Fatalf("the model should get no tools and be the one chosen: tools %v, models %v", fake.withTools, fake.models)
	}
	// Not for bob: Claude, a model the server doesn't have, approvals, or the admin's conversations.
	if code := bob.json(t, "POST", "/api/ai/chat", map[string]string{"text": "Hi", "model": "claude"}, nil); code != http.StatusBadRequest {
		t.Errorf("Claude for a non-admin: %d", code)
	}
	if code := bob.json(t, "POST", "/api/ai/chat", map[string]string{"text": "Hi", "model": "other:1b"}, nil); code != http.StatusBadRequest {
		t.Errorf("an unknown model: %d", code)
	}
	if code := bob.json(t, "GET", "/api/ai/approvals", nil, nil); code != http.StatusForbidden {
		t.Errorf("approvals for a non-admin: %d", code)
	}
	// The admin API stays on the main domain only.
	if code := bob.json(t, "GET", "/api/users", nil, nil); code != http.StatusNotFound {
		t.Errorf("the admin API on the AI page's domain: %d", code)
	}
}

func TestAIPageForAnAdmin(t *testing.T) {
	r, fake := aiRig(t)
	alice := r.browser()
	alice.signInOnAI(t, "alice", "correct horse")
	var conv assist.Conversation
	alice.json(t, "POST", "/api/ai/chat", map[string]string{"text": "What's installed?"}, &conv)
	conv = waitAnswered(t, alice, conv.ID)
	if conv.Plain || len(fake.withTools) != 1 || !fake.withTools[0] {
		t.Fatalf("an admin gets the assistant with its tools: plain %v, tools %v", conv.Plain, fake.withTools)
	}
	if code := alice.json(t, "GET", "/api/ai/approvals", nil, nil); code != http.StatusOK {
		t.Errorf("approvals for an admin: %d", code)
	}
}

func TestAIPageGivesWayToAnApp(t *testing.T) {
	r, _ := aiRig(t)
	r.install("whoami", "ai.home.test", &state.Access{Public: true})
	if h := r.srv.aiHost(); h != "" {
		t.Fatalf("an app on ai.home.test should have the name, not the AI page (%q)", h)
	}
}
