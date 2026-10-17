package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUpdateCheck(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/"+Repo+"/releases" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "v0.7.12-rc1", "name": "test", "prerelease": true, "published_at": time.Now()},
			{"tag_name": "v0.7.11", "name": "Homeport 0.7.11", "body": "- Home names", "published_at": time.Now().Add(-time.Hour)},
			{"tag_name": "nightly", "published_at": time.Now()},
			{"tag_name": "v0.7.10", "draft": true},
		})
	}))
	defer gh.Close()
	old := GitHubAPI
	GitHubAPI = gh.URL
	releases.list = nil
	t.Cleanup(func() { GitHubAPI = old; releases.list = nil })

	r := newRig(t)
	r.srv.Version = "v0.7.11-rc8"
	w := r.local(t, "GET", "/api/update", "")
	var got struct {
		Current   string
		CanUpdate bool
		Latest    *release
		Test      *release
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(w.Body.String())
	}
	if got.Current != "v0.7.11-rc8" || got.Latest == nil || got.Latest.Tag != "v0.7.11" || got.Latest.Notes != "- Home names" ||
		got.Test == nil || got.Test.Tag != "v0.7.12-rc1" {
		t.Errorf("got %+v latest %+v test %+v", got, got.Latest, got.Test)
	}
	// Not running under systemd (as in tests): no one-click update, and starting one is refused.
	t.Setenv("INVOCATION_ID", "")
	if got.CanUpdate {
		t.Error("tests don't run under systemd")
	}
	if w := r.local(t, "POST", "/api/update", `{"version":"v0.7.11"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "installer command") {
		t.Errorf("update outside systemd: %d %s", w.Code, w.Body.String())
	}
	if w := r.local(t, "POST", "/api/update", `{"version":"v0.7.11; rm -rf /"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a bad version: %d", w.Code)
	}
}
