package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"homeport/internal/apps"
	"homeport/internal/system"
)

// MARK: Maintenance
//
// The Maintenance page keeps the server up to date: Homeport itself (update.go), the system's packages (Ubuntu's
// updates, a restart when one needs it), every app's newer versions, and old app versions' disk space.

func (s *Server) maintenanceRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/maintenance", guard(s.maintenance))
	mux.HandleFunc("POST /api/maintenance/system/check", guard(s.systemCheck))
	mux.HandleFunc("POST /api/maintenance/system/upgrade", guard(s.systemUpgrade))
	mux.HandleFunc("POST /api/maintenance/reboot", guard(s.reboot))
	mux.HandleFunc("POST /api/maintenance/apps/check", guard(s.appsCheck))
	mux.HandleFunc("POST /api/maintenance/apps/update", guard(s.appsUpdate))
	mux.HandleFunc("POST /api/maintenance/prune", guard(s.prune))
}

// upkeep keeps the last checks, so the page shows them without asking again.
type upkeep struct {
	mu      sync.Mutex
	system  *system.SystemUpdates
	apps    []apps.AppUpdate
	appsAt  time.Time
	sysErr  string
	appsErr string
}

func (s *Server) maintenance(w http.ResponseWriter, r *http.Request) {
	s.upkeep.mu.Lock()
	sys, list, at, sysErr := s.upkeep.system, s.upkeep.apps, s.upkeep.appsAt, s.upkeep.sysErr
	s.upkeep.mu.Unlock()
	if sys == nil && s.System != nil {
		// What apt already knows (no asking the servers yet: that's "Check now").
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		u, err := s.System.Updates(ctx, false)
		cancel()
		if err != nil {
			sysErr = err.Error()
		} else {
			sys = &u
		}
	}
	out := map[string]any{"system": sys, "systemError": sysErr, "apps": list}
	if !at.IsZero() {
		out["appsChecked"] = at
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) systemCheck(w http.ResponseWriter, r *http.Request) {
	if s.System == nil {
		writeError(w, http.StatusBadRequest, errors.New("not available here"))
		return
	}
	j := s.jobs.start("check system updates", func(ctx context.Context, j *job) error {
		fmt.Fprintln(j, "Asking the package servers for updates…")
		u, err := s.System.Updates(ctx, true)
		s.upkeep.mu.Lock()
		if err != nil {
			s.upkeep.sysErr = err.Error()
		} else {
			s.upkeep.system, s.upkeep.sysErr = &u, ""
		}
		s.upkeep.mu.Unlock()
		if err != nil {
			return err
		}
		fmt.Fprintf(j, "%d update(s) waiting, %d of them for security.\n", len(u.Packages), u.Security)
		return nil
	})
	writeJSON(w, http.StatusAccepted, j.view())
}

func (s *Server) systemUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.System == nil {
		writeError(w, http.StatusBadRequest, errors.New("not available here"))
		return
	}
	log.Printf("Maintenance: %s installs the system's updates", actor(r).Name)
	j := s.jobs.start("install system updates", func(ctx context.Context, j *job) error {
		fmt.Fprintln(j, "Installing the system's updates (apt-get upgrade)…")
		if err := s.System.Upgrade(ctx, j); err != nil {
			return fmt.Errorf("the updates didn't all install: %w", err)
		}
		u, err := s.System.Updates(ctx, false)
		if err == nil {
			s.upkeep.mu.Lock()
			s.upkeep.system = &u
			s.upkeep.mu.Unlock()
			if u.RebootNeeded {
				fmt.Fprintln(j, "\nDone. Some updates need a restart of the server to take effect (Maintenance → Restart the server).")
			} else {
				fmt.Fprintln(j, "\nDone.")
			}
		}
		return nil
	})
	writeJSON(w, http.StatusAccepted, j.view())
}

func (s *Server) reboot(w http.ResponseWriter, r *http.Request) {
	if s.System == nil {
		writeError(w, http.StatusBadRequest, errors.New("not available here"))
		return
	}
	if busy := s.jobs.busy(); len(busy) > 0 {
		writeError(w, http.StatusConflict, fmt.Errorf("Homeport is busy (%s): restart once that's finished", strings.Join(busy, ", ")))
		return
	}
	log.Printf("Maintenance: %s restarts the server", actor(r).Name)
	if err := s.System.Reboot(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) appsCheck(w http.ResponseWriter, r *http.Request) {
	j := s.jobs.start("check app updates", func(ctx context.Context, j *job) error {
		fmt.Fprintln(j, "Asking each app's registry for its newest version…")
		list := s.Apps.CheckUpdates(ctx)
		n := 0
		for _, a := range list {
			if a.Available {
				n++
			}
		}
		s.upkeep.mu.Lock()
		s.upkeep.apps, s.upkeep.appsAt = list, time.Now()
		s.upkeep.mu.Unlock()
		fmt.Fprintf(j, "%d of %d app(s) have a newer version.\n", n, len(list))
		return nil
	})
	writeJSON(w, http.StatusAccepted, j.view())
}

// appsUpdate updates the apps given (or every app with a newer version), one after another.
func (s *Server) appsUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(in.IDs) == 0 {
		s.upkeep.mu.Lock()
		for _, a := range s.upkeep.apps {
			if a.Available {
				in.IDs = append(in.IDs, a.ID)
			}
		}
		s.upkeep.mu.Unlock()
	}
	if len(in.IDs) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("no app has a newer version (check first)"))
		return
	}
	ids := in.IDs
	j := s.jobs.start("update "+strings.Join(ids, ", "), func(ctx context.Context, j *job) error {
		var failed []string
		for _, id := range ids {
			fmt.Fprintf(j, "\n== %s ==\n", id)
			if err := s.Apps.Update(ctx, id, j); err != nil {
				fmt.Fprintf(j, "%s wasn't updated: %v\n", id, err)
				failed = append(failed, id)
				continue
			}
			s.upkeep.mu.Lock()
			for i := range s.upkeep.apps {
				if s.upkeep.apps[i].ID == id {
					s.upkeep.apps[i].Available = false
					for k := range s.upkeep.apps[i].Images {
						s.upkeep.apps[i].Images[k].Newer = false
					}
				}
			}
			s.upkeep.mu.Unlock()
		}
		if len(failed) > 0 {
			return fmt.Errorf("not updated: %s (the log says why)", strings.Join(failed, ", "))
		}
		fmt.Fprintln(j, "\nDone. Old versions stay on the disk until \"Free disk space\".")
		return nil
	})
	writeJSON(w, http.StatusAccepted, j.view())
}

// prune removes app images nothing uses any more (old versions left by updates) and Docker's build cache.
func (s *Server) prune(w http.ResponseWriter, r *http.Request) {
	j := s.jobs.start("free disk space", func(ctx context.Context, j *job) error {
		fmt.Fprintln(j, "Removing app versions nothing uses any more…")
		if err := s.Apps.Runner.Stream(ctx, "", j, "docker", "image", "prune", "--all", "--force"); err != nil {
			return err
		}
		fmt.Fprintln(j, "Removing Docker's build cache…")
		_ = s.Apps.Runner.Stream(ctx, "", j, "docker", "builder", "prune", "--force")
		return nil
	})
	writeJSON(w, http.StatusAccepted, j.view())
}
