package assist

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// ModelInfo describes the local model for the AI page: its name, what it can do, and whether it's in memory.
type ModelInfo struct {
	Model       string `json:"model"`       // as the server names it: qwen3.5:4b, qwen/qwen3.6-35b-a3b
	Name        string `json:"name"`        // for people: Qwen 3.5 4B
	Description string `json:"description"` // one or two sentences
	Where       string `json:"where"`       // "this server", or the computer it runs on
	Server      string `json:"server"`      // Ollama, LM Studio, or "" when it's something else
	Loaded      *bool  `json:"loaded,omitempty"`
}

// LocalModelInfo asks the local model's server about model (Ollama's and LM Studio's own APIs; any other server just
// gets a name). forAdmin mentions what admins can do with it.
func LocalModelInfo(ctx context.Context, rawURL, model string, forAdmin bool) ModelInfo {
	info := ModelInfo{Model: model, Name: PrettyModel(model), Where: "this server"}
	base, err := LocalBase(rawURL)
	if err != nil || base == "" {
		return info
	}
	u, _ := url.Parse(base)
	if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
		info.Where = h
	}
	root := strings.TrimSuffix(base, "/v1")
	var params, ctxLen string
	var caps []string

	// Ollama
	var show struct {
		Capabilities []string `json:"capabilities"`
		Details      struct {
			ParameterSize string `json:"parameter_size"`
		} `json:"details"`
	}
	if localRequest(ctx, "POST", root, "/api/show", map[string]string{"model": model}, &show) == nil && len(show.Capabilities) > 0 {
		info.Server, caps, params = "Ollama", show.Capabilities, show.Details.ParameterSize
		var ps struct {
			Models []struct {
				Name          string `json:"name"`
				ContextLength int    `json:"context_length"`
			} `json:"models"`
		}
		if localRequest(ctx, "GET", root, "/api/ps", nil, &ps) == nil {
			loaded := false
			for _, m := range ps.Models {
				if m.Name == model {
					loaded = true
					if m.ContextLength > 0 {
						ctxLen = fmt.Sprintf("%dk", m.ContextLength/1024)
					}
				}
			}
			info.Loaded = &loaded
		}
	} else {
		// LM Studio
		var lm struct {
			State        string   `json:"state"`
			Capabilities []string `json:"capabilities"`
			Type         string   `json:"type"`
		}
		if localRequest(ctx, "GET", root, "/api/v0/models/"+model, nil, &lm) == nil && lm.State != "" {
			info.Server = "LM Studio"
			loaded := lm.State == "loaded"
			info.Loaded = &loaded
			if slices.Contains(lm.Capabilities, "tool_use") {
				caps = append(caps, "tools")
			}
			if lm.Type == "vlm" {
				caps = append(caps, "vision")
			}
		}
	}
	if params == "" {
		if m := paramsRe.FindStringSubmatch(strings.ToLower(model)); m != nil {
			params = strings.ToUpper(m[1])
		}
	}
	info.Description = describe(params, info.Where, caps, ctxLen, forAdmin)
	return info
}

var paramsRe = regexp.MustCompile(`[:\-_/](\d+(?:\.\d+)?b)\b`)

func describe(params, where string, caps []string, ctxLen string, forAdmin bool) string {
	var b strings.Builder
	if params != "" {
		fmt.Fprintf(&b, "A %s-parameter model", params)
	} else {
		b.WriteString("A model")
	}
	if where == "this server" {
		b.WriteString(" running privately on this server: nothing you write here leaves your network.")
	} else {
		fmt.Fprintf(&b, " running privately on %s, on your own network.", where)
	}
	var can []string
	if slices.Contains(caps, "thinking") {
		can = append(can, "thinks problems through before it answers")
	}
	if forAdmin && slices.Contains(caps, "tools") {
		can = append(can, "can look after this server with Homeport's tools (for you as an admin)")
	}
	if ctxLen != "" {
		can = append(can, "keeps about "+ctxLen+" tokens of the conversation in mind")
	}
	switch len(can) {
	case 0:
	case 1:
		b.WriteString(" It " + can[0] + ".")
	default:
		b.WriteString(" It " + strings.Join(can[:len(can)-1], ", ") + " and " + can[len(can)-1] + ".")
	}
	return b.String()
}

var sizeRe = regexp.MustCompile(`^[a-z]?\d+(\.\d+)?[bkm]$`)

// PrettyModel turns a model's id into a name for people: qwen3.5:4b → Qwen 3.5 4B,
// qwen/qwen3.6-35b-a3b → Qwen 3.6 35B A3B, google/gemma-4-e4b → Gemma 4 E4B.
func PrettyModel(id string) string {
	name := id
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name, tag, _ := strings.Cut(name, ":")
	if tag == "latest" {
		tag = ""
	}
	// qwen3.5 → qwen 3.5: a space where the family's letters meet its version.
	if m := regexp.MustCompile(`^([a-z]+)(\d[\d.]*)(.*)$`).FindStringSubmatch(strings.ToLower(name)); m != nil {
		name = m[1] + " " + m[2] + m[3]
	}
	words := strings.FieldsFunc(name+" "+tag, func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	for i, w := range words {
		switch {
		case sizeRe.MatchString(w):
			words[i] = strings.ToUpper(w)
		case w != "":
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// WarmLocal has the local server load its model now (a one-token answer), so the first real answer comes sooner.
func WarmLocal(ctx context.Context, rawURL, model string) error {
	base, err := LocalBase(rawURL)
	if err != nil || base == "" {
		return fmt.Errorf("no local model is set up")
	}
	body := map[string]any{"model": model, "max_tokens": 1, "stream": false,
		"messages": []map[string]string{{"role": "user", "content": "Hi"}}}
	return localRequest(ctx, "POST", base, "/chat/completions", body, nil)
}
