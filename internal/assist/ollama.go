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
	"strings"
	"time"
)

// MARK: Managing Ollama's models

// OllamaRoot is the address of Ollama's own API for the local model setting (http://127.0.0.1:11434), or "" when the
// local model server isn't Ollama.
func OllamaRoot(ctx context.Context, rawURL string) string {
	base, err := LocalBase(rawURL)
	if err != nil || base == "" {
		return ""
	}
	root := strings.TrimSuffix(base, "/v1")
	var v struct {
		Version string `json:"version"`
	}
	if localRequest(ctx, "GET", root, "/api/version", nil, &v) != nil || v.Version == "" {
		return ""
	}
	return root
}

// Installed is a model Ollama has.
type Installed struct {
	Model    string    `json:"model"`
	Size     int64     `json:"size"` // on disk
	Family   string    `json:"family,omitempty"`
	Params   string    `json:"params,omitempty"` // as Ollama says it: 4.7B
	Quant    string    `json:"quant,omitempty"`
	Modified time.Time `json:"modified"`
	Loaded   bool      `json:"loaded"`
	OnGPU    int       `json:"onGPU,omitempty"` // percent of it in the graphics card's memory, when loaded
}

// OllamaModels lists Ollama's models (embedding models left out), and which are loaded.
func OllamaModels(ctx context.Context, root string) ([]Installed, error) {
	var tags struct {
		Models []struct {
			Name       string    `json:"name"`
			Size       int64     `json:"size"`
			ModifiedAt time.Time `json:"modified_at"`
			Details    struct {
				Family            string   `json:"family"`
				Families          []string `json:"families"`
				ParameterSize     string   `json:"parameter_size"`
				QuantizationLevel string   `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := localRequest(ctx, "GET", root, "/api/tags", nil, &tags); err != nil {
		return nil, err
	}
	loaded := ollamaLoaded(ctx, root)
	out := []Installed{}
	for _, m := range tags.Models {
		if strings.Contains(strings.ToLower(m.Name+" "+m.Details.Family), "embed") || strings.Contains(m.Details.Family, "bert") {
			continue
		}
		in := Installed{Model: m.Name, Size: m.Size, Family: m.Details.Family, Params: m.Details.ParameterSize,
			Quant: m.Details.QuantizationLevel, Modified: m.ModifiedAt}
		if pct, ok := loaded[m.Name]; ok {
			in.Loaded, in.OnGPU = true, pct
		}
		out = append(out, in)
	}
	return out, nil
}

// ollamaLoaded is the models in memory now, with the share of each on the graphics card.
func ollamaLoaded(ctx context.Context, root string) map[string]int {
	var ps struct {
		Models []struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			SizeVRAM int64  `json:"size_vram"`
		} `json:"models"`
	}
	out := map[string]int{}
	if localRequest(ctx, "GET", root, "/api/ps", nil, &ps) != nil {
		return out
	}
	for _, m := range ps.Models {
		pct := 0
		if m.Size > 0 {
			pct = int(m.SizeVRAM * 100 / m.Size)
		}
		out[m.Name] = pct
	}
	return out
}

// OllamaUsesGPU says whether Ollama runs models on the graphics card, going by the loaded ones (ok false: nothing's
// loaded, so it can't tell).
func OllamaUsesGPU(ctx context.Context, root string) (uses, ok bool) {
	for _, pct := range ollamaLoaded(ctx, root) {
		ok = true
		if pct > 0 {
			return true, true
		}
	}
	return false, ok
}

// ModelName checks a name to pull: qwen3.5:4b, llama3.2, someone/model:tag.
var ModelName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)?(:[A-Za-z0-9][A-Za-z0-9._-]*)?$`)

// PullProgress is a download's progress, as Ollama reports it.
type PullProgress struct {
	Status string // "pulling manifest", "downloading", "verifying sha256 digest", "success"
	Done   int64
	Total  int64
}

// OllamaPull downloads a model, reporting progress, until it's done, fails or ctx ends.
func OllamaPull(ctx context.Context, root, model string, on func(PullProgress)) error {
	b, _ := json.Marshal(map[string]any{"model": model, "stream": true})
	req, err := http.NewRequestWithContext(ctx, "POST", root+"/api/pull", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := streamHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach Ollama: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return localError("Ollama", res.StatusCode, data)
	}
	type layer struct{ done, total int64 }
	layers := map[string]*layer{}
	var order []string
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	success := false
	for sc.Scan() {
		var line struct {
			Status    string `json:"status"`
			Digest    string `json:"digest"`
			Total     int64  `json:"total"`
			Completed int64  `json:"completed"`
			Error     string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		if line.Error != "" {
			return errors.New(plainPullError(line.Error))
		}
		if line.Digest != "" && line.Total > 0 {
			l := layers[line.Digest]
			if l == nil {
				l = &layer{}
				layers[line.Digest] = l
				order = append(order, line.Digest)
			}
			l.total, l.done = line.Total, max(l.done, line.Completed)
		}
		p := PullProgress{Status: line.Status}
		for _, d := range order {
			p.Done += layers[d].done
			p.Total += layers[d].total
		}
		if strings.HasPrefix(p.Status, "pulling ") && line.Digest != "" {
			p.Status = "downloading"
		}
		success = success || line.Status == "success"
		if on != nil {
			on(p)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !success {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("the download stopped before it finished")
	}
	return nil
}

func plainPullError(e string) string {
	switch {
	case strings.Contains(e, "file does not exist"), strings.Contains(e, "manifest unknown"):
		return "Ollama's library has no model by that name"
	case strings.Contains(e, "no space left"):
		return "there's not enough disk space for it"
	}
	return e
}

// OllamaDelete removes a model from Ollama.
func OllamaDelete(ctx context.Context, root, model string) error {
	return localRequest(ctx, "DELETE", root, "/api/delete", map[string]string{"model": model}, nil)
}
