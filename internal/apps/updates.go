package apps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"homeport/internal/state"
)

// MARK: App updates
//
// An app has an update when the registry's image for a tag it uses (nextcloud:31, ghcr.io/x/y:latest) is no longer
// the one it runs: Homeport asks the registry for the tag's digest (a HEAD request, which doesn't count against
// Docker Hub's download limits) and compares it with the digests of the image here.

// ImageCheck is one of an app's images.
type ImageCheck struct {
	Image string `json:"image"`
	Newer bool   `json:"newer"`
	Error string `json:"error,omitempty"`
}

// AppUpdate is whether an app has a newer version waiting.
type AppUpdate struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Available bool         `json:"available"`
	Images    []ImageCheck `json:"images"`
	Error     string       `json:"error,omitempty"`
}

// RegistryClient talks to image registries (tests replace it).
var RegistryClient = &http.Client{Timeout: 20 * time.Second}

// CheckUpdates looks for newer versions of every app's images (a few apps at a time).
func (m *Manager) CheckUpdates(ctx context.Context) []AppUpdate {
	var list []state.App
	m.Store.Read(func(d *state.Data) { list = d.Apps() })
	out := make([]AppUpdate, len(list))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, a := range list {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = m.checkApp(ctx, a)
		}()
	}
	wg.Wait()
	return out
}

func (m *Manager) checkApp(ctx context.Context, a state.App) AppUpdate {
	u := AppUpdate{ID: a.ID, Name: a.Name, Images: []ImageCheck{}}
	txt, err := m.Runner.Output(ctx, m.dir(a.ID), "docker", "compose", "-p", project(a.ID), "-f", "compose.yaml", "config", "--images")
	if err != nil {
		u.Error = "couldn't read its images: " + strings.TrimSpace(txt)
		return u
	}
	seen := map[string]bool{}
	for _, img := range strings.Fields(txt) {
		if seen[img] {
			continue
		}
		seen[img] = true
		c := ImageCheck{Image: img}
		if strings.Contains(img, "@sha256:") {
			u.Images = append(u.Images, c) // pinned to one version: nothing newer to get
			continue
		}
		local, _ := m.Runner.Output(ctx, "", "docker", "image", "inspect", "--format", "{{json .RepoDigests}}", img)
		var digests []string
		_ = json.Unmarshal([]byte(strings.TrimSpace(local)), &digests)
		remote, err := RemoteDigest(ctx, img)
		switch {
		case err != nil:
			c.Error = err.Error()
		case len(digests) == 0:
			c.Newer = true // not downloaded yet (or built here): an update fetches it
		default:
			c.Newer = true
			for _, d := range digests {
				if strings.HasSuffix(d, "@"+remote) {
					c.Newer = false
				}
			}
		}
		u.Available = u.Available || c.Newer
		u.Images = append(u.Images, c)
	}
	return u
}

// imageRef splits an image name into its registry, repository and tag (nginx → registry-1.docker.io,
// library/nginx, latest).
func imageRef(img string) (host, repo, tag string) {
	name, tag := img, "latest"
	if i := strings.LastIndex(img, ":"); i > strings.LastIndex(img, "/") {
		name, tag = img[:i], img[i+1:]
	}
	host = "registry-1.docker.io"
	if first, rest, ok := strings.Cut(name, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		host, name = first, rest
		if host == "docker.io" || host == "index.docker.io" {
			host = "registry-1.docker.io"
		}
	}
	if host == "registry-1.docker.io" && !strings.Contains(name, "/") {
		name = "library/" + name
	}
	return host, name, tag
}

var bearer = regexp.MustCompile(`(\w+)="([^"]*)"`)

// RemoteDigest is the digest the registry has for an image's tag now.
func RemoteDigest(ctx context.Context, img string) (string, error) {
	host, repo, tag := imageRef(img)
	u := "https://" + host + "/v2/" + repo + "/manifests/" + tag
	head := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, "HEAD", u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", strings.Join([]string{"application/vnd.oci.image.index.v1+json",
			"application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.docker.distribution.manifest.v2+json",
			"application/vnd.oci.image.manifest.v1+json"}, ", "))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return RegistryClient.Do(req)
	}
	res, err := head("")
	if err != nil {
		return "", fmt.Errorf("couldn't reach %s", host)
	}
	res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		// Anonymous access still needs a token, from where the registry says.
		params := map[string]string{}
		for _, m := range bearer.FindAllStringSubmatch(res.Header.Get("Www-Authenticate"), -1) {
			params[strings.ToLower(m[1])] = m[2]
		}
		if params["realm"] == "" {
			return "", fmt.Errorf("%s wants a sign-in", host)
		}
		q := url.Values{}
		if params["service"] != "" {
			q.Set("service", params["service"])
		}
		q.Set("scope", cmpOr(params["scope"], "repository:"+repo+":pull"))
		req, err := http.NewRequestWithContext(ctx, "GET", params["realm"]+"?"+q.Encode(), nil)
		if err != nil {
			return "", err
		}
		tr, err := RegistryClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("couldn't reach %s", params["realm"])
		}
		var t struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}
		_ = json.NewDecoder(tr.Body).Decode(&t)
		tr.Body.Close()
		if res, err = head(cmpOr(t.Token, t.AccessToken)); err != nil {
			return "", fmt.Errorf("couldn't reach %s", host)
		}
		res.Body.Close()
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered %d for %s", host, res.StatusCode, img)
	}
	d := res.Header.Get("Docker-Content-Digest")
	if d == "" {
		return "", errors.New(host + " didn't say which version it has")
	}
	return d, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
