package assist

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"

	"homeport/internal/state"
)

// Chat is the admin panel's assistant: a conversation with Claude or a local model (LM Studio, Ollama…) that uses the
// same tools, approvals, secret hiding and audit log as agents connected over MCP. In "auto" mode easy questions go to
// the local model and hard ones to Claude; the local model can also hand a conversation over when it's out of its depth.
type Chat struct {
	A *Assistant
	// KeyFile holds the Anthropic API key (root-only). ANTHROPIC_API_KEY is used when it's missing.
	KeyFile string
	// UsageFile keeps Claude's estimated spending this month, for the monthly cap.
	UsageFile string
	// ClaudeURL replaces Anthropic's address (tests).
	ClaudeURL string

	mu     sync.Mutex
	convs  map[string]*Conversation
	speeds map[string]float64 // local model → words (tokens) a second, as last measured
}

// Defaults for Claude.
const (
	DefaultClaudeModel = "claude-opus-5-5"
	DefaultEffort      = "medium"
	maxSteps           = 40 // model calls in one answer, tools included
	localSteps         = 12 // a small model going round in circles hands over sooner
	maxConversations   = 30 // per admin, oldest dropped
)

// Turn is one entry in a conversation, kept in a form both kinds of model can read.
type Turn struct {
	// Role is "user", "assistant", "tools" (results of the assistant's tool calls) or "note" (shown, never sent).
	Role    string       `json:"role"`
	Text    string       `json:"text,omitempty"`
	By      string       `json:"by,omitempty"` // which model answered
	Calls   []ToolCall   `json:"calls,omitempty"`
	Results []ToolResult `json:"results,omitempty"`
	Time    time.Time    `json:"time"`
	// claude is Claude's own message, thinking included, sent back unchanged on later requests.
	claude *anthropic.BetaMessageParam
}

type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args Args   `json:"args"`
}

type ToolResult struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Text    string `json:"text"`
	IsError bool   `json:"isError,omitempty"`
}

// Conversation is one chat. They're kept in memory: a restart of Homeport starts afresh.
type Conversation struct {
	ID     string `json:"id"`
	Owner  string `json:"-"`
	Title  string `json:"title"`
	Turns  []Turn `json:"turns"`
	Busy   bool   `json:"busy"`
	Status string `json:"status,omitempty"`
	// StatusFor is how many seconds the status has been the same (so a slow model is seen to be working).
	StatusFor int       `json:"statusFor,omitempty"`
	Partial   string    `json:"partial,omitempty"` // the answer as it's written
	Updated   time.Time `json:"updated"`
	// Consent is set when the chat would send this to Claude and the admin asked to be asked first.
	Consent    bool   `json:"consent,omitempty"`
	ConsentWhy string `json:"consentWhy,omitempty"`
	CanLocal   bool   `json:"canLocal,omitempty"`
	// Plain is a conversation on the AI page with someone who isn't an admin: the local model only, with no tools, so it
	// can't see or change anything on the server, and nothing goes to Claude.
	Plain bool `json:"plain,omitempty"`
	// Model is the local model chosen for this conversation ("": the one in the settings).
	Model string `json:"model,omitempty"`

	claudeTookOver bool
	cancel         context.CancelFunc
	statusAt       time.Time
}

// setStatus changes the status (under the lock), restarting its clock when it's new.
func (conv *Conversation) setStatus(s string) {
	if s != conv.Status {
		conv.Status, conv.statusAt = s, time.Now()
	}
}

// Summary is a conversation in the list.
type Summary struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Updated time.Time `json:"updated"`
	Busy    bool      `json:"busy"`
}

var (
	ErrNoConversation = errors.New("there's no such conversation")
	errBusy           = errors.New("still working on the last message: wait for it, or stop it")
)

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (c *Chat) settings() state.ChatSettings {
	var st state.ChatSettings
	c.A.Store.Read(func(d *state.Data) { st = d.Settings().Chat })
	if st.Mode == "" {
		st.Mode = "auto"
	}
	if st.ClaudeModel == "" {
		st.ClaudeModel = DefaultClaudeModel
	}
	if st.Effort == "" {
		st.Effort = DefaultEffort
	}
	return st
}

// MARK: Conversations

// Send adds the admin's message to a conversation (a new one when id is empty) and starts answering it. use is ""
// (follow the settings), "local" or "claude"; allowCloud answers "ask before sending to Claude" for this message.
func (c *Chat) Send(owner, id, text, use string, allowCloud bool) (Conversation, error) {
	return c.send(owner, id, text, use, allowCloud, false, "")
}

// SendLocal is Send answered by one of the local server's models (model; "" for the one in the settings).
func (c *Chat) SendLocal(owner, id, text, model string) (Conversation, error) {
	return c.send(owner, id, text, "local", false, false, model)
}

// SendPlain is SendLocal for a plain conversation (see Conversation.Plain), for people who aren't admins.
func (c *Chat) SendPlain(owner, id, text, model string) (Conversation, error) {
	return c.send(owner, id, text, "local", false, true, model)
}

func (c *Chat) send(owner, id, text, use string, allowCloud, plain bool, model string) (Conversation, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return Conversation{}, errors.New("write a message first")
	}
	c.mu.Lock()
	if c.convs == nil {
		c.convs = map[string]*Conversation{}
	}
	var conv *Conversation
	if id == "" {
		conv = &Conversation{ID: newID(), Owner: owner, Title: titleFrom(text), Plain: plain}
		c.convs[conv.ID] = conv
		c.dropOldLocked(owner)
	} else if conv = c.convs[id]; conv == nil || conv.Owner != owner || conv.Plain != plain {
		c.mu.Unlock()
		return Conversation{}, ErrNoConversation
	}
	if conv.Busy {
		c.mu.Unlock()
		return Conversation{}, errBusy
	}
	conv.Turns = append(conv.Turns, Turn{Role: "user", Text: text, Time: time.Now()})
	conv.Model = model
	c.startLocked(conv, use, allowCloud)
	out := conv.copyLocked()
	c.mu.Unlock()
	return out, nil
}

// Continue answers what's already in a conversation: after "ask before sending to Claude", or to retry.
func (c *Chat) Continue(owner, id, use string, allowCloud bool) (Conversation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[id]
	if conv == nil || conv.Owner != owner {
		return Conversation{}, ErrNoConversation
	}
	if conv.Busy {
		return Conversation{}, errBusy
	}
	c.startLocked(conv, use, allowCloud)
	return conv.copyLocked(), nil
}

func (c *Chat) startLocked(conv *Conversation, use string, allowCloud bool) {
	ctx, cancel := context.WithCancel(context.Background())
	conv.Busy, conv.Consent, conv.ConsentWhy, conv.Status, conv.Partial, conv.statusAt = true, false, "", "Thinking…", "", time.Now()
	conv.cancel, conv.Updated = cancel, time.Now()
	go c.answer(ctx, conv, use, allowCloud)
}

func (c *Chat) Get(owner, id string) (Conversation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[id]
	if conv == nil || conv.Owner != owner {
		return Conversation{}, ErrNoConversation
	}
	return conv.copyLocked(), nil
}

func (c *Chat) List(owner string) []Summary {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Summary
	for _, conv := range c.convs {
		if conv.Owner == owner {
			out = append(out, Summary{conv.ID, conv.Title, conv.Updated, conv.Busy})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// Stop cancels the answer in progress (and any approval it's waiting for).
func (c *Chat) Stop(owner, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[id]
	if conv == nil || conv.Owner != owner {
		return ErrNoConversation
	}
	if conv.cancel != nil {
		conv.cancel()
	}
	return nil
}

func (c *Chat) Delete(owner, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[id]
	if conv == nil || conv.Owner != owner {
		return ErrNoConversation
	}
	if conv.cancel != nil {
		conv.cancel()
	}
	delete(c.convs, id)
	return nil
}

func (c *Chat) dropOldLocked(owner string) {
	var mine []*Conversation
	for _, conv := range c.convs {
		if conv.Owner == owner && !conv.Busy {
			mine = append(mine, conv)
		}
	}
	if len(mine) <= maxConversations {
		return
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Updated.Before(mine[j].Updated) })
	for _, conv := range mine[:len(mine)-maxConversations] {
		delete(c.convs, conv.ID)
	}
}

func (conv *Conversation) copyLocked() Conversation {
	out := *conv
	out.Turns = append([]Turn(nil), conv.Turns...)
	out.cancel = nil
	if conv.Busy && !conv.statusAt.IsZero() {
		out.StatusFor = int(time.Since(conv.statusAt).Seconds())
	}
	return out
}

func titleFrom(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	if r := []rune(t); len(r) > 60 {
		t = string(r[:57]) + "…"
	}
	return t
}

// update changes a conversation under the lock.
func (c *Chat) update(conv *Conversation, f func(conv *Conversation)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(conv)
	conv.Updated = time.Now()
}

func (c *Chat) add(conv *Conversation, t Turn) {
	t.Time = time.Now()
	c.update(conv, func(conv *Conversation) { conv.Turns = append(conv.Turns, t) })
}

func (c *Chat) note(conv *Conversation, text string) { c.add(conv, Turn{Role: "note", Text: text}) }

func (c *Chat) status(conv *Conversation, s string) {
	c.update(conv, func(conv *Conversation) { conv.setStatus(s) })
}

// noteSpeed keeps how fast a local model wrote, from an answer long enough to tell.
func (c *Chat) noteSpeed(model string, res streamed) {
	if res.Tokens < 20 || res.Writing < time.Second {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.speeds == nil {
		c.speeds = map[string]float64{}
	}
	c.speeds[model] = float64(res.Tokens) / res.Writing.Seconds()
}

// Speed is how many words (tokens) a second a local model wrote last time (0: not measured since Homeport started).
func (c *Chat) Speed(model string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.speeds[model]
}

// commandNote follows an answer that shows a command but didn't run anything.
func (c *Chat) commandNote() string {
	t := "Nothing was run: that command is for you to type on the server (its console, SSH or the web terminal). " +
		"When the assistant does something itself, an Approve button shows up here first."
	if !c.A.shellOn() {
		t += " It can't run commands like that itself: shell commands are off (Settings → AI helpers)."
	}
	return t
}

func (c *Chat) history(conv *Conversation) []Turn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Turn(nil), conv.Turns...)
}

// MARK: Choosing a model

// hardWords mark a request that needs more than a small model: diagnosing, fixing, planning.
var hardWords = regexp.MustCompile(`(?i)\b(why|fix|broken|error|fail\w*|crash\w*|not working|doesn'?t work|isn'?t working|won'?t|can'?t|cannot|debug|troubleshoot|slow|migrat\w*|configur\w*|set ?up|plan|compare|secur\w*|backup|restore|certificate|dns|upgrade)\b`)

// Hard guesses whether a request needs Claude rather than a small local model.
func Hard(text string) bool {
	return len(text) > 400 || hardWords.MatchString(text)
}

type route struct {
	provider string // "claude" or "local"
	why      string
	// canHandOver lets the local model pass the conversation to Claude.
	canHandOver bool
}

func (c *Chat) choose(conv *Conversation, use string, st state.ChatSettings) (route, error) {
	haveClaude, haveLocal := c.APIKey() != "", st.LocalURL != ""
	mode := st.Mode
	if use == "local" || use == "claude" {
		mode = use
	}
	switch mode {
	case "claude":
		if !haveClaude {
			return route{}, errors.New("there's no Anthropic API key yet: add one under Settings → Assistant chat")
		}
		return route{provider: "claude", why: "you chose Claude"}, nil
	case "local":
		if !haveLocal {
			return route{}, errors.New("no local model is set up yet: add its address under Settings → Assistant chat")
		}
		return route{provider: "local"}, nil
	}
	switch {
	case !haveClaude && !haveLocal:
		return route{}, errors.New("no model is set up yet: add an Anthropic API key or a local model (LM Studio, Ollama) under Settings → Assistant chat")
	case !haveLocal:
		return route{provider: "claude", why: "no local model is set up"}, nil
	case !haveClaude:
		return route{provider: "local"}, nil
	}
	c.mu.Lock()
	tookOver := conv.claudeTookOver
	last := ""
	for i := len(conv.Turns) - 1; i >= 0; i-- {
		if conv.Turns[i].Role == "user" {
			last = conv.Turns[i].Text
			break
		}
	}
	c.mu.Unlock()
	switch {
	case tookOver:
		return route{provider: "claude", why: "Claude is already handling this conversation"}, nil
	case Hard(last):
		return route{provider: "claude", why: "this looks like troubleshooting or a bigger job"}, nil
	}
	return route{provider: "local", canHandOver: true}, nil
}

// answer runs one reply, with tool calls, to the end.
func (c *Chat) answer(ctx context.Context, conv *Conversation, use string, allowCloud bool) {
	st := c.settings()
	defer c.update(conv, func(conv *Conversation) {
		conv.Busy, conv.Status, conv.Partial, conv.cancel = false, "", "", nil
	})
	if conv.Plain {
		var err error
		if st.LocalURL == "" {
			err = errors.New("there's no local AI model on this server yet: ask an admin to set one up")
		} else {
			_, err = c.runLocal(ctx, conv, st, false)
		}
		switch {
		case ctx.Err() != nil:
			c.note(conv, "Stopped.")
		case err != nil:
			c.note(conv, "Couldn't answer: "+err.Error())
		}
		return
	}
	r, err := c.choose(conv, use, st)
	for err == nil {
		if r.provider == "claude" {
			if st.AskBeforeCloud && !allowCloud {
				c.update(conv, func(conv *Conversation) {
					conv.Consent, conv.ConsentWhy, conv.CanLocal = true, r.why, st.LocalURL != ""
				})
				return
			}
			c.update(conv, func(conv *Conversation) { conv.claudeTookOver = true })
			err = c.runClaude(ctx, conv, st)
			break
		}
		var handOver string
		handOver, err = c.runLocal(ctx, conv, st, r.canHandOver)
		if err != nil && r.canHandOver && ctx.Err() == nil {
			handOver = "the local model didn't answer (" + err.Error() + ")"
			err = nil
		}
		if handOver == "" {
			break
		}
		c.note(conv, "Handed to Claude: "+handOver)
		r = route{provider: "claude", why: handOver}
	}
	switch {
	case ctx.Err() != nil:
		c.note(conv, "Stopped.")
	case err != nil:
		c.note(conv, "Couldn't answer: "+err.Error())
	}
}

// client names the chat in the audit log and on approval requests.
func chatClient(owner, model string) string { return "chat:" + owner + " (" + model + ")" }

// runTools runs a reply's tool calls in order and records their results.
func (c *Chat) runTools(ctx context.Context, conv *Conversation, who string, calls []ToolCall) {
	levels := map[string]Level{}
	for _, t := range c.A.all() {
		levels[t.Name] = t.Level
	}
	var results []ToolResult
	for _, call := range calls {
		if ctx.Err() != nil {
			results = append(results, ToolResult{ID: call.ID, Name: call.Name, Text: "Stopped by the admin before this ran.", IsError: true})
			continue
		}
		if levels[call.Name] == Look {
			c.status(conv, "Looking: "+call.Name+"…")
		} else {
			c.status(conv, "Waiting for your approval: "+call.Name)
		}
		r := c.A.Call(ctx, who, call.Name, call.Args)
		results = append(results, ToolResult{ID: call.ID, Name: call.Name, Text: r.Text, IsError: r.IsError})
	}
	c.add(conv, Turn{Role: "tools", Results: results})
	c.status(conv, "Thinking…")
}

// plainPrompt is for plain conversations: a private chat on the server, with no tools.
func (c *Chat) plainPrompt(owner string) string {
	var domain, name string
	c.A.Store.Read(func(d *state.Data) {
		domain = d.Settings().MainDomain
		if u, ok := d.User(owner); ok {
			name = cmp.Or(u.DisplayName, u.Name)
		}
	})
	return `You're a helpful AI assistant running privately on the home server ` + domain + `, talking with ` + cmp.Or(name, owner) + `.
- Nothing they write here leaves their own network.
- Answer clearly and to the point. Use Markdown: short paragraphs, lists, **bold** and code blocks where they help.
- You can't see or change anything on the server, browse the web, or remember other conversations. If they ask for
  something like that, say so plainly.`
}

func (c *Chat) systemPrompt(owner string) string {
	var domain string
	c.A.Store.Read(func(d *state.Data) { domain = d.Settings().MainDomain })
	return Instructions + `

You're the assistant in Homeport's admin panel at https://` + domain + `/admin, talking with the admin "` + owner + `".
- They see your replies in a chat. Keep them short and plain; use short lists for steps. No tables.
- When you call a change tool, an Approve button appears for them right in the chat: say what you're asking for first.
- Homeport can't do everything with its tools; when something needs doing by hand, give them the exact command.`
}

// MARK: Claude

// APIKey is the Anthropic key from the key file, or ANTHROPIC_API_KEY.
func (c *Chat) APIKey() string {
	if c.KeyFile != "" {
		if b, err := os.ReadFile(c.KeyFile); err == nil {
			if k := strings.TrimSpace(string(b)); k != "" {
				return k
			}
		}
	}
	return strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
}

// SetAPIKey saves the Anthropic key where only root can read it; an empty key removes it.
func (c *Chat) SetAPIKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		if err := os.Remove(c.KeyFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if strings.ContainsAny(key, " \t\r\n") || len(key) < 20 {
		return errors.New("that doesn't look like an Anthropic API key")
	}
	if err := os.WriteFile(c.KeyFile, []byte(key+"\n"), 0o600); err != nil {
		return err
	}
	return os.Chmod(c.KeyFile, 0o600)
}

// KeyHint shows the end of the key, so an admin can tell which one is saved.
func (c *Chat) KeyHint() string {
	k := c.APIKey()
	if len(k) < 8 {
		return ""
	}
	return "…" + k[len(k)-4:]
}

func claudeTools(tools []Tool) []anthropic.BetaToolUnionParam {
	var out []anthropic.BetaToolUnionParam
	for _, t := range tools {
		props := t.Params
		if props == nil {
			props = map[string]any{}
		}
		desc := t.Description
		if t.Level != Look {
			desc += " [Waits for the admin's approval.]"
		}
		out = append(out, anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name:        t.Name,
			Description: param.NewOpt(desc),
			InputSchema: anthropic.BetaToolInputSchemaParam{Properties: props, Required: t.Required,
				ExtraFields: map[string]any{"additionalProperties": false}},
		}})
	}
	return out
}

var toolIDRe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// claudeID makes a local model's tool call ID acceptable to Claude.
func claudeID(id string) string {
	id = toolIDRe.ReplaceAllString(id, "_")
	if id == "" {
		id = "call_" + newID()
	}
	return id
}

// claudeHistory turns the conversation into Claude's messages. Claude's own replies go back exactly as they came
// (append-only, thinking included); the local model's are rebuilt from text and tool calls.
func claudeHistory(turns []Turn) []anthropic.BetaMessageParam {
	var out []anthropic.BetaMessageParam
	addUser := func(blocks ...anthropic.BetaContentBlockParamUnion) {
		if n := len(out); n > 0 && out[n-1].Role == anthropic.BetaMessageParamRoleUser {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, anthropic.NewBetaUserMessage(blocks...))
	}
	for _, t := range turns {
		switch t.Role {
		case "user":
			addUser(anthropic.NewBetaTextBlock(t.Text))
		case "tools":
			var blocks []anthropic.BetaContentBlockParamUnion
			for _, r := range t.Results {
				blocks = append(blocks, anthropic.NewBetaToolResultBlock(claudeID(r.ID), r.Text, r.IsError))
			}
			if len(blocks) > 0 {
				addUser(blocks...)
			}
		case "assistant":
			if t.claude != nil {
				out = append(out, *t.claude)
				continue
			}
			var blocks []anthropic.BetaContentBlockParamUnion
			if strings.TrimSpace(t.Text) != "" {
				blocks = append(blocks, anthropic.NewBetaTextBlock(t.Text))
			}
			for _, call := range t.Calls {
				args := call.Args
				if args == nil {
					args = Args{}
				}
				blocks = append(blocks, anthropic.NewBetaToolUseBlock(claudeID(call.ID), map[string]any(args), call.Name))
			}
			if len(blocks) > 0 {
				out = append(out, anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant, Content: blocks})
			}
		}
	}
	return out
}

func (c *Chat) runClaude(ctx context.Context, conv *Conversation, st state.ChatSettings) error {
	key := c.APIKey()
	if key == "" {
		return errors.New("there's no Anthropic API key yet: add one under Settings → Assistant chat")
	}
	opts := []option.RequestOption{option.WithAPIKey(key)}
	if c.ClaudeURL != "" {
		opts = append(opts, option.WithBaseURL(c.ClaudeURL))
	}
	client := anthropic.NewClient(opts...)
	tools := claudeTools(c.A.Tools())
	system := []anthropic.BetaTextBlockParam{{Text: c.systemPrompt(conv.Owner), CacheControl: anthropic.NewBetaCacheControlEphemeralParam()}}
	who := chatClient(conv.Owner, st.ClaudeModel)
	for step := 0; step < maxSteps; step++ {
		if err := c.underCap(st); err != nil {
			return err
		}
		params := anthropic.BetaMessageNewParams{
			Model:     st.ClaudeModel,
			MaxTokens: 16000,
			System:    system,
			Messages:  claudeHistory(c.history(conv)),
			Tools:     tools,
			// Caches everything up to the newest message, so each step only pays for what's new.
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
			OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(st.Effort)},
		}
		if hasFallbacks(st.ClaudeModel) {
			// If a safety check declines a request, Anthropic re-runs it on its recommended fallback model.
			params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
			params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
		}
		stream := client.Beta.Messages.NewStreaming(ctx, params)
		msg := anthropic.BetaMessage{}
		for stream.Next() {
			if err := msg.Accumulate(stream.Current()); err != nil {
				return err
			}
			c.update(conv, func(conv *Conversation) { conv.Partial = textOf(msg.Content) })
		}
		if err := stream.Err(); err != nil {
			return claudeError(err)
		}
		c.addSpend(st.ClaudeModel, msg.Usage)
		if msg.StopReason == anthropic.BetaStopReasonRefusal {
			why := string(msg.StopDetails.Category)
			if why != "" {
				why = " (" + why + ")"
			}
			c.update(conv, func(conv *Conversation) { conv.Partial = "" })
			c.note(conv, "Claude declined to help with this"+why+". Try rewording it, or ask something else.")
			return nil
		}
		turn := Turn{Role: "assistant", By: st.ClaudeModel, Text: textOf(msg.Content)}
		for _, b := range msg.Content {
			if u, ok := b.AsAny().(anthropic.BetaToolUseBlock); ok {
				var args Args
				raw, _ := json.Marshal(u.Input)
				_ = json.Unmarshal(raw, &args)
				if args == nil {
					args = Args{}
				}
				turn.Calls = append(turn.Calls, ToolCall{ID: u.ID, Name: u.Name, Args: args})
			}
		}
		p := msg.ToParam()
		turn.claude = &p
		c.add(conv, turn)
		c.update(conv, func(conv *Conversation) { conv.Partial = "" })
		if msg.StopReason != anthropic.BetaStopReasonToolUse {
			if len(turn.Calls) > 0 { // cut off mid-call: every call needs a result before Claude is asked again
				var results []ToolResult
				for _, call := range turn.Calls {
					results = append(results, ToolResult{ID: call.ID, Name: call.Name, Text: "Not run: the reply was cut off.", IsError: true})
				}
				c.add(conv, Turn{Role: "tools", Results: results})
			}
			if msg.StopReason == anthropic.BetaStopReasonMaxTokens {
				c.note(conv, "The reply hit its length limit. Say \"go on\" to continue.")
			}
			return nil
		}
		c.runTools(ctx, conv, who, turn.Calls)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	c.note(conv, fmt.Sprintf("Paused after %d steps. Say \"go on\" to let it continue.", maxSteps))
	return nil
}

// hasFallbacks says whether a model takes server-side fallbacks (Haiku has none).
func hasFallbacks(model string) bool {
	return strings.HasPrefix(model, "claude-opus") || strings.HasPrefix(model, "claude-fable")
}

func textOf(blocks []anthropic.BetaContentBlockUnion) string {
	var parts []string
	for _, b := range blocks {
		if t, ok := b.AsAny().(anthropic.BetaTextBlock); ok && strings.TrimSpace(t.Text) != "" {
			parts = append(parts, strings.TrimSpace(t.Text))
		}
	}
	return strings.Join(parts, "\n\n")
}

func claudeError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case 401:
			return errors.New("Anthropic didn't accept the API key: check it under Settings → Assistant chat")
		case 402, 403:
			return errors.New("Anthropic refused the request (" + apiErr.Error() + "); check the account's billing and permissions")
		case 429:
			return errors.New("Anthropic says too many requests right now; wait a minute and try again")
		case 500, 502, 503, 529:
			return errors.New("Anthropic's service is busy or down right now; try again in a minute")
		}
	}
	return err
}

// MARK: Spending

// prices are US dollars per million tokens: input, output, cache read, cache write.
var prices = map[string][4]float64{
	"claude-opus-5-5":   {4, 20, 0.20, 5},
	"claude-sonnet-5-5": {2, 10, 0.20, 2.5},
	"claude-haiku-5-5":  {0.10, 0.50, 0.01, 0.125},
}

// Usage is Claude's estimated spending in a month.
type Usage struct {
	Month  string  `json:"month"`
	USD    float64 `json:"usd"`
	Input  int64   `json:"inputTokens"`
	Output int64   `json:"outputTokens"`
}

func thisMonth() string { return time.Now().Format("2006-01") }

// Spent is this month's estimated Claude spending.
func (c *Chat) Spent() Usage {
	var u Usage
	if b, err := os.ReadFile(c.UsageFile); err == nil {
		_ = json.Unmarshal(b, &u)
	}
	if u.Month != thisMonth() {
		u = Usage{Month: thisMonth()}
	}
	return u
}

func (c *Chat) addSpend(model string, usage anthropic.BetaUsage) {
	p, ok := prices[model]
	if !ok {
		p = prices[DefaultClaudeModel]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.Spent()
	u.Input += usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	u.Output += usage.OutputTokens
	u.USD += (float64(usage.InputTokens)*p[0] + float64(usage.OutputTokens)*p[1] +
		float64(usage.CacheReadInputTokens)*p[2] + float64(usage.CacheCreationInputTokens)*p[3]) / 1e6
	if b, err := json.Marshal(u); err == nil {
		_ = os.WriteFile(c.UsageFile, b, 0o600)
	}
}

func (c *Chat) underCap(st state.ChatSettings) error {
	if st.MonthlyCapUSD <= 0 {
		return nil
	}
	if u := c.Spent(); u.USD >= st.MonthlyCapUSD {
		return fmt.Errorf("Claude has reached this month's limit (about $%.2f of $%.2f). Raise it under Settings → Assistant chat, or use the local model", u.USD, st.MonthlyCapUSD)
	}
	return nil
}
