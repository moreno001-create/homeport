package assist

import (
	"strings"
	"testing"
)

func TestGuide(t *testing.T) {
	all := Guide("linux")
	if len(all) < 10 {
		t.Fatalf("only %d guide entries", len(all))
	}
	for _, e := range all {
		if e.Title == "" || e.Signs == "" || !strings.Contains(e.Text, "## Why") || !strings.Contains(e.Text, "## Fix") {
			t.Errorf("%s: needs a title, signs, Why and Fix", e.ID)
		}
	}
	for q, want := range map[string]string{
		"certificate warning in the browser":      "browser-warning",
		"write /var/lib: no space left on device": "disk-full",
		"502 bad gateway on my app":               "app-wont-start",
		"homeport.local doesn't open":             "local-names",
		"the assistant is stuck thinking":         "assistant-slow",
	} {
		if got := SearchGuide("linux", q); len(got) == 0 || got[0].ID != want {
			ids := []string{}
			for _, g := range got {
				ids = append(ids, g.ID)
			}
			t.Errorf("%q: got %v, want %s first", q, ids, want)
		}
	}
	// Linux-only entries aren't shown on a Mac.
	for _, e := range Guide("macos") {
		if e.ID == "installer-hangs" {
			t.Error("the Linux installer entry shows on a Mac")
		}
	}
}
