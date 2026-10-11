package assist

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"homeport/internal/apps"
	"homeport/internal/runner"
	"homeport/internal/state"
)

// Level is how much a tool can do, which decides whether it needs an admin's approval.
type Level string

const (
	// Look reads and reports; it runs straight away.
	Look Level = "look"
	// Change alters the server (installs, removals, restarts…); an admin approves each call.
	Change Level = "change"
	// Shell runs any command on the server: off unless turned on in Settings, and each call is approved.
	Shell Level = "shell"
)

// Tool is one thing an AI may do.
type Tool struct {
	Name        string
	Description string
	Level       Level
	// Params is the JSON Schema "properties"; Required lists the ones that must be given.
	Params   map[string]any
	Required []string
	// Summary says in plain words what a change will do, for the approval request.
	Summary func(args Args) string
	Run     func(ctx context.Context, args Args) (string, error)
}

// Args are a tool call's arguments.
type Args map[string]any

func (a Args) Str(k string) string {
	s, _ := a[k].(string)
	return strings.TrimSpace(s)
}

func (a Args) Int(k string, def, max int) int {
	n := def
	switch v := a[k].(type) {
	case float64:
		n = int(v)
	case int:
		n = v
	}
	if n <= 0 {
		n = def
	}
	if n > max {
		n = max
	}
	return n
}

func (a Args) Bool(k string) bool { b, _ := a[k].(bool); return b }

func (a Args) Strs(k string) []string {
	var out []string
	switch v := a[k].(type) {
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case string:
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// Assistant runs tools for AI helpers, under Homeport's rules.
type Assistant struct {
	Store     *state.Store
	Apps      *apps.Manager
	Runner    runner.Runner
	Caddyfile string
	Version   string
	Audit     *Audit
	Approvals *Approvals
	Memory    *Memory
	// ApplyProxy rewrites Caddy's configuration after changes outside the app manager (domains).
	ApplyProxy func(ctx context.Context) error

	tools []Tool
}

// Limits on what goes back to the AI.
const (
	maxResult   = 30000 // characters
	lookTimeout = 2 * time.Minute
	workTimeout = 30 * time.Minute
)

func (a *Assistant) all() []Tool {
	if a.tools == nil {
		a.tools = a.define()
	}
	return a.tools
}

// Tools lists every tool, with shell tools only when they're turned on.
func (a *Assistant) Tools() []Tool {
	var out []Tool
	for _, t := range a.all() {
		if t.Level == Shell && !a.shellOn() {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (a *Assistant) shellOn() bool {
	on := false
	a.Store.Read(func(d *state.Data) { on = d.Settings().AssistantShell })
	return on
}

func (a *Assistant) approvalTimeout() time.Duration {
	m := 0
	a.Store.Read(func(d *state.Data) { m = d.Settings().ApprovalMinutes })
	if m <= 0 {
		m = 10
	}
	return time.Duration(m) * time.Minute
}

// Result is what a tool call gives back to the AI.
type Result struct {
	Text    string
	IsError bool
}

// Call runs a tool for client (who's asking, for the audit log): checks its arguments, gets an admin's approval for
// changes, runs it, hides secrets in the result, and logs everything.
func (a *Assistant) Call(ctx context.Context, client, name string, args Args) Result {
	start := time.Now()
	argsJSON, _ := json.Marshal(args)
	entry := AuditEntry{Time: start, Client: client, Tool: name, Args: json.RawMessage(Redact(a.Store, string(argsJSON)))}
	finish := func(r Result, decision, by string, err error) Result {
		entry.Decision, entry.By, entry.OK, entry.Millis = decision, by, !r.IsError, time.Since(start).Milliseconds()
		if err != nil {
			entry.Error = Redact(a.Store, err.Error())
		}
		entry.Result = clip(r.Text, 600)
		_ = a.Audit.Write(entry)
		return r
	}

	var tool *Tool
	for _, t := range a.all() {
		if t.Name == name {
			t := t
			tool = &t
			break
		}
	}
	if tool == nil {
		return finish(Result{"There's no tool called " + name + ".", true}, "blocked", "", nil)
	}
	entry.Level = tool.Level
	for _, k := range tool.Required {
		if _, ok := args[k]; !ok {
			return finish(Result{fmt.Sprintf("%s needs %q.", name, k), true}, "blocked", "", nil)
		}
	}
	if tool.Level == Shell && !a.shellOn() {
		return finish(Result{"Shell commands are turned off on this server. An admin can turn them on in Homeport's settings.", true}, "blocked", "", nil)
	}

	decision, by := "ran", ""
	if tool.Level != Look {
		summary := name
		if tool.Summary != nil {
			summary = tool.Summary(args)
		}
		ok, who, timedOut := a.Approvals.Request(ctx.Done(), Approval{Tool: name, Level: tool.Level, Client: client,
			Summary: Redact(a.Store, summary), Args: entry.Args}, a.approvalTimeout())
		switch {
		case timedOut:
			return finish(Result{"No admin approved this in time, so it wasn't done. Ask them to look at Homeport's approvals, then try again.", true}, "timed out", "", nil)
		case !ok:
			return finish(Result{"An admin declined this, so it wasn't done. Ask them what they'd prefer.", true}, "denied", who, nil)
		}
		decision, by = "approved", who
	}

	timeout := lookTimeout
	if tool.Level != Look {
		timeout = workTimeout
	}
	runCtx, cancel := context.WithTimeout(context.WithValue(context.Background(), clientKey{}, client), timeout)
	defer cancel()
	text, err := tool.Run(runCtx, args)
	text = Redact(a.Store, text)
	if err != nil {
		msg := Redact(a.Store, err.Error())
		if text != "" {
			msg = text + "\n\nError: " + msg
		}
		return finish(Result{clip(msg, maxResult), true}, decision, by, err)
	}
	if strings.TrimSpace(text) == "" {
		text = "Done."
	}
	return finish(Result{clip(text, maxResult), false}, decision, by, nil)
}

type clientKey struct{}

// caller is who's using a tool (for memory notes).
func caller(ctx context.Context) string {
	if c, _ := ctx.Value(clientKey{}).(string); c != "" {
		return c
	}
	return "an assistant"
}

// clip keeps the start and the end of long text (logs end with what matters most).
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	head := n / 3
	return s[:head] + fmt.Sprintf("\n…[%d characters left out]…\n", len(s)-n) + s[len(s)-(n-head):]
}
