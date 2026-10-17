package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"homeport/internal/state"
	"homeport/internal/weather"
)

func TestMoreWeatherPlacesAndClocks(t *testing.T) {
	r := newRig(t)
	zips := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, `{"places":[{"place name":"Town","latitude":"34.1","longitude":"-118.1","state abbreviation":"CA"}]}`)
	}))
	defer zips.Close()
	old := weather.ZipURL
	weather.ZipURL = zips.URL + "/"
	defer func() { weather.ZipURL = old }()

	b := r.browser()
	b.do("POST", "https://home.test/login", url.Values{"name": {"bob"}, "password": {"battery staple"}, "return": {"/"}}, nil)
	put := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "https://home.test"+path, strings.NewReader(body))
		req.Header.Set("X-Homeport", "1")
		req.Header.Set("Content-Type", "application/json")
		for _, c := range b.cookies {
			req.AddCookie(&http.Cookie{Name: cookieName, Value: c})
		}
		w := httptest.NewRecorder()
		r.h.ServeHTTP(w, req)
		return w
	}
	for i, want := range []int{http.StatusNoContent, http.StatusNoContent, http.StatusBadRequest} {
		if w := put("/api/me/weather", `{"add":"90210"}`); w.Code != want {
			t.Fatalf("place %d: %d %s", i+2, w.Code, w.Body)
		}
	}
	if w := put("/api/me/weather", `{"remove":5}`); w.Code != http.StatusBadRequest {
		t.Fatalf("removing a place that isn't there: %d", w.Code)
	}
	if w := put("/api/me/weather", `{"remove":0}`); w.Code != http.StatusNoContent || len(r.srv.myPlaces("bob")) != 1 {
		t.Fatalf("remove: %d, %v", w.Code, r.srv.myPlaces("bob"))
	}

	w := b.do("GET", "https://home.test/api/me/clocks", nil, nil)
	if !strings.Contains(w.Body.String(), "Europe/London") {
		t.Fatalf("the usual clocks first: %s", w.Body)
	}
	if w := put("/api/me/clocks", `{"zones":["Asia/Kolkata","Asia/Kolkata","Australia/Sydney"]}`); w.Code != http.StatusOK {
		t.Fatalf("set clocks: %d %s", w.Code, w.Body)
	}
	var x struct{ Zones []string }
	_ = json.Unmarshal(b.do("GET", "https://home.test/api/me/clocks", nil, nil).Body.Bytes(), &x)
	if strings.Join(x.Zones, ",") != "Asia/Kolkata,Australia/Sydney" {
		t.Fatalf("kept once each: %v", x.Zones)
	}
	if w := put("/api/me/clocks", `{"zones":["Mars/Olympus"]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("a made-up zone: %d", w.Code)
	}
	if w := put("/api/me/clocks", `{"zones":[]}`); w.Code != http.StatusOK || !strings.Contains(b.do("GET", "https://home.test/api/me/clocks", nil, nil).Body.String(), `"zones":[]`) {
		t.Fatalf("no clocks is a choice too: %d", w.Code)
	}
	// …and stays one after a restart (the state file read again).
	again, err := state.Open(r.srv.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	again.Read(func(d *state.Data) {
		if u, _ := d.User("bob"); u.Clocks == nil {
			t.Error("an empty world clock came back as the usual one")
		}
	})
}
