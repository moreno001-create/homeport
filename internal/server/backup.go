package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"homeport/internal/auth"
	"homeport/internal/backup"
	"homeport/internal/state"
)

// MARK: Backups

func (s *Server) backupRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/backup", guard(s.backupOverview))
	mux.HandleFunc("PUT /api/backup/settings", guard(s.backupSettings))
	mux.HandleFunc("GET /api/backup/password", guard(s.backupPassword))
	mux.HandleFunc("PUT /api/backup/password", guard(s.setBackupPassword))
	mux.HandleFunc("GET /api/backup/public-key", guard(s.backupPublicKey))
	mux.HandleFunc("POST /api/backup/destinations", guard(s.addBackupDestination))
	mux.HandleFunc("PATCH /api/backup/destinations/{id}", guard(s.changeBackupDestination))
	mux.HandleFunc("DELETE /api/backup/destinations/{id}", guard(s.removeBackupDestination))
	mux.HandleFunc("GET /api/backup/destinations/{id}/snapshots", guard(s.backupSnapshots))
	mux.HandleFunc("POST /api/backup/run", guard(s.runBackup))
	mux.HandleFunc("POST /api/backup/restore", guard(s.restoreBackup))
}

// destView is a destination without its keys and passwords (they're never sent back to the browser).
type destView struct {
	state.BackupDestination
	Where     string `json:"where"`
	HasSecret bool   `json:"hasSecret"`
}

func view(d state.BackupDestination) destView {
	v := destView{BackupDestination: d, HasSecret: d.SecretKey != "" || d.Secret != ""}
	v.SecretKey, v.Secret = "", ""
	switch d.Kind {
	case "sftp":
		v.Where = d.User + "@" + d.Host + ":" + d.Path
	case "s3":
		v.Where = strings.TrimPrefix(d.Endpoint, "https://") + " · " + d.Bucket
	case "homeport":
		v.Where = d.URL + " (as " + d.Username + ")"
	case "local":
		v.Where = d.Path
	}
	return v
}

func (s *Server) backupOverview(w http.ResponseWriter, r *http.Request) {
	if s.Backup == nil {
		writeError(w, http.StatusNotFound, errors.New("backups aren't available on this server"))
		return
	}
	if err := s.Backup.Prepare(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var b state.Backup
	var list []state.App
	s.Store.Read(func(d *state.Data) { b, list = *d.Backup(), d.Apps() })
	dests := []destView{}
	for _, d := range b.Destinations {
		dests = append(dests, view(d))
	}
	appsList := []map[string]string{}
	for _, a := range list {
		appsList = append(appsList, map[string]string{"id": a.ID, "name": a.Name})
	}
	lastLog, _ := os.ReadFile(filepath.Join(s.Backup.Dir, "last-backup.log"))
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{
		"time": b.Time, "keepDaily": b.KeepDaily, "keepWeekly": b.KeepWeekly, "keepMonthly": b.KeepMonthly,
		"noPause": b.NoPause, "skip": append([]string{}, b.Skip...), "destinations": dests, "apps": appsList,
		"running": s.Backup.Running(), "drives": backup.Drives(ctx), "lastLog": string(lastLog), "lastScheduled": b.LastScheduled,
	})
}

var clockRe = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

func (s *Server) backupSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Time    string   `json:"time"`
		Daily   int      `json:"keepDaily"`
		Weekly  int      `json:"keepWeekly"`
		Monthly int      `json:"keepMonthly"`
		NoPause bool     `json:"noPause"`
		Skip    []string `json:"skip"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if in.Time != "" && !clockRe.MatchString(in.Time) {
		writeError(w, http.StatusBadRequest, errors.New(`the time looks like "03:00" (or empty for no daily backup)`))
		return
	}
	for _, n := range []int{in.Daily, in.Weekly, in.Monthly} {
		if n < 0 || n > 400 {
			writeError(w, http.StatusBadRequest, errors.New("keep between 0 and 400 of each"))
			return
		}
	}
	if in.Daily+in.Weekly+in.Monthly == 0 {
		writeError(w, http.StatusBadRequest, errors.New("keep at least one backup"))
		return
	}
	err := s.Store.Update(func(d *state.Data) error {
		b := d.Backup()
		b.Time, b.KeepDaily, b.KeepWeekly, b.KeepMonthly, b.NoPause = in.Time, in.Daily, in.Weekly, in.Monthly, in.NoPause
		b.Skip = slices.DeleteFunc(slices.Clone(in.Skip), func(id string) bool { _, ok := d.App(id); return !ok })
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) backupPassword(w http.ResponseWriter, r *http.Request) {
	var p string
	s.Store.Read(func(d *state.Data) { p = d.Backup().Password })
	writeJSON(w, http.StatusOK, map[string]string{"password": p})
}

// setBackupPassword uses another password: the one of backups made before (restoring onto a new server, say).
func (s *Server) setBackupPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(strings.TrimSpace(in.Password)) < 8 {
		writeError(w, http.StatusBadRequest, errors.New("the password needs at least 8 characters"))
		return
	}
	_ = s.Store.Update(func(d *state.Data) error { d.Backup().Password = strings.TrimSpace(in.Password); return nil })
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) backupPublicKey(w http.ResponseWriter, r *http.Request) {
	key, err := s.Backup.PublicKey(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key})
}

// addBackupDestination checks a new destination (reaching it, and starting the backup store there) before keeping it.
func (s *Server) addBackupDestination(w http.ResponseWriter, r *http.Request) {
	var d state.BackupDestination
	if err := decode(r, &d); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	d.ID, d.LastRun, d.LastOK, d.LastError, d.LastAdded = auth.NewSecret(8), time.Time{}, false, "", 0
	d.Name = strings.TrimSpace(d.Name)
	if d.Name == "" {
		writeError(w, http.StatusBadRequest, errors.New("give it a name"))
		return
	}
	if err := s.Backup.Prepare(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if d.Kind == "sftp" {
		if _, err := s.Backup.PublicKey(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	msg, err := s.Backup.Check(ctx, d)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	_ = s.Store.Update(func(st *state.Data) error { st.Backup().Destinations = append(st.Backup().Destinations, d); return nil })
	writeJSON(w, http.StatusCreated, map[string]any{"message": msg, "destination": view(d)})
}

func (s *Server) changeBackupDestination(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name *string `json:"name"`
		Off  *bool   `json:"off"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	err := s.Store.Update(func(d *state.Data) error {
		for i := range d.Backup().Destinations {
			x := &d.Backup().Destinations[i]
			if x.ID == r.PathValue("id") {
				if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
					x.Name = strings.TrimSpace(*in.Name)
				}
				if in.Off != nil {
					x.Off = *in.Off
				}
				return nil
			}
		}
		return state.ErrNotFound
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// removeBackupDestination forgets a destination; the backups already there stay.
func (s *Server) removeBackupDestination(w http.ResponseWriter, r *http.Request) {
	_ = s.Store.Update(func(d *state.Data) error {
		d.Backup().Destinations = slices.DeleteFunc(d.Backup().Destinations, func(x state.BackupDestination) bool { return x.ID == r.PathValue("id") })
		return nil
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) backupSnapshots(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	list, err := s.Backup.Snapshots(ctx, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) runBackup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Destination string `json:"destination"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if busy := s.Backup.Running(); busy != "" {
		writeError(w, http.StatusConflict, errors.New("already "+busy))
		return
	}
	j := s.jobs.start("backup", func(ctx context.Context, j *job) error { return s.Backup.BackUp(ctx, in.Destination, j) })
	writeJSON(w, http.StatusAccepted, j.view())
}

func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Destination, Snapshot, App string
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if busy := s.Backup.Running(); busy != "" {
		writeError(w, http.StatusConflict, errors.New("already "+busy))
		return
	}
	j := s.jobs.start("restore "+in.App, func(ctx context.Context, j *job) error {
		return s.Backup.RestoreApp(ctx, in.Destination, in.Snapshot, in.App, j)
	})
	writeJSON(w, http.StatusAccepted, j.view())
}
