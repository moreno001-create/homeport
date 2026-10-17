package server

import (
	"testing"

	"homeport/internal/state"
)

func TestCleanLink(t *testing.T) {
	for in, want := range map[string]state.QuickLink{
		"netflix.com":               {Name: "Netflix", URL: "https://netflix.com"},
		"https://www.chase.com/":    {Name: "Chase", URL: "https://www.chase.com/"},
		"http://192.168.86.1/admin": {Name: "192.168.86.1", URL: "http://192.168.86.1/admin"},
	} {
		got, err := cleanLink(state.QuickLink{URL: in})
		if err != nil || got != want {
			t.Errorf("%s: got %+v %v", in, got, err)
		}
	}
	if got, _ := cleanLink(state.QuickLink{Name: "My bank", URL: "bank.example"}); got.Name != "My bank" {
		t.Errorf("a given name stays: %+v", got)
	}
	for _, bad := range []string{"", "javascript:alert(1)", "ftp://x.com", "https://", "file:///etc/passwd"} {
		if _, err := cleanLink(state.QuickLink{URL: bad}); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}
