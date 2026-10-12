package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"homeport/internal/apps"
	"homeport/internal/state"
	"homeport/internal/system"
)

// MARK: Services
//
// The Services page: each app's containers (state, CPU, memory, restarts; restart, stop, start, logs) and the server's
// own services (Homeport, Caddy, Docker, Ollama, Avahi…; restart and logs).

type appContainers struct {
	ID         string           `json:"id"`
	Name       string           `json:"name"`
	Domain     string           `json:"domain"`
	Containers []apps.Container `json:"containers"`
	Error      string           `json:"error,omitempty"`
}

func (s *Server) serviceRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/services", guard(s.listServices))
	mux.HandleFunc("GET /api/services/apps/{id}/{service}/logs", guard(s.containerLogs))
	mux.HandleFunc("POST /api/services/apps/{id}/{service}/{action}", guard(s.containerAction))
	mux.HandleFunc("GET /api/services/system/{name}/logs", guard(s.systemLogs))
	mux.HandleFunc("POST /api/services/system/{name}/restart", guard(s.systemRestart))
}

func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var list []state.App
	s.Store.Read(func(d *state.Data) { list = d.Apps() })
	out := make([]appContainers, len(list))
	var wg sync.WaitGroup
	for i, a := range list {
		out[i] = appContainers{ID: a.ID, Name: a.Name, Domain: a.Domain, Containers: []apps.Container{}}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.Apps.Containers(ctx, a.ID)
			if err != nil {
				out[i].Error = err.Error()
				return
			}
			out[i].Containers = c
		}()
	}
	var sys []system.Service
	if s.System != nil {
		wg.Add(1)
		go func() { defer wg.Done(); sys = s.System.List(ctx) }()
	}
	wg.Wait()
	var names []string
	for _, a := range out {
		for _, c := range a.Containers {
			names = append(names, c.Name)
		}
	}
	use := s.Apps.Usage(ctx, names)
	for _, a := range out {
		for j := range a.Containers {
			u := use[a.Containers[j].Name]
			a.Containers[j].CPU, a.Containers[j].Memory, a.Containers[j].Restarts = u.CPU, u.Memory, u.Restarts
		}
	}
	if sys == nil {
		sys = []system.Service{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"apps": out, "system": sys})
}

func lines(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	return min(max(n, 50), 2000)
}

func (s *Server) containerLogs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	id, svc := r.PathValue("id"), r.PathValue("service")
	list, err := s.Apps.Containers(ctx, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	found := false
	for _, c := range list {
		found = found || c.Service == svc
	}
	if !found {
		writeError(w, http.StatusNotFound, errors.New("no such container"))
		return
	}
	out, err := s.Apps.ComposeOutput(ctx, id, "logs", "--no-color", "--timestamps", "--tail", strconv.Itoa(lines(r)), svc)
	if err != nil && out == "" {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"log": out})
}

func (s *Server) containerAction(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.Apps.ContainerAction(ctx, r.PathValue("id"), r.PathValue("service"), r.PathValue("action")); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) systemLogs(w http.ResponseWriter, r *http.Request) {
	if s.System == nil {
		writeError(w, http.StatusNotFound, errors.New("not available on this system"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	out, err := s.System.RecentLogs(ctx, r.PathValue("name"), lines(r))
	if err != nil && out == "" {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"log": out})
}

func (s *Server) systemRestart(w http.ResponseWriter, r *http.Request) {
	if s.System == nil {
		writeError(w, http.StatusNotFound, errors.New("not available on this system"))
		return
	}
	if err := s.System.Restart(r.Context(), r.PathValue("name")); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
