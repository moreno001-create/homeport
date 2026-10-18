package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"homeport/internal/assist"
	"homeport/internal/state"
	"homeport/internal/sysload"
)

// MARK: AI models
//
// The AI models page finds models in Ollama's library, says how well each would run on this machine, downloads them
// with a progress bar, and picks the one the assistant and the AI page use. It works when the local model server is
// Ollama (as the installer sets up); LM Studio has its own place for that.

func (s *Server) modelRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/ai-models", guard(s.aiModels))
	mux.HandleFunc("GET /api/ai-models/search", guard(s.searchModels))
	mux.HandleFunc("POST /api/ai-models/pull", guard(s.pullModel))
	mux.HandleFunc("POST /api/ai-models/stop", guard(s.stopPull))
	mux.HandleFunc("POST /api/ai-models/use", guard(s.useModel))
	mux.HandleFunc("POST /api/ai-models/remove", guard(s.removeModel))
}

// pulls are the downloads in progress (and recently ended ones, so the page sees how they ended).
type pulls struct {
	mu   sync.Mutex
	list map[string]*pull
}

type pull struct {
	Model    string    `json:"model"`
	Status   string    `json:"status"`
	Done     int64     `json:"done"`
	Total    int64     `json:"total"`
	Error    string    `json:"error,omitempty"`
	Finished bool      `json:"finished"`
	Started  time.Time `json:"started"`
	ended    time.Time
	cancel   context.CancelFunc
}

func (p *pulls) views() []pull {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []pull{}
	for m, pl := range p.list {
		if pl.Finished && (pl.Error == "" && time.Since(pl.ended) > 30*time.Second || time.Since(pl.ended) > 10*time.Minute) {
			delete(p.list, m)
			continue
		}
		out = append(out, *pl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

func (p *pulls) busy(model string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	pl := p.list[model]
	return pl != nil && !pl.Finished
}

// ollama is Ollama's API address, and where it runs ("" when that's this server).
func (s *Server) ollama(ctx context.Context) (root, where string, err error) {
	st := s.settings().Chat
	if st.LocalURL == "" {
		return "", "", errors.New("there's no local model server yet: run Homeport's installer again to set up Ollama, or connect one under Settings → Assistant chat")
	}
	root = assist.OllamaRoot(ctx, st.LocalURL)
	if root == "" {
		return "", "", errors.New("the local model server isn't Ollama (LM Studio, say), so its models are managed in its own app")
	}
	if u, err := url.Parse(root); err == nil {
		if h := u.Hostname(); h != "127.0.0.1" && h != "localhost" && h != "::1" {
			where = h
		}
	}
	return root, where, nil
}

// machine is this server, for guessing how models would run. The model in use, measured in a real answer, sets the
// guesses' scale.
func (s *Server) machine(ctx context.Context, root string) assist.Machine {
	m := assist.Machine{Hardware: sysload.ReadHardware(ctx)}
	uses, known := assist.OllamaUsesGPU(ctx, root)
	switch {
	case known:
		m.GPUWorks = uses
	default: // nothing loaded to tell by: NVIDIA cards work with Ollama; many older AMD ones don't; Intel Arc with Vulkan
		m.GPUWorks = m.GPUKind == "nvidia" || m.GPUKind == "intel" && vulkanIntel()
	}
	if cur := s.currentModel(ctx); cur != "" {
		if tps := s.Chat.Speed(cur); tps > 0 {
			p, a := assist.ParamsOf(cur)
			m.Measure(tps, cmp.Or(assist.Weights(ctx, cur), 1), p, a)
		}
	}
	return m
}

func (s *Server) currentModel(ctx context.Context) string {
	_, chosen := s.localChoices(ctx)
	return chosen
}

type installedView struct {
	assist.Installed
	Name     string           `json:"name"`
	Current  bool             `json:"current"`
	Words    float64          `json:"words,omitempty"` // a second, measured in a real answer
	Estimate *assist.Estimate `json:"estimate,omitempty"`
}

// aiModels is the page: this machine, the models Ollama has, and downloads in progress.
func (s *Server) aiModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	out := map[string]any{"pulls": s.pulls.views()}
	root, where, err := s.ollama(ctx)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	installed, err := assist.OllamaModels(ctx, root)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	cur := s.currentModel(ctx)
	var m assist.Machine
	if where == "" {
		m = s.machine(ctx, root)
		out["hardware"] = map[string]any{"hardware": m.Hardware, "gpuWorks": m.GPUWorks}
	}
	views := []installedView{}
	for _, in := range installed {
		v := installedView{Installed: in, Name: assist.PrettyModel(in.Model), Current: in.Model == cur}
		if tps := s.Chat.Speed(in.Model); tps > 0 {
			v.Words = tps * 0.75
		}
		if where == "" {
			p, a := assist.ParamsOf(in.Model)
			e := m.Estimate(cmp.Or(assist.Weights(ctx, in.Model), in.Size), p, a)
			v.Estimate = &e
		}
		views = append(views, v)
	}
	out["installed"], out["current"], out["where"] = views, cur, cmp.Or(where, "this server")
	writeJSON(w, http.StatusOK, out)
}

type familyView struct {
	assist.LibraryModel
	Installed []string `json:"installed"`
	Error     string   `json:"error,omitempty"`
}

// searchModels looks in Ollama's library, with each model's sizes and how they'd run here. q "" is the most popular.
func (s *Server) searchModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	root, where, err := s.ollama(ctx)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	found, err := assist.SearchLibrary(ctx, q)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if len(found) > 10 {
		found = found[:10]
	}
	have := map[string]bool{}
	if installed, err := assist.OllamaModels(ctx, root); err == nil {
		for _, in := range installed {
			have[in.Model] = true
		}
	}
	var m assist.Machine
	if where == "" {
		m = s.machine(ctx, root)
	}
	out := make([]familyView, len(found))
	var wg sync.WaitGroup
	for i, f := range found {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := familyView{LibraryModel: f, Installed: []string{}}
			vs, err := assist.LibraryVariants(ctx, f.Name)
			if err != nil {
				v.Error = err.Error()
			}
			for j := range vs {
				if have[vs[j].Model] {
					v.Installed = append(v.Installed, vs[j].Model)
				}
				if where == "" {
					e := m.Estimate(vs[j].Weights, vs[j].Params, vs[j].Active)
					vs[j].Estimate = &e
				}
			}
			assist.PickBest(vs)
			v.Variants = vs
			if v.Variants == nil {
				v.Variants = []assist.Variant{}
			}
			out[i] = v
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, out)
}

// pullModel starts downloading a model; the page follows it in GET /api/ai-models.
func (s *Server) pullModel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model string `json:"model"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	in.Model = strings.TrimSpace(in.Model)
	if !assist.ModelName.MatchString(in.Model) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%q isn't a model name like qwen3.5:4b", in.Model))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	root, _, err := s.ollama(ctx)
	cancel()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.pulls.mu.Lock()
	if s.pulls.list == nil {
		s.pulls.list = map[string]*pull{}
	}
	if p := s.pulls.list[in.Model]; p != nil && !p.Finished {
		s.pulls.mu.Unlock()
		writeError(w, http.StatusConflict, errors.New("it's already downloading"))
		return
	}
	pctx, stop := context.WithCancel(context.Background())
	p := &pull{Model: in.Model, Status: "starting", Started: time.Now(), cancel: stop}
	s.pulls.list[in.Model] = p
	s.pulls.mu.Unlock()
	log.Printf("AI models: %s downloads %s", actor(r).Name, in.Model)
	go func() {
		defer stop()
		// Ollama tells the parts' sizes one by one; the registry knows the whole download's size from the start.
		sctx, scancel := context.WithTimeout(pctx, 15*time.Second)
		whole := assist.DownloadSize(sctx, in.Model)
		scancel()
		err := assist.OllamaPull(pctx, root, in.Model, func(pr assist.PullProgress) {
			s.pulls.mu.Lock()
			p.Status, p.Done, p.Total = pr.Status, pr.Done, max(pr.Total, whole)
			s.pulls.mu.Unlock()
		})
		s.pulls.mu.Lock()
		defer s.pulls.mu.Unlock()
		p.Finished, p.ended = true, time.Now()
		switch {
		case pctx.Err() != nil:
			p.Status, p.Error = "stopped", "Stopped"
		case err != nil:
			p.Status, p.Error = "failed", err.Error()
		default:
			p.Status = "success"
		}
	}()
	writeJSON(w, http.StatusAccepted, p)
}

func (s *Server) stopPull(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model string `json:"model"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.pulls.mu.Lock()
	p := s.pulls.list[in.Model]
	if p != nil && !p.Finished {
		p.cancel()
	}
	s.pulls.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// useModel makes a model the one the assistant and the AI page use, and loads it so the first answer comes sooner.
func (s *Server) useModel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model string `json:"model"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	models, _ := s.localChoices(ctx)
	found := false
	for _, m := range models {
		found = found || m == in.Model
	}
	if !found {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the local model server has no model %q", in.Model))
		return
	}
	if err := s.Store.Update(func(d *state.Data) error { d.Settings().Chat.LocalModel = in.Model; return nil }); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	log.Printf("AI models: %s chose %s", actor(r).Name, in.Model)
	local := s.settings().Chat.LocalURL
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_ = assist.WarmLocal(ctx, local, in.Model)
	}()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeModel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model string `json:"model"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	root, _, err := s.ollama(ctx)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if in.Model == s.currentModel(ctx) {
		writeError(w, http.StatusBadRequest, errors.New("the assistant uses this model: choose another one first"))
		return
	}
	if s.pulls.busy(in.Model) {
		writeError(w, http.StatusConflict, errors.New("it's still downloading: stop that first"))
		return
	}
	if err := assist.OllamaDelete(ctx, root, in.Model); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	log.Printf("AI models: %s removed %s", actor(r).Name, in.Model)
	w.WriteHeader(http.StatusNoContent)
}

// vulkanIntel says whether Mesa's Vulkan driver for Intel graphics is installed (Ollama needs it for an Arc card).
func vulkanIntel() bool {
	m, _ := filepath.Glob("/usr/share/vulkan/icd.d/intel*.json")
	return len(m) > 0
}
