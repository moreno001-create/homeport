package assist

import "testing"

func TestPrettyModel(t *testing.T) {
	for id, want := range map[string]string{
		"qwen3.5:4b":           "Qwen 3.5 4B",
		"qwen/qwen3.6-35b-a3b": "Qwen 3.6 35B A3B",
		"google/gemma-4-e4b":   "Gemma 4 E4B",
		"llama3.2:latest":      "Llama 3.2",
		"meta/muse-glimmer":    "Muse Glimmer",
	} {
		if got := PrettyModel(id); got != want {
			t.Errorf("PrettyModel(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestDescribe(t *testing.T) {
	got := describe("4.2B", "this server", []string{"completion", "tools", "thinking", "vision"}, "16k", false)
	want := "A 4.2B-parameter model running privately on this server: nothing you write here leaves your network. It thinks problems through before it answers and keeps about 16k tokens of the conversation in mind."
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if got := describe("", "192.168.0.133", []string{"tools"}, "", true); got != "A model running privately on 192.168.0.133, on your own network. It can look after this server with Homeport's tools (for you as an admin)." {
		t.Errorf("admin, another computer: %q", got)
	}
}
