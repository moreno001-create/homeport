package weather

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeServices(t *testing.T) *[]string {
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path+"?"+r.URL.RawQuery)
		switch {
		case r.URL.Path == "/forecast":
			io.WriteString(w, `{"current":{"temperature_2m":71.6,"relative_humidity_2m":40,"apparent_temperature":70.1,"weather_code":2,"wind_speed_10m":5.4,"is_day":1},
"daily":{"time":["2026-10-17","2026-10-18"],"weather_code":[2,61],"temperature_2m_max":[80.2,66],"temperature_2m_min":[60.1,55],"precipitation_probability_max":[0,null]}}`)
		case r.URL.Path == "/zip/90210":
			io.WriteString(w, `{"places":[{"place name":"Beverly Hills","latitude":"34.0901","longitude":"-118.4065","state abbreviation":"CA"}]}`)
		case strings.HasPrefix(r.URL.Path, "/zip/"):
			w.WriteHeader(404)
			io.WriteString(w, `{}`)
		case r.URL.Path == "/search":
			io.WriteString(w, `{"results":[{"name":"Pasadena","latitude":29.69,"longitude":-95.2,"country":"United States","country_code":"US","admin1":"Texas"},
{"name":"Pasadena","latitude":34.15,"longitude":-118.14,"country":"United States","country_code":"US","admin1":"California"}]}`)
		case r.URL.Path == "/reverse":
			io.WriteString(w, `{"name":"Los Angeles","address":{"city":"Los Angeles","state":"California","country_code":"us"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	ForecastURL, SearchURL, ZipURL, ReverseURL = srv.URL+"/forecast", srv.URL+"/search", srv.URL+"/zip/", srv.URL+"/reverse"
	return &asked
}

func TestGet(t *testing.T) {
	asked := fakeServices(t)
	f, err := Get(context.Background(), Place{Name: "Home", Lat: 34.0512345, Lon: -118.2412345}, "f")
	if err != nil {
		t.Fatal(err)
	}
	if f.Now.Temp != 71.6 || f.Now.Text != "Partly cloudy" || f.Now.Icon != "⛅" || len(f.Days) != 2 || f.Days[1].Text != "Rain" || f.Days[1].Rain != 0 {
		t.Fatalf("%+v", f)
	}
	if !strings.Contains((*asked)[0], "latitude=34.05") || !strings.Contains((*asked)[0], "temperature_unit=fahrenheit") {
		t.Fatalf("asked %v", *asked)
	}
	if _, err := Get(context.Background(), Place{Lat: 34.05, Lon: -118.24}, "f"); err != nil || len(*asked) != 1 {
		t.Fatalf("the same place within 10 minutes comes from the cache: %v", *asked)
	}
}

func TestFind(t *testing.T) {
	fakeServices(t)
	p, err := Find(context.Background(), "90210")
	if err != nil || p.Name != "Beverly Hills, CA" || !p.US || p.Lat != 34.0901 {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := Find(context.Background(), "00000"); err == nil {
		t.Fatal("an unknown ZIP code is an error")
	}
	p, err = Find(context.Background(), "Pasadena, CA")
	if err != nil || p.Name != "Pasadena, CA" || p.Lat != 34.15 {
		t.Fatalf("the state picks the town: %+v %v", p, err)
	}
	if p, _ := Find(context.Background(), "Pasadena"); p.Name != "Pasadena, TX" {
		t.Fatalf("without one, the first: %+v", p)
	}
	if _, err := Find(context.Background(), "Pasadena, France"); err == nil {
		t.Fatal("no match is an error")
	}
	if n, us := Name(context.Background(), 34.05, -118.24); n != "Los Angeles, CA" || !us {
		t.Fatalf("got %q", n)
	}
}
