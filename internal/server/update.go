package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// MARK: Updating Homeport
//
// The Overview page checks GitHub for newer releases and updates Homeport with one click: it downloads that release's
// installer and runs it as its own system job (systemd-run), outside Homeport, since the installer restarts Homeport
// part way through. The page follows the installer's log until Homeport is back with the new version.

// Repo is where Homeport's releases are (GitHub).
const Repo = "moreno001-create/homeport"

// GitHubAPI and GitHubDownloads are GitHub's addresses (tests point them elsewhere).
var (
	GitHubAPI       = "https://api.github.com"
	GitHubDownloads = "https://github.com"
	installLog      = "/var/log/homeport-install.log"
)

func (s *Server) updateRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/update", guard(s.updateCheck))
	mux.HandleFunc("POST /api/update", guard(s.updateStart))
	mux.HandleFunc("GET /api/update/log", guard(s.updateLog))
}

type release struct {
	Tag        string    `json:"tag"`
	Name       string    `json:"name"`
	Notes      string    `json:"notes"`
	Prerelease bool      `json:"prerelease"`
	Published  time.Time `json:"published"`
}

var releases = struct {
	sync.Mutex
	list []release
	at   time.Time
}{}

// Releases are Homeport's recent releases, newest first (asked again every 10 minutes at most, or now when fresh,
// though not more than every 15 seconds: GitHub allows 60 questions an hour).
func Releases(ctx context.Context, fresh ...bool) ([]release, error) {
	releases.Lock()
	defer releases.Unlock()
	maxAge := 10 * time.Minute
	if len(fresh) > 0 && fresh[0] {
		maxAge = 15 * time.Second
	}
	if releases.list != nil && time.Since(releases.at) < maxAge {
		return releases.list, nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", GitHubAPI+"/repos/"+Repo+"/releases?per_page=15", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Homeport")
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach GitHub: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub answered %d", res.StatusCode)
	}
	var raw []struct {
		TagName     string    `json:"tag_name"`
		Name        string    `json:"name"`
		Body        string    `json:"body"`
		Prerelease  bool      `json:"prerelease"`
		Draft       bool      `json:"draft"`
		PublishedAt time.Time `json:"published_at"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&raw); err != nil {
		return nil, err
	}
	var out []release
	for _, r := range raw {
		if r.Draft || !versionTag.MatchString(r.TagName) {
			continue
		}
		out = append(out, release{Tag: r.TagName, Name: r.Name, Notes: r.Body, Prerelease: r.Prerelease, Published: r.PublishedAt})
	}
	releases.list, releases.at = out, time.Now()
	return out, nil
}

var versionTag = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)

// canUpdate says whether this server can update itself from the admin panel (Linux under systemd, as the installer
// sets it up), and why not.
func canUpdate() (bool, string) {
	if os.Getenv("INVOCATION_ID") == "" {
		return false, "Homeport isn't running as a system service here"
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false, "this system has no systemd-run"
	}
	return true, ""
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	ok, why := canUpdate()
	out := map[string]any{"current": s.Version, "canUpdate": ok, "why": why,
		"command": "curl -fsSL https://moreno001-create.github.io/homeport/install.sh | sudo bash"}
	if s.platformName() == "macos" {
		out["command"] = "curl -fsSL https://moreno001-create.github.io/homeport/install-mac.sh | bash"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	list, err := Releases(ctx, r.URL.Query().Get("fresh") == "1")
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	var latest, test *release
	for i := range list {
		if !list[i].Prerelease && latest == nil {
			latest = &list[i]
		}
		if list[i].Prerelease && test == nil && (latest == nil || list[i].Published.After(latest.Published)) {
			test = &list[i]
		}
	}
	out["latest"], out["test"] = latest, test
	writeJSON(w, http.StatusOK, out)
}

// updateStart runs a release's installer (version: a tag, "" for the latest) as its own system job.
func (s *Server) updateStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version string `json:"version"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if ok, why := canUpdate(); !ok {
		writeError(w, http.StatusBadRequest, errors.New(why+": run the installer command on the server instead"))
		return
	}
	if in.Version != "" && !versionTag.MatchString(in.Version) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%q isn't a release", in.Version))
		return
	}
	if busy := s.jobs.busy(); len(busy) > 0 {
		writeError(w, http.StatusConflict, fmt.Errorf("Homeport is busy (%s): update once that's finished", strings.Join(busy, ", ")))
		return
	}
	src := GitHubDownloads + "/" + Repo + "/releases/latest/download/install.sh"
	if in.Version != "" {
		src = GitHubDownloads + "/" + Repo + "/releases/download/" + in.Version + "/install.sh"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	script, err := download(ctx, src)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if !strings.HasPrefix(script, "#!/usr/bin/env bash") || !strings.Contains(script, "Homeport installer") {
		writeError(w, http.StatusBadGateway, errors.New("that didn't look like Homeport's installer"))
		return
	}
	dir := filepath.Join(s.stateDir(), "update")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	path := filepath.Join(dir, "install.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	args := []string{"--unit", fmt.Sprintf("homeport-update-%d", time.Now().Unix()), "--collect", "--quiet",
		"--property=Type=exec"}
	if in.Version != "" {
		args = append(args, "--setenv=HOMEPORT_VERSION="+in.Version)
	}
	args = append(args, "/bin/bash", path)
	if out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("couldn't start the installer: %v %s", err, strings.TrimSpace(string(out))))
		return
	}
	log.Printf("Update: %s started the installer for %s", actor(r).Name, cmpOr(in.Version, "the latest release"))
	writeJSON(w, http.StatusAccepted, map[string]string{"from": s.Version, "to": cmpOr(in.Version, "latest")})
}

func download(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Homeport")
	res, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("couldn't download the installer: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("couldn't download the installer (%s answered %d)", url, res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	return string(b), err
}

// updateLog is the installer log's last lines (its progress).
func (s *Server) updateLog(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(installLog)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"log": "", "version": s.Version})
		return
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > 25 {
		lines = lines[len(lines)-25:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"log": strings.Join(lines, "\n"), "version": s.Version})
}
