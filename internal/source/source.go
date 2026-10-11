// Package source reads an app from a GitHub repository: the files that say how to run it (a homeport.yaml, or a
// Docker Compose file with its README and example settings), pinned to one commit. Nothing from the repository is
// written to disk or run; it's only read.
package source

import (
	"archive/tar"
	"cmp"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// GitHub's addresses (tests point them elsewhere).
var (
	GitHubAPI      = "https://api.github.com"
	GitHubCodeload = "https://codeload.github.com"
)

// Repo is a repository, optionally a folder in it and a branch or tag.
type Repo struct {
	Owner, Name string
	Ref         string // branch, tag or commit; empty: the default branch
	Dir         string // folder inside the repository holding the app
}

func (r Repo) URL() string {
	u := "https://github.com/" + r.Owner + "/" + r.Name
	if r.Ref != "" || r.Dir != "" {
		u += "/tree/" + cmp.Or(r.Ref, "HEAD")
		if r.Dir != "" {
			u += "/" + r.Dir
		}
	}
	return u
}

var namePart = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// ParseURL understands github.com/owner/repo, with https://, .git, or /tree/<branch>/<folder>.
func ParseURL(raw string) (Repo, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	bad := errors.New("give a GitHub address like https://github.com/owner/app")
	if err != nil || (u.Host != "github.com" && u.Host != "www.github.com") {
		return Repo{}, bad
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return Repo{}, bad
	}
	r := Repo{Owner: parts[0], Name: strings.TrimSuffix(parts[1], ".git")}
	if !namePart.MatchString(r.Owner) || !namePart.MatchString(r.Name) {
		return Repo{}, bad
	}
	if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob") {
		r.Ref = parts[3]
		r.Dir = strings.Join(parts[4:], "/")
		if parts[2] == "blob" { // a link to a file: its folder
			r.Dir = path.Dir(r.Dir)
			if r.Dir == "." {
				r.Dir = ""
			}
		}
	}
	if r.Ref != "" && !namePart.MatchString(r.Ref) {
		return Repo{}, errors.New("branch names with slashes aren't supported; use the branch's latest commit instead")
	}
	return r, nil
}

// Files are what was read from the repository, pinned to Commit.
type Files struct {
	Repo   Repo
	Commit string
	// Manifest is the repository's own homeport.yaml, if it has one.
	Manifest string
	// Compose is the compose file (ComposeName says which).
	Compose, ComposeName string
	// Extra are other helpful files: the README, example settings, other compose files.
	Extra map[string]string
}

var client = &http.Client{Timeout: 60 * time.Second}

// Fetch reads the app's files from the repository.
func Fetch(ctx context.Context, r Repo) (Files, error) {
	commit, err := resolve(ctx, r)
	if err != nil {
		return Files{}, err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", GitHubCodeload+"/"+r.Owner+"/"+r.Name+"/tar.gz/"+commit, nil)
	res, err := client.Do(req)
	if err != nil {
		return Files{}, fmt.Errorf("couldn't download the repository: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Files{}, fmt.Errorf("couldn't download the repository: GitHub said %s", res.Status)
	}
	f := Files{Repo: r, Commit: commit, Extra: map[string]string{}}
	if err := f.read(res.Body); err != nil {
		return Files{}, err
	}
	if f.Manifest == "" && f.Compose == "" {
		where := "the repository"
		if r.Dir != "" {
			where = r.Dir
		}
		return Files{}, fmt.Errorf("%s has no homeport.yaml or Docker Compose file (compose.yaml or docker-compose.yml)", where)
	}
	return f, nil
}

// resolve pins the branch or tag to the commit it's at now.
func resolve(ctx context.Context, r Repo) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", GitHubAPI+"/repos/"+r.Owner+"/"+r.Name+"/commits/"+cmp.Or(r.Ref, "HEAD"), nil)
	req.Header.Set("Accept", "application/vnd.github.sha")
	req.Header.Set("User-Agent", "Homeport")
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("couldn't reach GitHub: %v", err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	switch {
	case res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusUnprocessableEntity:
		return "", fmt.Errorf("GitHub has no public repository %s/%s (or no %s in it)", r.Owner, r.Name, cmp.Or(r.Ref, "default branch"))
	case res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests:
		return "", errors.New("GitHub is limiting requests from this server for now; try again in an hour")
	case res.StatusCode != http.StatusOK:
		return "", fmt.Errorf("GitHub said %s", res.Status)
	}
	sha := strings.TrimSpace(string(b))
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(sha) {
		return "", errors.New("GitHub sent back something unexpected for the commit")
	}
	return sha, nil
}

// Which files are read, by name (lowercase), and how big they may be.
var composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}

const maxFile, maxTotal = 64 << 10, 256 << 10

func wanted(name string) bool {
	n := strings.ToLower(name)
	switch {
	case n == "homeport.yaml" || n == "homeport.yml":
		return true
	case strings.HasPrefix(n, "readme"):
		return true
	case strings.HasPrefix(n, ".env") || strings.HasSuffix(n, ".env.example") || n == "example.env" || n == "env.example":
		return true
	case (strings.Contains(n, "compose") || strings.Contains(n, "docker")) && (strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml")):
		return true
	}
	return false
}

// read goes through the repository's archive and keeps the files that matter, from the app's folder and the folders
// right under it (docker/, deploy/…).
func (f *Files) read(body io.Reader) error {
	gz, err := gzip.NewReader(body)
	if err != nil {
		return fmt.Errorf("the repository download is damaged: %v", err)
	}
	tr := tar.NewReader(gz)
	total := 0
	found := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("the repository download is damaged: %v", err)
		}
		if h.Typeflag != tar.TypeReg || h.Size > maxFile {
			continue
		}
		// Archive paths start with "<repo>-<commit>/".
		p := h.Name
		if i := strings.Index(p, "/"); i >= 0 {
			p = p[i+1:]
		}
		if f.Repo.Dir != "" {
			if !strings.HasPrefix(p, f.Repo.Dir+"/") {
				continue
			}
			p = strings.TrimPrefix(p, f.Repo.Dir+"/")
		}
		if strings.Count(p, "/") > 1 || !wanted(path.Base(p)) {
			continue
		}
		if total+int(h.Size) > maxTotal {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxFile))
		if err != nil {
			return err
		}
		total += len(b)
		found[p] = string(b)
	}
	names := make([]string, 0, len(found))
	for p := range found {
		names = append(names, p)
	}
	// Top-level files first, then by name.
	sort.Slice(names, func(i, j int) bool {
		di, dj := strings.Count(names[i], "/"), strings.Count(names[j], "/")
		if di != dj {
			return di < dj
		}
		return names[i] < names[j]
	})
	for _, p := range names {
		base := strings.ToLower(path.Base(p))
		switch {
		case f.Manifest == "" && (base == "homeport.yaml" || base == "homeport.yml"):
			f.Manifest = found[p]
		case f.Compose == "" && slices.Contains(composeNames, base):
			f.Compose, f.ComposeName = found[p], p
		default:
			f.Extra[p] = found[p]
		}
	}
	return nil
}
