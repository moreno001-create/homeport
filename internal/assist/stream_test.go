package assist

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeStreaming answers like Ollama with "stream": true: thinking first, then a tool call in pieces; after the result,
// an answer word by word that shows a command.
func fakeStreaming(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		call := n
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(delta string) { fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":%s}]}\n\n", delta) }
		send(`{"reasoning":"Let me look."}`)
		if call == 1 {
			send(`{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"list_","arguments":""}}]}`)
			send(`{"tool_calls":[{"index":0,"function":{"name":"apps","arguments":"{}"}}]}`)
		} else {
			for _, w := range []string{"Restart it with:\n", "```bash\n", "sudo systemctl restart ollama\n", "```"} {
				send(fmt.Sprintf("{\"content\":%q}", w))
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLocalChatStreams(t *testing.T) {
	c := newChat(t, fakeStreaming(t).URL+"/v1")
	conv, err := c.Send("alice", "", "What's installed?", "local", false)
	if err != nil {
		t.Fatal(err)
	}
	conv = waitIdle(t, c, conv.ID)
	var roles []string
	for _, tu := range conv.Turns {
		roles = append(roles, tu.Role)
	}
	if got := strings.Join(roles, ","); got != "user,assistant,tools,assistant,note" {
		t.Fatalf("turns = %s (%+v)", got, conv.Turns)
	}
	if c := conv.Turns[1].Calls; len(c) != 1 || c[0].Name != "list_apps" || c[0].ID != "c1" {
		t.Errorf("the tool call in pieces = %+v", c)
	}
	if !strings.Contains(conv.Turns[3].Text, "sudo systemctl restart ollama") {
		t.Errorf("answer = %q", conv.Turns[3].Text)
	}
	if !strings.Contains(conv.Turns[4].Text, "Nothing was run") {
		t.Errorf("note = %q", conv.Turns[4].Text)
	}
	if conv.Partial != "" || conv.Status != "" {
		t.Errorf("left over: partial %q, status %q", conv.Partial, conv.Status)
	}
}

func TestCallsInText(t *testing.T) {
	known := map[string]bool{"list_apps": true, "restart_app": true}
	rest, calls := callsInText("Restarting it.\n<tool_call>\n{\"name\": \"restart_app\", \"arguments\": {\"id\": \"jellyfin\"}}\n</tool_call>", known)
	if rest != "Restarting it." || len(calls) != 1 || calls[0].Function.Name != "restart_app" || calls[0].Function.Arguments != `{"id": "jellyfin"}` {
		t.Errorf("rest %q, calls %+v", rest, calls)
	}
	// Arguments as a string of JSON, and a tool that doesn't exist (left as it was).
	rest, calls = callsInText(`<tool_call>{"name":"list_apps","arguments":"{}"}</tool_call><tool_call>{"name":"rm_rf","arguments":{}}</tool_call>`, known)
	if len(calls) != 1 || calls[0].Function.Arguments != "{}" || !strings.Contains(rest, "rm_rf") {
		t.Errorf("rest %q, calls %+v", rest, calls)
	}
}

func TestShellCommand(t *testing.T) {
	for text, want := range map[string]bool{
		"Run:\n```bash\nsudo systemctl restart caddy\n```": true,
		"```\n$ docker ps\n```":                            true,
		"  sudo apt update":                                true,
		"Jellyfin is running and healthy.":                 false,
		"```yaml\nimage: nginx\n```":                       false,
	} {
		if got := shellCommand.MatchString(text); got != want {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
}
