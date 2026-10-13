package assist

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// A local model on a slow machine can read for minutes before its first word (a new conversation starts with
// Homeport's instructions and tools), then write at a few words a second. So answers come streamed, with no limit on
// the whole answer: only on how long the model may stay silent.
const (
	firstWordWait = 20 * time.Minute
	silenceWait   = 3 * time.Minute
)

var streamHTTP = &http.Client{}

// streamed is a finished streamed answer.
type streamed struct {
	Content string
	Calls   []oaToolCall
	Tokens  int           // pieces received (about one per token)
	Writing time.Duration // from the first piece to the last
}

// progress is told about each piece as it comes: everything written so far, and whether the model is still thinking
// (a "reasoning" piece, which isn't part of the answer).
type progress func(content string, thinking bool)

// localStream asks base's chat completions for a streamed answer. Servers that answer in one piece anyway work too.
func localStream(ctx context.Context, base string, body map[string]any, on progress) (streamed, error) {
	body["stream"] = true
	b, err := json.Marshal(body)
	if err != nil {
		return streamed{}, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	quiet := time.AfterFunc(firstWordWait, func() {
		cancel(fmt.Errorf("the local model didn't start answering within %d minutes", int(firstWordWait.Minutes())))
	})
	defer quiet.Stop()

	req, err := http.NewRequestWithContext(ctx, "POST", base+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return streamed{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := streamHTTP.Do(req)
	if err != nil {
		if c := context.Cause(ctx); c != nil && ctx.Err() != nil && !errors.Is(c, context.Canceled) {
			return streamed{}, c
		}
		return streamed{}, fmt.Errorf("can't reach %s: %w", base, err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return streamed{}, localError(base, res.StatusCode, data)
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		return oneShot(res.Body)
	}

	var out streamed
	var text strings.Builder
	calls := map[int]*oaToolCall{}
	var first time.Time
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Error any `json:"error"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if chunk.Error != nil {
			b, _ := json.Marshal(map[string]any{"error": chunk.Error})
			return streamed{}, localError(base, http.StatusBadGateway, b)
		}
		quiet.Reset(silenceWait)
		if first.IsZero() {
			first = time.Now()
		}
		out.Tokens++
		out.Writing = time.Since(first)
		for _, ch := range chunk.Choices {
			d := ch.Delta
			for _, tc := range d.ToolCalls {
				c := calls[tc.Index]
				if c == nil {
					c = &oaToolCall{Type: "function"}
					calls[tc.Index] = c
				}
				if tc.ID != "" {
					c.ID = tc.ID
				}
				c.Function.Name += tc.Function.Name
				c.Function.Arguments += tc.Function.Arguments
			}
			text.WriteString(d.Content)
			if on != nil {
				on(text.String(), d.Content == "" && (d.Reasoning != "" || d.ReasoningContent != ""))
			}
		}
	}
	if err := sc.Err(); err != nil {
		if c := context.Cause(ctx); c != nil && !errors.Is(c, context.Canceled) {
			return streamed{}, c
		}
		return streamed{}, err
	}
	if c := context.Cause(ctx); c != nil && ctx.Err() != nil {
		return streamed{}, c
	}
	out.Content = text.String()
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		out.Calls = append(out.Calls, *calls[i])
	}
	return out, nil
}

// oneShot reads a whole (not streamed) answer.
func oneShot(r io.Reader) (streamed, error) {
	var res struct {
		Choices []struct {
			Message struct {
				Content   string       `json:"content"`
				ToolCalls []oaToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(io.LimitReader(r, 8<<20)).Decode(&res); err != nil {
		return streamed{}, fmt.Errorf("the local model's answer didn't make sense: %w", err)
	}
	if len(res.Choices) == 0 {
		return streamed{}, errors.New("the local model sent back an empty answer")
	}
	m := res.Choices[0].Message
	return streamed{Content: m.Content, Calls: m.ToolCalls}, nil
}

// textToolCall is a tool call some models (Qwen, Hermes) write into their answer when the server didn't turn it into
// a real one: <tool_call>{"name": …, "arguments": {…}}</tool_call>.
var textToolCall = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

// callsInText takes such calls out of the text, keeping only the names of tools there are.
func callsInText(text string, known map[string]bool) (rest string, calls []oaToolCall) {
	rest = textToolCall.ReplaceAllStringFunc(text, func(m string) string {
		var c struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal([]byte(textToolCall.FindStringSubmatch(m)[1]), &c) != nil || !known[c.Name] {
			return m
		}
		tc := oaToolCall{ID: "call_" + newID(), Type: "function"}
		tc.Function.Name = c.Name
		tc.Function.Arguments = string(c.Arguments)
		if s := ""; json.Unmarshal(c.Arguments, &s) == nil {
			tc.Function.Arguments = s // arguments written as a string of JSON
		}
		calls = append(calls, tc)
		return ""
	})
	return strings.TrimSpace(rest), calls
}

// shellCommand spots an answer that shows a command to run on the server (in a code block, or after sudo).
var shellCommand = regexp.MustCompile("(?m)(```(bash|sh|shell|console)?\\s*\\n\\s*\\$?\\s*(sudo|systemctl|docker|apt|homeport|journalctl|ollama|curl)\\b)|(^\\s*\\$?\\s*sudo\\s+\\S)")
