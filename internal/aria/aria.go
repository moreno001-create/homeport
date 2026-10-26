// Package aria runs aria2, the downloader behind apps' Files button: ordinary links, magnet links and .torrent links,
// resuming after interruptions. Homeport starts it in the background (listening on 127.0.0.1 only, with a secret)
// and asks it how each download is going. Downloads in progress are saved, so they go on after a restart.
package aria

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Manager is the aria2 daemon.
type Manager struct {
	StateDir string // its session file goes here
	Port     int    // RPC port on 127.0.0.1 (6833 unless set)
	// Install installs aria2 when it's missing (apt-get, as its own system job); nil means it can't.
	Install func(ctx context.Context, w io.Writer) error

	mu      sync.Mutex
	secret  string
	cmd     *exec.Cmd
	url     string
	client  *http.Client
	started bool
}

func (m *Manager) port() int {
	if m.Port == 0 {
		return 6833
	}
	return m.Port
}

// Ready starts aria2 if it isn't running (installing it first when it's missing and Install is set).
func (m *Manager) Ready(ctx context.Context, w io.Writer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started && m.alive(ctx) {
		return nil
	}
	bin, err := exec.LookPath("aria2c")
	if err != nil {
		if m.Install == nil {
			return errors.New("aria2 isn't installed (it downloads files, magnet and torrent links)")
		}
		fmt.Fprintln(w, "Installing aria2 (the downloader)…")
		if err := m.Install(ctx, w); err != nil {
			return fmt.Errorf("couldn't install aria2: %w", err)
		}
		if bin, err = exec.LookPath("aria2c"); err != nil {
			return errors.New("aria2 was installed but can't be found")
		}
	}
	if m.secret == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		m.secret = hex.EncodeToString(b)
	}
	if err := os.MkdirAll(m.StateDir, 0o700); err != nil {
		return err
	}
	session := filepath.Join(m.StateDir, "aria2.session")
	if _, err := os.Stat(session); err != nil {
		_ = os.WriteFile(session, nil, 0o600)
	}
	args := []string{
		"--enable-rpc", "--rpc-listen-all=false", "--rpc-listen-port=" + strconv.Itoa(m.port()), "--rpc-secret=" + m.secret,
		"--input-file=" + session, "--save-session=" + session, "--save-session-interval=30",
		"--continue=true", "--max-concurrent-downloads=3", "--follow-torrent=mem", "--follow-metalink=mem", "--seed-time=0",
		"--bt-save-metadata=false", "--file-allocation=none", "--auto-file-renaming=false", "--allow-overwrite=false",
		"--summary-interval=0", "--console-log-level=warn",
	}
	// Files readable by the apps whatever account they run as (Homeport's own default keeps files private).
	cmd := exec.Command("sh", append([]string{"-c", `umask 022; exec "$0" "$@"`, bin}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("couldn't start aria2: %w", err)
	}
	m.cmd, m.started = cmd, true
	m.url = "http://127.0.0.1:" + strconv.Itoa(m.port()) + "/jsonrpc"
	m.client = &http.Client{Timeout: 10 * time.Second}
	go func() { _ = cmd.Wait() }()
	for i := 0; i < 50; i++ {
		if m.alive(ctx) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("aria2 started but doesn't answer")
}

// Running says whether aria2 has been started here (without starting it).
func (m *Manager) Running(ctx context.Context) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.started && m.alive(ctx)
}

func (m *Manager) alive(ctx context.Context) bool {
	var v map[string]any
	return m.callLocked(ctx, "aria2.getVersion", &v) == nil
}

func (m *Manager) call(ctx context.Context, method string, out any, params ...any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.started {
		return errors.New("aria2 isn't running")
	}
	return m.callLocked(ctx, method, out, params...)
}

func (m *Manager) callLocked(ctx context.Context, method string, out any, params ...any) error {
	if m.client == nil {
		return errors.New("aria2 isn't running")
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "hp", "method": method,
		"params": append([]any{"token:" + m.secret}, params...)})
	req, err := http.NewRequestWithContext(ctx, "POST", m.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&r); err != nil {
		return err
	}
	if r.Error != nil {
		return errors.New("aria2: " + r.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// Add starts downloading uri (a link, a magnet link or a .torrent link) into dir. It returns aria2's id for it.
func (m *Manager) Add(ctx context.Context, uri, dir string) (string, error) {
	var gid string
	err := m.call(ctx, "aria2.addUri", &gid, []string{uri}, map[string]string{"dir": dir})
	return gid, err
}

// Pause, Resume and Remove act on one download.
func (m *Manager) Pause(ctx context.Context, gid string) error {
	return m.call(ctx, "aria2.pause", nil, gid)
}
func (m *Manager) Resume(ctx context.Context, gid string) error {
	return m.call(ctx, "aria2.unpause", nil, gid)
}
func (m *Manager) Remove(ctx context.Context, gid string) error {
	_ = m.call(ctx, "aria2.forceRemove", nil, gid)
	_ = m.call(ctx, "aria2.removeDownloadResult", nil, gid) // a finished or stopped one: forget it
	return nil
}

// HasSession says whether downloads were left going last time (so aria2 should start to go on with them).
func (m *Manager) HasSession() bool {
	fi, err := os.Stat(filepath.Join(m.StateDir, "aria2.session"))
	return err == nil && fi.Size() > 0
}

// Download is how one download is going.
type Download struct {
	GID      string `json:"gid"`
	Name     string `json:"name"`
	Dir      string `json:"dir"`
	Status   string `json:"status"` // active, waiting, paused, error, complete, removed
	Total    int64  `json:"total"`
	Done     int64  `json:"done"`
	Speed    int64  `json:"speed"`    // bytes a second
	Peers    int    `json:"peers"`    // connections
	Seeders  int    `json:"seeders"`  // for torrents
	Torrent  bool   `json:"torrent"`  // a magnet or .torrent download
	Metadata bool   `json:"metadata"` // only fetching a magnet link's details so far
	Error    string `json:"error,omitempty"`
}

type rawStatus struct {
	GID             string `json:"gid"`
	Status          string `json:"status"`
	Dir             string `json:"dir"`
	TotalLength     string `json:"totalLength"`
	CompletedLength string `json:"completedLength"`
	DownloadSpeed   string `json:"downloadSpeed"`
	Connections     string `json:"connections"`
	NumSeeders      string `json:"numSeeders"`
	ErrorMessage    string `json:"errorMessage"`
	Files           []struct {
		Path string `json:"path"`
		URIs []struct {
			URI string `json:"uri"`
		} `json:"uris"`
	} `json:"files"`
	Bittorrent *struct {
		Info *struct {
			Name string `json:"name"`
		} `json:"info"`
	} `json:"bittorrent"`
}

// List is every download aria2 knows about: going, waiting and finished (until removed).
func (m *Manager) List(ctx context.Context) ([]Download, error) {
	var all []rawStatus
	for _, q := range []struct {
		method string
		params []any
	}{{"aria2.tellActive", nil}, {"aria2.tellWaiting", []any{0, 100}}, {"aria2.tellStopped", []any{0, 100}}} {
		var part []rawStatus
		if err := m.call(ctx, q.method, &part, q.params...); err != nil {
			return nil, err
		}
		all = append(all, part...)
	}
	out := make([]Download, 0, len(all))
	for _, r := range all {
		d := convert(r)
		if d.Status == "complete" && (d.Metadata || hasSuffix(d.Name, ".torrent", ".meta4", ".metalink")) {
			continue // a magnet link's details, a .torrent or a mirror list: the download itself follows as its own entry
		}
		out = append(out, d)
	}
	return out, nil
}

func convert(r rawStatus) Download {
	n := func(s string) int64 { v, _ := strconv.ParseInt(s, 10, 64); return v }
	d := Download{GID: r.GID, Status: r.Status, Dir: r.Dir, Total: n(r.TotalLength), Done: n(r.CompletedLength),
		Speed: n(r.DownloadSpeed), Peers: int(n(r.Connections)), Seeders: int(n(r.NumSeeders)), Error: r.ErrorMessage}
	if r.Bittorrent != nil {
		d.Torrent = true
		if r.Bittorrent.Info != nil {
			d.Name = r.Bittorrent.Info.Name
		}
	}
	if len(r.Files) > 0 {
		p := r.Files[0].Path
		switch {
		case strings.HasPrefix(p, "[METADATA]"):
			d.Metadata, d.Torrent = true, true
			if d.Name == "" {
				d.Name = "a magnet link"
			}
		case p != "" && d.Name == "":
			d.Name = filepath.Base(p)
		case d.Name == "" && len(r.Files[0].URIs) > 0:
			d.Name = filepath.Base(r.Files[0].URIs[0].URI)
		}
	}
	return d
}

func hasSuffix(s string, ends ...string) bool {
	for _, e := range ends {
		if strings.HasSuffix(strings.ToLower(s), e) {
			return true
		}
	}
	return false
}
