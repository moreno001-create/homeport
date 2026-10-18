package assist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"homeport/internal/sysload"
)

// MARK: Ollama's library
//
// Ollama's model library (ollama.com) has no search API, so Homeport reads its search and tags pages, and the
// registry's manifests (exact sizes). If the pages change, search shows an error and installing by name still works.

// LibraryModel is a model family in Ollama's library: qwen3.5, llama3.2…
type LibraryModel struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Caps        []string  `json:"caps"`  // tools, vision, thinking…
	Sizes       []string  `json:"sizes"` // as the library lists them: 4b, 35b-a3b
	Downloads   string    `json:"downloads,omitempty"`
	Variants    []Variant `json:"variants"`
}

// Variant is one size of a model: what Ollama pulls as name:tag.
type Variant struct {
	Model    string    `json:"model"`            // qwen3.5:4b
	Download int64     `json:"download"`         // bytes
	Weights  int64     `json:"weights"`          // the model itself (no image reader or draft model): what's read for every word
	Params   float64   `json:"params"`           // billions
	Active   float64   `json:"active,omitempty"` // billions at work per word (mixture of experts)
	Context  string    `json:"context,omitempty"`
	Estimate *Estimate `json:"estimate,omitempty"`
	Best     bool      `json:"best,omitempty"` // the one to pick on this machine
}

var (
	libraryHTTP = &http.Client{Timeout: 20 * time.Second}
	libraryURL  = "https://ollama.com"
	registryURL = "https://registry.ollama.ai"

	libCache   sync.Map // url → cached
	searchTTL  = time.Hour
	detailsTTL = 6 * time.Hour
)

type cached struct {
	at   time.Time
	body string
}

func libraryGet(ctx context.Context, u string, ttl time.Duration) (string, error) {
	if c, ok := libCache.Load(u); ok && time.Since(c.(cached).at) < ttl {
		return c.(cached).body, nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Homeport")
	// Ollama's pages answer in JSON (without the sizes) to anything that would take it.
	if strings.Contains(u, "/v2/") { // the registry
		req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	} else {
		req.Header.Set("Accept", "text/html")
	}
	res, err := libraryHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("can't reach Ollama's library: %w", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode == http.StatusNotFound {
		return "", errNotInLibrary
	}
	if res.StatusCode/100 != 2 {
		return "", fmt.Errorf("Ollama's library answered %d", res.StatusCode)
	}
	libCache.Store(u, cached{time.Now(), string(b)})
	return string(b), nil
}

var errNotInLibrary = errors.New("Ollama's library has no such model")

var (
	resultName  = regexp.MustCompile(`^([a-z0-9][a-z0-9._-]*)"`)
	resultDesc  = regexp.MustCompile(`<p[^>]*title="([^"]*)"|<p class="max-w-lg[^"]*">([^<]+)</p>`)
	resultBadge = regexp.MustCompile(`<span\s+class="[^"]*(bg-indigo-50|bg-\[#ddf4ff\])[^"]*"[^>]*>([^<]+)</span>`)
	resultPulls = regexp.MustCompile(`title="([\d,]+) downloads"|<span >([\d.]+[KMB]?)</span>\s*<span class="hidden sm:flex">&nbsp;Pulls`)
)

// SearchLibrary finds models in Ollama's library (q "": the most popular). Embedding models (which can't chat) and
// cloud-only ones are left out.
func SearchLibrary(ctx context.Context, q string) ([]LibraryModel, error) {
	u := libraryURL + "/search?q=" + url.QueryEscape(q)
	if q == "" {
		u = libraryURL + "/library?sort=popular"
	}
	page, err := libraryGet(ctx, u, searchTTL)
	if err != nil {
		return nil, err
	}
	var out []LibraryModel
	parts := strings.Split(page, `<a href="/library/`)
	for _, part := range parts[1:] {
		m := resultName.FindStringSubmatch(part)
		if m == nil {
			continue
		}
		if end := strings.Index(part, "</li>"); end > 0 {
			part = part[:end]
		}
		lm := LibraryModel{Name: m[1], Caps: []string{}, Sizes: []string{}}
		if d := resultDesc.FindStringSubmatch(part); d != nil {
			lm.Description = strings.TrimSpace(html.UnescapeString(d[1] + d[2]))
		}
		for _, b := range resultBadge.FindAllStringSubmatch(part, -1) {
			v := strings.TrimSpace(html.UnescapeString(b[2]))
			if b[1] == "bg-indigo-50" {
				lm.Caps = append(lm.Caps, v)
			} else {
				lm.Sizes = append(lm.Sizes, v)
			}
		}
		if p := resultPulls.FindStringSubmatch(part); p != nil {
			lm.Downloads = p[2]
			if p[1] != "" {
				lm.Downloads = shortCount(p[1])
			}
		}
		if hasAny(lm.Caps, "embedding") || len(lm.Sizes) == 0 { // no sizes: only in Ollama's cloud
			continue
		}
		out = append(out, lm)
	}
	if len(out) == 0 && !strings.Contains(page, "/library/") && q == "" {
		return nil, errors.New("couldn't read Ollama's library page (it may have changed)")
	}
	return out, nil
}

func hasAny(list []string, want ...string) bool {
	for _, l := range list {
		for _, w := range want {
			if l == w {
				return true
			}
		}
	}
	return false
}

func shortCount(s string) string {
	n, _ := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.FormatInt(n/1000, 10) + "K"
	}
	return strconv.FormatInt(n, 10)
}

// sizeTag is a tag that's just a size: 4b, 0.8b, 270m, e4b, 35b-a3b (Ollama's usual build of that size).
var sizeTag = regexp.MustCompile(`^(e)?(\d+(?:\.\d+)?)([bm])(?:-a(\d+(?:\.\d+)?)b)?$`)

// mixtures are mixtures of experts whose names don't say so: billions at work per word.
var mixtures = map[string]float64{
	"gpt-oss:20b": 3.6, "gpt-oss:120b": 5.1,
	"qwen3:30b": 3, "qwen3:235b": 22, "qwen3-coder:30b": 3, "qwen3-coder:480b": 35,
	"deepseek-v3:671b": 37, "deepseek-r1:671b": 37, "mixtral:8x7b": 13, "mixtral:8x22b": 39,
}

// ParamsOf reads a model's size from its tag (qwen3.5:35b-a3b → 35 billion, 3 at work). 0 when the tag doesn't say.
func ParamsOf(model string) (params, active float64) {
	name, tag, _ := strings.Cut(model, ":")
	defer func() {
		if a, ok := mixtures[name+":"+tag]; ok && active == 0 {
			active = a
		}
	}()
	m := sizeTag.FindStringSubmatch(tag)
	if m == nil {
		return 0, 0
	}
	params, _ = strconv.ParseFloat(m[2], 64)
	if m[3] == "m" {
		params /= 1000
	}
	if m[4] != "" {
		active, _ = strconv.ParseFloat(m[4], 64)
	}
	return params, active
}

var (
	tagEntry   = regexp.MustCompile(`<a href="/library/([a-z0-9._/-]+:[A-Za-z0-9._-]+)" class="md:hidden`)
	tagSize    = regexp.MustCompile(`•\s*([\d.]+)\s*(GB|MB|KB)`)
	tagContext = regexp.MustCompile(`([\d.]+[KM]) context window`)
)

// LibraryVariants lists a model's sizes (the plain size tags; "latest" only when there are none), with exact sizes.
func LibraryVariants(ctx context.Context, name string) ([]Variant, error) {
	page, err := libraryGet(ctx, libraryURL+"/library/"+url.PathEscape(name)+"/tags", detailsTTL)
	if err != nil {
		return nil, err
	}
	var out []Variant
	var latest *Variant
	seen := map[int64]int{} // download size → index (35b and 35b-a3b are the same download)
	locs := tagEntry.FindAllStringSubmatchIndex(page, -1)
	for i, loc := range locs {
		model := page[loc[2]:loc[3]]
		end := len(page)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		block := page[loc[1]:min(end, loc[1]+3000)]
		v := Variant{Model: model}
		if s := tagSize.FindStringSubmatch(block); s != nil {
			v.Download = parseBytes(s[1], s[2])
		}
		if c := tagContext.FindStringSubmatch(block); c != nil {
			v.Context = c[1]
		}
		if v.Download == 0 { // cloud-only
			continue
		}
		v.Params, v.Active = ParamsOf(model)
		_, tag, _ := strings.Cut(model, ":")
		switch {
		case tag == "latest":
			l := v
			latest = &l
		case v.Params > 0:
			if j, ok := seen[v.Download]; ok {
				if v.Active > 0 { // keep the name that says it's a mixture of experts
					out[j] = v
				}
				continue
			}
			seen[v.Download] = len(out)
			out = append(out, v)
		}
	}
	if len(out) == 0 && latest != nil {
		out = append(out, *latest)
	}
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i].Weights = Weights(ctx, out[i].Model)
			if out[i].Weights == 0 {
				out[i].Weights = out[i].Download
			}
		}()
	}
	wg.Wait()
	return out, nil
}

func parseBytes(n, unit string) int64 {
	f, _ := strconv.ParseFloat(n, 64)
	switch unit {
	case "GB":
		return int64(f * 1e9)
	case "MB":
		return int64(f * 1e6)
	}
	return int64(f * 1e3)
}

// Weights is the size of a model's own weights, from the registry (0 if it can't tell).
func Weights(ctx context.Context, model string) int64 {
	w, _ := manifestSizes(ctx, model)
	return w
}

// DownloadSize is everything Ollama downloads for a model (0 if the registry can't tell).
func DownloadSize(ctx context.Context, model string) int64 {
	_, all := manifestSizes(ctx, model)
	return all
}

func manifestSizes(ctx context.Context, model string) (weights, all int64) {
	name, tag, ok := strings.Cut(model, ":")
	if !ok {
		tag = "latest"
	}
	if !strings.Contains(name, "/") {
		name = "library/" + name
	}
	body, err := libraryGet(ctx, registryURL+"/v2/"+name+"/manifests/"+tag, detailsTTL)
	if err != nil {
		return 0, 0
	}
	var m struct {
		Config struct {
			Size int64 `json:"size"`
		} `json:"config"`
		Layers []struct {
			MediaType string `json:"mediaType"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	if json.Unmarshal([]byte(body), &m) != nil {
		return 0, 0
	}
	all = m.Config.Size
	for _, l := range m.Layers {
		all += l.Size
		if l.MediaType == "application/vnd.ollama.image.model" {
			weights += l.Size
		}
	}
	return weights, all
}

// MARK: Will it run well here?

// Machine is where models would run, for guessing how fast they'd be.
type Machine struct {
	sysload.Hardware
	// GPUWorks: Ollama runs models on the graphics card (false: it's too old for Ollama, or there's none).
	GPUWorks bool
	// Measured is how fast a model really ran: memory read per second (bytes), and where it ran.
	Measured      float64
	MeasuredPlace string
}

// Estimate is how a model would run on this machine.
type Estimate struct {
	Place    string  `json:"place"` // "graphics card", "graphics", "processor", "graphics card and processor", "" (too big)
	Words    float64 `json:"words"` // a second, roughly
	Label    string  `json:"label"` // Fast, Good, Usable, Slow, Too big
	Measured bool    `json:"measured,omitempty"`
}

// Each word means reading the model's weights once (only the experts at work, for a mixture of experts), so speed is
// memory speed over weights read. These are cautious guesses of the memory speed a model really gets; one real answer
// replaces them (Measure).
const (
	gbs            = 1e9
	nvidiaSpeed    = 250 * gbs
	amdSpeed       = 180 * gbs
	intelSpeed     = 90 * gbs // an Arc card through Vulkan (measured on a B580)
	appleSpeed     = 90 * gbs
	cpuSpeed       = 20 * gbs // a recent processor (AVX2)
	oldCPUSpeed    = 9 * gbs  // without AVX2
	wordsPerToken  = 0.75
	usableWords    = 3       // a second: slower than that, waiting for answers gets tiresome
	osAndAppsSpare = 3 << 30 // memory left for the system and apps
)

// readPerToken is how much of the model is read for each token. A mixture of experts reads only the experts at work,
// but jumping between them costs some speed.
func readPerToken(weights int64, params, active float64) float64 {
	r := float64(weights)
	if active > 0 && params > 0 {
		r *= active / params * 1.5
	}
	return r
}

// place says where a model of this size would run.
func (m Machine) place(weights int64) (string, float64) {
	w := float64(weights)
	switch {
	case m.Apple:
		if w <= float64(m.Memory)*0.7 {
			return "graphics", appleSpeed
		}
		return "", 0
	case m.GPUWorks && m.GPUMemory > 0 && w*1.1+512<<20 <= float64(m.GPUMemory):
		switch m.GPUKind {
		case "amd":
			return "graphics card", amdSpeed
		case "intel":
			return "graphics card", intelSpeed
		}
		return "graphics card", nvidiaSpeed
	}
	cpu := float64(cpuSpeed)
	if !m.AVX2 {
		cpu = oldCPUSpeed
	}
	room := float64(m.Memory - osAndAppsSpare)
	if m.GPUWorks {
		room += float64(m.GPUMemory)
		if w <= room {
			return "graphics card and processor", cpu * 1.3
		}
	}
	if w <= room {
		return "processor", cpu
	}
	return "", 0
}

// Estimate guesses how a model would run here.
func (m Machine) Estimate(weights int64, params, active float64) Estimate {
	if weights <= 0 {
		return Estimate{}
	}
	place, speed := m.place(weights)
	if place == "" {
		return Estimate{Label: "Too big"}
	}
	e := Estimate{Place: place}
	if m.Measured > 0 && m.MeasuredPlace == place {
		speed, e.Measured = m.Measured, true
	}
	e.Words = speed / readPerToken(weights, params, active) * wordsPerToken
	switch {
	case e.Words >= 15:
		e.Label = "Fast"
	case e.Words >= 6:
		e.Label = "Good"
	case e.Words >= usableWords:
		e.Label = "Usable"
	default:
		e.Label = "Slow"
	}
	return e
}

// Measure takes a real answer's speed (tokens a second) for a model into account.
func (m *Machine) Measure(tokensPerSec float64, weights int64, params, active float64) {
	place, _ := m.place(weights)
	if tokensPerSec <= 0 || place == "" {
		return
	}
	m.Measured, m.MeasuredPlace = tokensPerSec*readPerToken(weights, params, active), place
}

// PickBest marks the variant to choose: the most capable (biggest) one that still answers at a usable speed here.
// None is marked when no size is usable here.
func PickBest(vs []Variant) {
	best := -1
	for i, v := range vs {
		if v.Estimate == nil || v.Estimate.Words < usableWords {
			continue
		}
		if best < 0 || v.Weights > vs[best].Weights {
			best = i
		}
	}
	if best >= 0 {
		vs[best].Best = true
	}
}
