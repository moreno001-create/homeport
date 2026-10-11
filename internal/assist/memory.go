package assist

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Memory is the assistants' shared notebook (/var/lib/homeport/assistant-memory.md): what was done to what, quirks
// learned, and a hand-off of unfinished work, so the next agent (Claude, a local model, opencode…) carries on where
// the last one stopped. It's plain Markdown, and admins may edit it. Secrets are hidden before anything is written.
type Memory struct {
	Path string
	mu   sync.Mutex
}

const (
	handoffTopic = "In progress"
	maxMemory    = 64 * 1024 // bytes; past this, agents are asked to tidy a section up
	memoryHeader = "# Homeport assistant memory\n\nKept by Homeport's AI helpers: what was done to what, and work in progress. Admins may edit it.\n"
)

type section struct {
	title string
	body  string
}

func (m *Memory) load() ([]section, error) {
	b, err := os.ReadFile(m.Path)
	if errors.Is(err, os.ErrNotExist) {
		return []section{{title: handoffTopic, body: "Nothing.\n"}}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []section
	var cur *section
	for _, line := range strings.SplitAfter(string(b), "\n") {
		if strings.HasPrefix(line, "## ") {
			out = append(out, section{title: strings.TrimSpace(strings.TrimPrefix(line, "## "))})
			cur = &out[len(out)-1]
			continue
		}
		if cur != nil {
			cur.body += line
		}
	}
	if len(out) == 0 || out[0].title != handoffTopic {
		out = append([]section{{title: handoffTopic, body: "Nothing.\n"}}, out...)
	}
	return out, nil
}

func (m *Memory) save(secs []section) (int, error) {
	var b strings.Builder
	b.WriteString(memoryHeader)
	for _, s := range secs {
		fmt.Fprintf(&b, "\n## %s\n%s", s.title, strings.TrimLeft(ensureNewline(s.body), "\n"))
	}
	if b.Len() > maxMemory*2 {
		return b.Len(), fmt.Errorf("the memory would be %d KB; tidy a section with rewrite_memory_topic first", b.Len()/1024)
	}
	tmp := m.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return 0, err
	}
	return b.Len(), os.Rename(tmp, m.Path)
}

func ensureNewline(s string) string {
	if s != "" && !strings.HasSuffix(s, "\n") {
		return s + "\n"
	}
	return s
}

// Read gives the whole memory.
func (m *Memory) Read() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := os.ReadFile(m.Path)
	if errors.Is(err, os.ErrNotExist) {
		return memoryHeader + "\n## " + handoffTopic + "\nNothing.\n", nil
	}
	return string(b), err
}

// cleanTopic keeps topics to one line: "server", "app: nextcloud", "domain: example.com"…
func cleanTopic(t string) string {
	t = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(t, "\n", " "), "#", ""))
	if len(t) > 80 {
		t = t[:80]
	}
	return t
}

// Remember adds a dated note under a topic (making the topic if it's new).
func (m *Memory) Remember(topic, note, who string) (string, error) {
	topic, note = cleanTopic(topic), strings.TrimSpace(note)
	if topic == "" || note == "" {
		return "", errors.New("give both a topic and a note")
	}
	if strings.EqualFold(topic, handoffTopic) {
		return "", errors.New("use set_handoff for work in progress")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	secs, err := m.load()
	if err != nil {
		return "", err
	}
	line := fmt.Sprintf("- %s (%s): %s\n", time.Now().Format("2006-01-02 15:04"), who, strings.ReplaceAll(note, "\n", "\n  "))
	found := false
	for i := range secs {
		if strings.EqualFold(secs[i].title, topic) {
			secs[i].body = ensureNewline(secs[i].body) + line
			found = true
		}
	}
	if !found {
		secs = append(secs, section{title: topic, body: line})
	}
	size, err := m.save(secs)
	if err != nil {
		return "", err
	}
	msg := "Noted under \"" + topic + "\"."
	if size > maxMemory {
		msg += fmt.Sprintf(" The memory is getting long (%d KB): when convenient, condense the longest topic with rewrite_memory_topic.", size/1024)
	}
	return msg, nil
}

// SetHandoff replaces the work-in-progress note (empty clears it).
func (m *Memory) SetHandoff(text, who string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	secs, err := m.load()
	if err != nil {
		return "", err
	}
	body := "Nothing.\n"
	if t := strings.TrimSpace(text); t != "" {
		body = fmt.Sprintf("Left by %s at %s:\n\n%s\n", who, time.Now().Format("2006-01-02 15:04"), t)
	}
	secs[0].body = body
	if _, err := m.save(secs); err != nil {
		return "", err
	}
	if body == "Nothing.\n" {
		return "Hand-off cleared: nothing is in progress.", nil
	}
	return "Hand-off saved: the next agent will see it first.", nil
}

// RewriteTopic replaces a topic's notes with a condensed version (empty removes the topic).
func (m *Memory) RewriteTopic(topic, content string) (string, error) {
	topic = cleanTopic(topic)
	if strings.EqualFold(topic, handoffTopic) {
		return "", errors.New("use set_handoff for work in progress")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	secs, err := m.load()
	if err != nil {
		return "", err
	}
	out := secs[:0]
	found := false
	for _, s := range secs {
		if strings.EqualFold(s.title, topic) {
			found = true
			if strings.TrimSpace(content) == "" {
				continue
			}
			s.body = strings.TrimSpace(content) + "\n"
		}
		out = append(out, s)
	}
	if !found {
		return "", fmt.Errorf("there's no topic %q in the memory", topic)
	}
	if _, err := m.save(out); err != nil {
		return "", err
	}
	return "Rewrote \"" + topic + "\".", nil
}
