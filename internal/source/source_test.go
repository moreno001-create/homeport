package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseURL(t *testing.T) {
	for in, want := range map[string]Repo{
		"https://github.com/owner/app":                        {Owner: "owner", Name: "app"},
		"github.com/owner/app.git":                            {Owner: "owner", Name: "app"},
		"https://github.com/owner/app/tree/v2/deploy":         {Owner: "owner", Name: "app", Ref: "v2", Dir: "deploy"},
		"https://github.com/o/a/blob/main/docker/compose.yml": {Owner: "o", Name: "a", Ref: "main", Dir: "docker"},
		"https://github.com/o/a/blob/main/compose.yml":        {Owner: "o", Name: "a", Ref: "main"},
	} {
		got, err := ParseURL(in)
		if err != nil || got != want {
			t.Errorf("ParseURL(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"https://gitlab.com/o/a", "https://github.com/onlyowner", "https://github.com/o/a b", "file:///etc/passwd"} {
		if _, err := ParseURL(bad); err == nil {
			t.Errorf("ParseURL(%q) should fail", bad)
		}
	}
}

const sha = "0123456789abcdef0123456789abcdef01234567"

func tarball(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: "app-" + sha + "/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func fakeGitHub(t *testing.T, files map[string]string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/owner/app/commits/HEAD" && r.Header.Get("Accept") == "application/vnd.github.sha":
			w.Write([]byte(sha))
		case r.URL.Path == "/owner/app/tar.gz/"+sha:
			w.Write(tarball(t, files))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	GitHubAPI, GitHubCodeload = srv.URL, srv.URL
}

func TestFetch(t *testing.T) {
	fakeGitHub(t, map[string]string{
		"README.md":               "# App",
		"docker-compose.yml":      "services: {}",
		"docker/compose.prod.yml": "services: {}",
		".env.example":            "PORT=80",
		"src/main.go":             "package main",
		"deep/a/b/compose.yaml":   "too deep",
		"big/README":              strings.Repeat("x", maxFile+1),
	})
	f, err := Fetch(context.Background(), Repo{Owner: "owner", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Commit != sha || f.ComposeName != "docker-compose.yml" || f.Manifest != "" {
		t.Errorf("got commit %s, compose %q, manifest %q", f.Commit, f.ComposeName, f.Manifest)
	}
	for _, want := range []string{"README.md", ".env.example", "docker/compose.prod.yml"} {
		if _, ok := f.Extra[want]; !ok {
			t.Errorf("missing %s in %v", want, f.Extra)
		}
	}
	for _, not := range []string{"src/main.go", "deep/a/b/compose.yaml", "big/README"} {
		if _, ok := f.Extra[not]; ok {
			t.Errorf("%s shouldn't be read", not)
		}
	}
}

func TestFetchManifestAndFolder(t *testing.T) {
	fakeGitHub(t, map[string]string{"homeport/homeport.yaml": "id: app", "homeport/compose.yaml": "services: {}", "compose.yaml": "root one"})
	f, err := Fetch(context.Background(), Repo{Owner: "owner", Name: "app", Dir: "homeport"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Manifest != "id: app" || f.Compose != "services: {}" {
		t.Errorf("should read only the folder: manifest %q compose %q", f.Manifest, f.Compose)
	}
	fakeGitHub(t, map[string]string{"README.md": "# nothing to run"})
	if _, err := Fetch(context.Background(), Repo{Owner: "owner", Name: "app"}); err == nil || !strings.Contains(err.Error(), "no homeport.yaml or Docker Compose") {
		t.Errorf("a repository without a compose file: %v", err)
	}
	if _, err := Fetch(context.Background(), Repo{Owner: "owner", Name: "missing"}); err == nil || !strings.Contains(err.Error(), "no public repository") {
		t.Errorf("a missing repository: %v", err)
	}
}
