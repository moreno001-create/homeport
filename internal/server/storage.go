package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"homeport/internal/state"
	"homeport/internal/storage"
)

// MARK: Storage
//
// The Storage page is a switchboard: every app and Docker's images, each on a drive, and a switch to put it on
// another. (The AI models stay on the system drive.) Moves copy while things keep running, stop them only for a
// last copy, switch, check, and only then delete the old copy (or go back if the check fails).

func (s *Server) storageRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/storage", guard(s.storageView))
	mux.HandleFunc("GET /api/storage/drives", guard(s.storageDrives))
	mux.HandleFunc("POST /api/storage/mount", guard(s.storageMount))
	mux.HandleFunc("PUT /api/storage/new-apps", guard(s.storageNewApps))
	mux.HandleFunc("POST /api/storage/move", guard(s.storageMove))
}

// dockerPlace is Docker's images, which can live on another drive too.
var dockerPlace = storage.Place{Name: "Docker's images", Paths: []string{"/var/lib/docker", "/var/lib/containerd"},
	Subs: []string{"docker", "containerd"}, Stop: []string{"docker.socket", "docker", "containerd"}}

// storageItem is one thing on the switchboard.
type storageItem struct {
	ID    string `json:"id"` // app:<id> or docker
	Name  string `json:"name"`
	About string `json:"about"`
	Size  int64  `json:"size"`
	Drive string `json:"drive"`           // the mount point it's on ("" when its drive is missing)
	Where string `json:"where"`           // its folder
	Fixed string `json:"fixed,omitempty"` // why it can't be moved from here
	Down  string `json:"down"`            // what stops while it's switched
}

var sizes = struct {
	sync.Mutex
	m map[string]sizeAt
}{m: map[string]sizeAt{}}

type sizeAt struct {
	n  int64
	at time.Time
}

// folderSize is a folder's size, remembered for a few minutes (a big media library takes a while to add up).
func folderSize(ctx context.Context, path string) int64 {
	sizes.Lock()
	c, ok := sizes.m[path]
	sizes.Unlock()
	if ok && time.Since(c.at) < 5*time.Minute {
		return c.n
	}
	n := storage.Size(ctx, path)
	sizes.Lock()
	sizes.m[path] = sizeAt{n, time.Now()}
	sizes.Unlock()
	return n
}

func forgetSizes() {
	sizes.Lock()
	sizes.m = map[string]sizeAt{}
	sizes.Unlock()
}

// storageItems lists what can be moved, with where each is now.
func (s *Server) storageItems(ctx context.Context, drives []storage.Drive) []storageItem {
	var list []state.App
	s.Store.Read(func(d *state.Data) { list = d.Apps() })
	items := make([]storageItem, 0, len(list)+2)
	for _, a := range list {
		about := "Its settings, database and files"
		if a.Catalog == "jellyfin" {
			about = "Its settings and the media folder: all the movies and shows"
		}
		items = append(items, storageItem{ID: "app:" + a.ID, Name: a.Name, About: about, Where: s.Apps.Dir(a.ID),
			Down: a.Name + " stops for the last copy (usually seconds)"})
	}
	linux := s.platformName() != "macos"
	docker := storageItem{ID: "docker", Name: "Docker's images", About: "The apps' downloaded programs (images and containers; not their data)",
		Down: "Every app stops for the last copy (a minute or two)"}
	if linux {
		docker.Where = strings.Join(dockerPlace.Existing().Where(), " and ")
		// Overlay storage works on any Linux drive; Docker's zfs or btrfs drivers only on their own kind.
		out, err := exec.CommandContext(ctx, "docker", "info", "-f", "{{.Driver}}").Output()
		switch driver := strings.TrimSpace(string(out)); {
		case err != nil:
			docker.Fixed = "Docker isn't running"
		case !strings.HasPrefix(driver, "overlay"):
			docker.Fixed = "Docker keeps its images with its " + driver + " driver, which only works on that kind of drive"
		}
	} else {
		docker.Fixed = "On a Mac, move them in OrbStack's (or Docker Desktop's) settings"
	}
	items = append(items, docker)

	var wg sync.WaitGroup
	for i := range items {
		it := &items[i]
		if it.Where == "" {
			continue
		}
		first := strings.Split(it.Where, " and ")[0]
		if d, ok := storage.DriveOf(drives, first); ok {
			it.Drive = d.Mount
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, w := range strings.Split(it.Where, " and ") {
				it.Size += folderSize(ctx, w)
			}
		}()
	}
	wg.Wait()
	return items
}

func (s *Server) storageView(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	drives, err := storage.Drives(ctx, s.stateDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	newApps := s.settings().NewAppsDrive
	if newApps != "" {
		newApps = strings.TrimSuffix(newApps, "/homeport/apps")
	}
	writeJSON(w, http.StatusOK, map[string]any{"drives": drives, "items": s.storageItems(ctx, drives), "newApps": newApps,
		"missing": s.Apps.Missing(), "busy": s.jobs.busy()})
}

// storageDrives is the drives apps can go on, for the install window (quick: no folder sizes).
func (s *Server) storageDrives(w http.ResponseWriter, r *http.Request) {
	drives, err := storage.Drives(r.Context(), s.stateDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	usable := []storage.Drive{}
	for _, d := range drives {
		if d.Usable && d.Lasting && d.Mounted {
			usable = append(usable, d)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"drives": usable, "newApps": strings.TrimSuffix(s.settings().NewAppsDrive, "/homeport/apps")})
}

// appsPlace is where apps' folders go on a drive (by its mount point): "home" for the system drive.
func (s *Server) appsPlace(ctx context.Context, mount string) (string, error) {
	drives, err := storage.Drives(ctx, s.stateDir())
	if err != nil {
		return "", err
	}
	d, err := usableDrive(drives, mount)
	if err != nil {
		return "", err
	}
	if d.System {
		return "home", nil
	}
	base := filepath.Join(d.Mount, "homeport", "apps")
	return base, os.MkdirAll(base, 0o700)
}

// storageMount mounts a drive and keeps it mounted after restarts (/etc/fstab).
func (s *Server) storageMount(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Device string `json:"device"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	drives, err := storage.Drives(ctx, s.stateDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	i := slices.IndexFunc(drives, func(d storage.Drive) bool { return d.Device == in.Device })
	if i < 0 {
		writeError(w, http.StatusNotFound, errors.New("there's no such drive"))
		return
	}
	mount, err := storage.KeepMounted(ctx, drives[i])
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	log.Printf("Storage: %s keeps %s mounted at %s", actor(r).Name, in.Device, mount)
	writeJSON(w, http.StatusOK, map[string]string{"mount": mount})
}

// usableDrive finds a mounted drive things may be moved to ("" is the system drive).
func usableDrive(drives []storage.Drive, mount string) (storage.Drive, error) {
	for _, d := range drives {
		if (mount == "" && d.System) || (mount != "" && d.Mount == mount) {
			if !d.Usable {
				return d, fmt.Errorf("%s can't hold apps: %s", d.Mount, d.Why)
			}
			if !d.Lasting {
				return d, fmt.Errorf("%s is only mounted until the next restart: choose \"Keep it mounted\" first", d.Mount)
			}
			return d, nil
		}
	}
	return storage.Drive{}, fmt.Errorf("there's no drive mounted at %s", mount)
}

func (s *Server) storageNewApps(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Drive string `json:"drive"` // its mount point; "" or the system drive's for Homeport's own folder
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	base, err := s.appsPlace(r.Context(), in.Drive)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if base == "home" {
		base = ""
	}
	if err := s.Store.Update(func(st *state.Data) error { st.Settings().NewAppsDrive = base; return nil }); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// moveProgress is a move's progress, for the switchboard.
type moveProgress struct {
	Item  string `json:"item"`
	Name  string `json:"name"`
	Done  int64  `json:"done"`
	Total int64  `json:"total"`
	Step  int    `json:"step"` // which of the moves (1…)
	Of    int    `json:"of"`
}

// storageMove moves the chosen things to their chosen drives, one after another, as a job.
func (s *Server) storageMove(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Moves []struct {
			Item  string `json:"item"`
			Drive string `json:"drive"` // mount point
		} `json:"moves"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(in.Moves) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("choose something to move first"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	drives, err := storage.Drives(ctx, s.stateDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	items := s.storageItems(ctx, drives)
	type plan struct {
		item  storageItem
		drive storage.Drive
	}
	var plans []plan
	need := map[string]int64{}
	for _, m := range in.Moves {
		i := slices.IndexFunc(items, func(it storageItem) bool { return it.ID == m.Item })
		if i < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("there's nothing called %q to move", m.Item))
			return
		}
		it := items[i]
		if it.Fixed != "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("%s: %s", it.Name, it.Fixed))
			return
		}
		d, err := usableDrive(drives, m.Drive)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if d.Mount == it.Drive {
			continue
		}
		need[d.Mount] += it.Size
		plans = append(plans, plan{it, d})
	}
	for _, d := range drives {
		// Room for it all, and a little to spare.
		if n := need[d.Mount]; n > 0 && n+n/20+(1<<30) > d.Free {
			writeError(w, http.StatusBadRequest, fmt.Errorf("%s has %s free, and moving these there needs about %s", d.Mount, gib(d.Free), gib(n+n/20+(1<<30))))
			return
		}
	}
	if len(plans) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("everything chosen is already on that drive"))
		return
	}
	// Docker last: it stops every app for a moment, so the apps' own moves come first.
	sort.SliceStable(plans, func(i, j int) bool { return plans[j].item.ID == "docker" && plans[i].item.ID != "docker" })
	names := make([]string, len(plans))
	for i, p := range plans {
		names[i] = p.item.Name
	}
	log.Printf("Storage: %s moves %s", actor(r).Name, strings.Join(names, ", "))
	j := s.jobs.start("move "+strings.Join(names, ", "), func(ctx context.Context, j *job) error {
		defer forgetSizes()
		var failed []string
		for n, p := range plans {
			prog := func(done, total int64) {
				j.setResult(moveProgress{Item: p.item.ID, Name: p.item.Name, Done: done, Total: total, Step: n + 1, Of: len(plans)})
			}
			prog(0, p.item.Size)
			fmt.Fprintf(j, "\n== %s → %s ==\n", p.item.Name, cmpOr(p.drive.Label, p.drive.Mount))
			if err := s.moveOne(ctx, p.item, p.drive, prog, j); err != nil {
				fmt.Fprintf(j, "%s wasn't moved: %v\n", p.item.Name, err)
				failed = append(failed, p.item.Name)
			}
		}
		j.setResult(moveProgress{Step: len(plans), Of: len(plans)})
		if len(failed) > 0 {
			return fmt.Errorf("not moved: %s (the log says why; they're still where they were)", strings.Join(failed, ", "))
		}
		return nil
	})
	writeJSON(w, http.StatusAccepted, j.view())
}

func (s *Server) moveOne(ctx context.Context, it storageItem, d storage.Drive, prog storage.Progress, out *job) error {
	base := "" // the system drive
	if !d.System {
		base = d.Mount
	}
	switch {
	case strings.HasPrefix(it.ID, "app:"):
		if base != "" {
			base = filepath.Join(base, "homeport", "apps")
		}
		return s.Apps.Move(ctx, strings.TrimPrefix(it.ID, "app:"), base, prog, out)
	case it.ID == "docker":
		before := runningContainers(ctx)
		return storage.MovePlace(ctx, dockerPlace, base, it.Size, prog, func(ctx context.Context) error {
			return storage.Retry(ctx, 3*time.Minute, func() error {
				after := runningContainers(ctx)
				var missing []string
				for _, c := range before {
					if !slices.Contains(after, c) {
						missing = append(missing, c)
					}
				}
				if after == nil || len(missing) > 0 {
					return fmt.Errorf("not running again yet: %s", strings.Join(missing, ", "))
				}
				return nil
			})
		}, out)
	}
	return fmt.Errorf("there's no %q to move", it.ID)
}

// runningContainers are the names of the containers running now (nil when Docker doesn't answer).
func runningContainers(ctx context.Context) []string {
	b, err := exec.CommandContext(ctx, "docker", "ps", "--format", "{{.Names}}").Output()
	if err != nil {
		return nil
	}
	out := []string{}
	for _, n := range strings.Fields(string(b)) {
		out = append(out, n)
	}
	return out
}
