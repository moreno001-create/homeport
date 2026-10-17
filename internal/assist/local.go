package assist

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"homeport/internal/state"
)

// The local model is any server that speaks OpenAI's chat completions API with tools: LM Studio, Ollama, llama.cpp,
// vLLM… Homeport doesn't run one itself; a machine with a good GPU on the network does (the server may have none).

// handOverTool lets the local model pass a conversation to Claude.
const handOverTool = "ask_claude"

type oaMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content"` // text, or parts (text and pictures)
	ToolCalls  []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

// LocalBase cleans up the address an admin typed: http://host:1234 becomes http://host:1234/v1.
func LocalBase(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("the local model's address should look like http://192.168.1.20:1234")
	}
	if u.Path == "" {
		u.Path = "/v1"
	}
	return strings.TrimRight(u.String(), "/"), nil
}

var localHTTP = &http.Client{Timeout: 5 * time.Minute}

func localRequest(ctx context.Context, method, base, path string, body any, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := localHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach %s: %w", base, err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if res.StatusCode/100 != 2 {
		return localError(base, res.StatusCode, data)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// localError makes a local server's error answer readable.
func localError(base string, status int, data []byte) error {
	msg := strings.TrimSpace(string(data))
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(data, &e) == nil && e.Error != nil {
		if m, ok := e.Error.(map[string]any); ok && m["message"] != nil {
			msg = fmt.Sprint(m["message"])
		} else {
			msg = fmt.Sprint(e.Error)
		}
	}
	return fmt.Errorf("%s answered %d: %s", base, status, clip(msg, 300))
}

// LocalModels lists the models a local server offers.
func LocalModels(ctx context.Context, base string) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := localRequest(ctx, "GET", base, "/models", nil, &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range out.Data {
		// Embedding models can't chat.
		if !strings.Contains(strings.ToLower(m.ID), "embed") {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

func localTools(tools []Tool, canHandOver bool) []oaTool {
	var out []oaTool
	add := func(name, desc string, props map[string]any, required []string) {
		var t oaTool
		t.Type = "function"
		t.Function.Name, t.Function.Description = name, desc
		if props == nil {
			props = map[string]any{}
		}
		if required == nil {
			required = []string{}
		}
		t.Function.Parameters = map[string]any{"type": "object", "properties": props, "required": required}
		out = append(out, t)
	}
	for _, t := range tools {
		desc := t.Description
		if t.Level != Look {
			desc += " [Waits for the admin's approval.]"
		}
		add(t.Name, desc, t.Params, t.Required)
	}
	if canHandOver {
		add(handOverTool, "Hand this conversation to Claude, a stronger model. Use it when the request is complex (diagnosing a problem, "+
			"a multi-step change, anything you're unsure about) or after a fix didn't work. Don't answer it yourself as well.",
			map[string]any{"reason": str("In a few words, why Claude should take over.")}, []string{"reason"})
	}
	return out
}

func localHistory(system string, turns []Turn) []oaMessage {
	out := []oaMessage{{Role: "system", Content: system}}
	for _, t := range turns {
		switch t.Role {
		case "user":
			if len(t.Images) == 0 {
				out = append(out, oaMessage{Role: "user", Content: t.full()})
				continue
			}
			parts := []map[string]any{{"type": "text", "text": t.full()}}
			for _, im := range t.Images {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{
					"url": "data:" + im.Type + ";base64," + base64.StdEncoding.EncodeToString(im.Data)}})
			}
			out = append(out, oaMessage{Role: "user", Content: parts})
		case "assistant":
			m := oaMessage{Role: "assistant", Content: t.Text}
			for _, call := range t.Calls {
				var tc oaToolCall
				tc.ID, tc.Type, tc.Function.Name = call.ID, "function", call.Name
				args, _ := json.Marshal(call.Args)
				tc.Function.Arguments = string(args)
				m.ToolCalls = append(m.ToolCalls, tc)
			}
			if m.Content != "" || len(m.ToolCalls) > 0 {
				out = append(out, m)
			}
		case "tools":
			for _, r := range t.Results {
				out = append(out, oaMessage{Role: "tool", ToolCallID: r.ID, Content: r.Text})
			}
		}
	}
	return out
}

// runLocal answers with the local model. It returns a reason when the model hands the conversation to Claude.
func (c *Chat) runLocal(ctx context.Context, conv *Conversation, st state.ChatSettings, canHandOver bool) (handOver string, err error) {
	base, err := LocalBase(st.LocalURL)
	if err != nil || base == "" {
		return "", errors.New("no local model is set up yet: add its address under Settings → Assistant chat")
	}
	c.mu.Lock()
	model := cmp.Or(conv.Model, st.LocalModel)
	c.mu.Unlock()
	if model == "" {
		models, err := LocalModels(ctx, base)
		if err != nil {
			return "", err
		}
		if len(models) == 0 {
			return "", errors.New("the local server has no chat model loaded")
		}
		model = models[0]
	}
	for _, t := range c.history(conv) {
		if len(t.Images) > 0 && !c.localSees(st.LocalURL, model) {
			return "", fmt.Errorf("%s can't see pictures: pick a model that can (on the AI models page, models marked \"vision\", like qwen3.5 or gemma4), or start a new chat without the picture", PrettyModel(model))
		}
		if len(t.Images) > 0 {
			break
		}
	}
	who := chatClient(conv.Owner, "local "+model)
	var tools []oaTool
	system := c.plainPrompt(conv.Owner)
	if !conv.Plain {
		tools, system = localTools(c.A.Tools(), canHandOver), c.systemPrompt(conv.Owner)
	}
	if canHandOver {
		system += "\n- You're a small local model. For anything complex, call " + handOverTool + " instead of guessing."
	}
	known := map[string]bool{}
	for _, t := range tools {
		known[t.Function.Name] = true
	}
	empty := 0
	for step := 0; step < localSteps; step++ {
		body := map[string]any{"model": model, "messages": localHistory(system, c.history(conv))}
		if len(tools) > 0 {
			body["tools"] = tools
		}
		c.status(conv, "Reading the conversation…")
		res, err := localStream(ctx, base, body, func(text string, thinking bool) {
			c.update(conv, func(conv *Conversation) {
				shown := strings.TrimSpace(stripThinking(text))
				conv.Partial = shown
				switch {
				case shown != "":
					conv.setStatus("Writing…")
				case thinking || text != "":
					conv.setStatus("Thinking…")
				}
			})
		})
		c.update(conv, func(conv *Conversation) { conv.Partial = "" })
		if err != nil {
			return "", err
		}
		c.noteSpeed(model, res)
		m := res
		text, inText := callsInText(stripThinking(m.Content), known)
		m.Calls = append(m.Calls, inText...)
		turn := Turn{Role: "assistant", By: "local " + model, Text: strings.TrimSpace(text)}
		for _, tc := range m.Calls {
			if tc.Function.Name == handOverTool {
				var a struct {
					Reason string `json:"reason"`
				}
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &a)
				if a.Reason == "" {
					a.Reason = "the local model asked for help"
				}
				// Only what it said goes into the conversation; Claude has no ask_claude tool to see called.
				if turn.Text != "" {
					c.add(conv, Turn{Role: "assistant", By: turn.By, Text: turn.Text})
				}
				return a.Reason, nil
			}
			args := Args{}
			if strings.TrimSpace(tc.Function.Arguments) != "" {
				if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
					args = Args{}
				}
			}
			id := tc.ID
			if id == "" {
				id = "call_" + newID()
			}
			turn.Calls = append(turn.Calls, ToolCall{ID: id, Name: tc.Function.Name, Args: args})
		}
		if turn.Text == "" && len(turn.Calls) == 0 {
			// Small models sometimes only think and never answer; asking again usually works.
			if empty++; empty <= 2 {
				step--
				continue
			}
			return "", errors.New("the local model sent back an empty answer three times")
		}
		c.add(conv, turn)
		if len(turn.Calls) == 0 {
			if !conv.Plain && shellCommand.MatchString(turn.Text) {
				c.note(conv, c.commandNote())
			}
			return "", nil
		}
		c.runTools(ctx, conv, who, turn.Calls)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	if canHandOver {
		return fmt.Sprintf("the local model used %d steps without finishing", localSteps), nil
	}
	c.note(conv, fmt.Sprintf("Paused after %d steps. Say \"go on\" to let it continue.", localSteps))
	return "", nil
}

// stripThinking drops the <think>…</think> some local models put before their answer.
func stripThinking(s string) string {
	for {
		i := strings.Index(s, "<think>")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "</think>")
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+len("</think>"):]
	}
}
