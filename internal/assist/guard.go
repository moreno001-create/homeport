// Package assist lets an AI help run the server safely: Homeport's abilities as tools at three levels (look, change,
// shell), every change approved by an admin first, secrets hidden from everything the AI sees, and every call written
// to an audit log. It speaks MCP (the Model Context Protocol), so terminal agents like opencode or Claude Code can use
// the same tools as Homeport's own assistant.
package assist

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"homeport/internal/auth"
	"homeport/internal/state"
)

// MARK: Redaction

var (
	privateKeyRe = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	// key: value / key=value pairs whose name says they're secret.
	secretPairRe = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|private[_-]?key)[a-z0-9_.-]*)(["']?\s*[:=]\s*["']?)([^\s"',;]{4,})`)
	bearerRe     = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[a-z0-9._~+/=-]{12,}`)
	// Anthropic API keys (the chat's own key lives in a file a shell command could read).
	anthropicKeyRe = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`)
)

const hidden = "••••••"

// Redact hides what an AI must never see: every app secret Homeport made, private keys, bearer tokens and anything
// labelled as a password, secret, token or key.
func Redact(store *state.Store, text string) string {
	var secrets []string
	store.Read(func(d *state.Data) {
		for _, a := range d.Apps() {
			for _, v := range a.Secrets {
				if len(v) >= 6 {
					secrets = append(secrets, v)
				}
			}
		}
	})
	// Longest first, so a secret that contains another is hidden whole.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, s := range secrets {
		text = strings.ReplaceAll(text, s, hidden)
	}
	text = privateKeyRe.ReplaceAllString(text, "[private key hidden]")
	text = bearerRe.ReplaceAllString(text, "$1 "+hidden)
	text = anthropicKeyRe.ReplaceAllString(text, hidden)
	text = secretPairRe.ReplaceAllString(text, "${1}${2}"+hidden)
	return text
}

// MARK: Audit log

// AuditEntry is one tool call, as written to the audit log.
type AuditEntry struct {
	Time     time.Time       `json:"time"`
	Client   string          `json:"client"` // who asked: "mcp", "assistant (claude-opus-5-5)", …
	Tool     string          `json:"tool"`
	Level    Level           `json:"level"`
	Args     json.RawMessage `json:"args,omitempty"`
	Decision string          `json:"decision"` // ran, approved, denied, timed out, blocked
	By       string          `json:"by,omitempty"`
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Result   string          `json:"result,omitempty"` // the start of what the AI saw
	Millis   int64           `json:"ms"`
}

// Audit appends entries to a JSON-lines file only root can read.
type Audit struct {
	Path string
	mu   sync.Mutex
}

func (a *Audit) Write(e AuditEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, _ := json.Marshal(e)
	_, err = f.Write(append(b, '\n'))
	return err
}

// Recent returns the last n entries, newest first.
func (a *Audit) Recent(n int) ([]AuditEntry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.Open(a.Path)
	if errors.Is(err, os.ErrNotExist) {
		return []AuditEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var e AuditEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			all = append(all, e)
		}
	}
	out := make([]AuditEntry, 0, n)
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, all[i])
	}
	return out, nil
}

// MARK: Approvals

// Approval is a change an AI wants to make, waiting for an admin.
type Approval struct {
	ID      string          `json:"id"`
	Tool    string          `json:"tool"`
	Level   Level           `json:"level"`
	Client  string          `json:"client"`
	Summary string          `json:"summary"` // in plain words: what will happen
	Args    json.RawMessage `json:"args"`
	Created time.Time       `json:"created"`
	Expires time.Time       `json:"expires"`
	done    chan decision
}

type decision struct {
	ok bool
	by string
}

var ErrDenied = errors.New("an admin said no")

// Approvals holds the changes waiting for an admin.
type Approvals struct {
	mu      sync.Mutex
	pending map[string]*Approval
	// Notify, if set, is told about each new request (to wake the admin panel).
	Notify func(Approval)
}

// Request waits until an admin decides, or until timeout (which counts as no).
func (q *Approvals) Request(stop <-chan struct{}, a Approval, timeout time.Duration) (approved bool, by string, timedOut bool) {
	a.ID = auth.NewToken()[:10]
	a.Created, a.Expires = time.Now(), time.Now().Add(timeout)
	a.done = make(chan decision, 1)
	q.mu.Lock()
	if q.pending == nil {
		q.pending = map[string]*Approval{}
	}
	q.pending[a.ID] = &a
	notify := q.Notify
	q.mu.Unlock()
	if notify != nil {
		notify(a)
	}
	defer func() { q.mu.Lock(); delete(q.pending, a.ID); q.mu.Unlock() }()
	select {
	case d := <-a.done:
		return d.ok, d.by, false
	case <-time.After(timeout):
		return false, "", true
	case <-stop:
		return false, "", true
	}
}

// Pending lists the waiting requests, oldest first.
func (q *Approvals) Pending() []Approval {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Approval, 0, len(q.pending))
	for _, a := range q.pending {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Decide approves or denies a waiting request.
func (q *Approvals) Decide(id string, ok bool, by string) error {
	q.mu.Lock()
	a, found := q.pending[id]
	q.mu.Unlock()
	if !found {
		return errors.New("no such request (it may have been decided already, or timed out)")
	}
	select {
	case a.done <- decision{ok, by}:
		return nil
	default:
		return errors.New("that request was already decided")
	}
}
