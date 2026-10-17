package apps

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"homeport/internal/state"
)

func TestImageRef(t *testing.T) {
	for img, want := range map[string]string{
		"nginx":                              "registry-1.docker.io library/nginx latest",
		"nextcloud:31-apache":                "registry-1.docker.io library/nextcloud 31-apache",
		"mattermost/mattermost-team-edition": "registry-1.docker.io mattermost/mattermost-team-edition latest",
		"docker.io/library/redis:7":          "registry-1.docker.io library/redis 7",
		"ghcr.io/librespeed/speedtest:5":     "ghcr.io librespeed/speedtest 5",
		"localhost:5000/me/app:dev":          "localhost:5000 me/app dev",
	} {
		h, r, tag := imageRef(img)
		if got := h + " " + r + " " + tag; got != want {
			t.Errorf("%s: got %q, want %q", img, got, want)
		}
	}
}

// registry answers like Docker Hub: no token → 401 pointing at its token service.
func registry(t *testing.T) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if r.URL.Query().Get("scope") != "repository:me/app:pull" {
				t.Errorf("scope %q", r.URL.Query().Get("scope"))
			}
			io.WriteString(w, `{"token":"t0k"}`)
		case r.Header.Get("Authorization") != "Bearer t0k":
			w.Header().Set("Www-Authenticate", `Bearer realm="`+srv.URL+`/token",service="test",scope="repository:me/app:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == "HEAD" && r.URL.Path == "/v2/me/app/manifests/1":
			w.Header().Set("Docker-Content-Digest", "sha256:new")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	old := RegistryClient
	RegistryClient = srv.Client()
	t.Cleanup(func() { RegistryClient = old })
	return srv
}

func TestRemoteDigestGetsAToken(t *testing.T) {
	srv := registry(t)
	host := strings.TrimPrefix(srv.URL, "https://")
	d, err := RemoteDigest(context.Background(), host+"/me/app:1")
	if err != nil || d != "sha256:new" {
		t.Fatalf("got %q, %v", d, err)
	}
	if _, err := RemoteDigest(context.Background(), host+"/me/app:2"); err == nil {
		t.Fatal("a missing tag should be an error")
	}
}

type imagesRunner struct{ images, digests string }

func (r *imagesRunner) Output(_ context.Context, _, name string, args ...string) (string, error) {
	cmd := strings.Join(args, " ")
	switch {
	case strings.Contains(cmd, "config --images"):
		return r.images, nil
	case strings.HasPrefix(cmd, "image inspect"):
		return r.digests, nil
	}
	return "", nil
}
func (r *imagesRunner) Stream(context.Context, string, io.Writer, string, ...string) error {
	return nil
}

func TestCheckUpdates(t *testing.T) {
	host := strings.TrimPrefix(registry(t).URL, "https://")
	store, _ := state.Open(filepath.Join(t.TempDir(), "state.json"))
	_ = store.Update(func(d *state.Data) error { d.AddApp(state.App{ID: "x", Name: "X"}); return nil })
	run := &imagesRunner{images: host + "/me/app:1\n" + host + "/me/db@sha256:pinned\n" + host + "/me/app:1\n"}
	m := &Manager{Store: store, Root: t.TempDir(), Runner: run}

	run.digests = `["` + host + `/me/app@sha256:old"]`
	got := m.CheckUpdates(context.Background())
	if len(got) != 1 || !got[0].Available || len(got[0].Images) != 2 || got[0].Images[1].Newer {
		t.Fatalf("an older image should be an update (pinned ones never): %+v", got)
	}
	run.digests = `["` + host + `/me/app@sha256:new"]`
	if got := m.CheckUpdates(context.Background()); got[0].Available {
		t.Fatalf("the newest image isn't an update: %+v", got)
	}
}
