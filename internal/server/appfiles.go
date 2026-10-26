package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"homeport/internal/aria"
	"homeport/internal/catalog"
	"homeport/internal/state"
)

// MARK: An app's files
//
// Apps whose catalog entry has "files" (Kiwix's ZIM files, say) get a Files button on the Apps page. A link pasted
// there (an ordinary download link, a magnet link or a .torrent link) is downloaded on the server into the app's
// folder by aria2, which resumes after interruptions; the page shows each download's progress, speed and peers, with
// pause, resume and cancel. Files already there are listed and can be deleted.

func (s *Server) appFilesRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/apps/{id}/files", guard(s.listAppFiles))
	mux.HandleFunc("POST /api/apps/{id}/files", guard(s.addAppDownload))
	mux.HandleFunc("POST /api/apps/{id}/downloads/{gid}/{action}", guard(s.appDownloadAction))
	mux.HandleFunc("DELETE /api/apps/{id}/files/{name}", guard(s.deleteAppFile))
}

type appFile struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// filesOf is an app's files setting and its folder (made if missing).
func (s *Server) filesOf(id string) (catalog.Files, string, error) {
	var app state.App
	var ok bool
	s.Store.Read(func(d *state.Data) {
		if a, found := d.App(id); found {
			app, ok = *a, true
		}
	})
	if !ok {
		return catalog.Files{}, "", state.ErrNotFound
	}
	e, ok := s.Apps.Entry(app)
	if !ok || e.Files == nil {
		return catalog.Files{}, "", fmt.Errorf("%s doesn't take files from here", app.Name)
	}
	dir := filepath.Join(s.Apps.Dir(id), "data", filepath.Clean("/"+e.Files.Folder))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return catalog.Files{}, "", err
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	return *e.Files, dir, nil
}

// downloadsIn are aria2's downloads into dir.
func (s *Server) downloadsIn(ctx context.Context, dir string) []aria.Download {
	out := []aria.Download{}
	if s.Aria == nil || !s.Aria.Running(ctx) {
		return out
	}
	list, err := s.Aria.List(ctx)
	if err != nil {
		return out
	}
	for _, d := range list {
		if filepath.Clean(d.Dir) == dir {
			out = append(out, d)
		}
	}
	return out
}

func (s *Server) listAppFiles(w http.ResponseWriter, r *http.Request) {
	f, dir, err := s.filesOf(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	downloads := s.downloadsIn(ctx, dir)
	going := map[string]bool{}
	for _, d := range downloads {
		if d.Status != "complete" {
			going[d.Name] = true
		}
	}
	entries, _ := os.ReadDir(dir)
	files := []appFile{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, f.Ext) || going[name] {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, name+".aria2")); err == nil {
			continue // still being downloaded (aria2's control file is beside it)
		}
		if info, err := e.Info(); err == nil {
			files = append(files, appFile{Name: name, Size: info.Size(), Modified: info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	var free int64
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) == nil {
		free = int64(st.Bavail) * int64(st.Bsize)
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "downloads": downloads, "free": free,
		"ext": f.Ext, "find": f.Find, "what": f.What})
}

// checkLink accepts an ordinary download link to a file of this kind, a .torrent link, or a magnet link.
func checkLink(link, ext string) (string, error) {
	link = strings.TrimSpace(link)
	if strings.HasPrefix(strings.ToLower(link), "magnet:?") {
		if !strings.Contains(link, "xt=urn:bt") {
			return "", errors.New("that magnet link has no torrent in it (xt=urn:btih:…)")
		}
		return link, nil
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("paste a download link (https://…), a magnet link (magnet:?…) or a .torrent link")
	}
	name := strings.ToLower(path.Base(u.Path))
	if !strings.HasSuffix(name, strings.ToLower(ext)) && !strings.HasSuffix(name, ".torrent") {
		return "", fmt.Errorf("that link isn't a %s file or a .torrent (its address should end in %s or .torrent)", ext, ext)
	}
	return u.String(), nil
}

func (s *Server) addAppDownload(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	f, dir, err := s.filesOf(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	link, err := checkLink(in.URL, f.Ext)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.Aria == nil {
		writeError(w, http.StatusBadRequest, errors.New("downloads aren't available on this server"))
		return
	}
	log.Printf("Files: %s adds a download for %s: %s", actor(r).Name, id, link)
	// A job, since the first download may have to install aria2.
	j := s.jobs.start("download for "+id, func(ctx context.Context, j *job) error {
		if err := s.Aria.Ready(ctx, j); err != nil {
			return err
		}
		if _, err := s.Aria.Add(ctx, link, dir); err != nil {
			return err
		}
		fmt.Fprintln(j, "Added: its progress shows in the list of files.")
		return nil
	})
	writeJSON(w, http.StatusAccepted, j.view())
}

func (s *Server) appDownloadAction(w http.ResponseWriter, r *http.Request) {
	_, dir, err := s.filesOf(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	gid := r.PathValue("gid")
	if !slices.ContainsFunc(s.downloadsIn(r.Context(), dir), func(d aria.Download) bool { return d.GID == gid }) {
		writeError(w, http.StatusNotFound, errors.New("no such download"))
		return
	}
	switch r.PathValue("action") {
	case "pause":
		err = s.Aria.Pause(r.Context(), gid)
	case "resume":
		err = s.Aria.Resume(r.Context(), gid)
	case "cancel":
		err = s.Aria.Remove(r.Context(), gid)
	default:
		err = errors.New("pause, resume or cancel")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAppFile(w http.ResponseWriter, r *http.Request) {
	f, dir, err := s.filesOf(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("name")
	if name != filepath.Base(name) || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, f.Ext) {
		writeError(w, http.StatusBadRequest, errors.New("no such file"))
		return
	}
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		writeError(w, http.StatusNotFound, errors.New("no such file"))
		return
	}
	_ = os.Remove(filepath.Join(dir, name+".aria2"))
	log.Printf("Files: %s deleted %s from %s", actor(r).Name, name, r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}
