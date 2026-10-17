// Package weather is the portal's weather: the forecast from Open-Meteo (free, no account), places from a US ZIP code
// (Zippopotam.us) or a town's name (Open-Meteo's geocoding), and a place's name from its coordinates (OpenStreetMap's
// Nominatim). Forecasts are kept for 10 minutes, so a busy portal asks rarely.
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Endpoints (tests point them elsewhere).
var (
	ForecastURL = "https://api.open-meteo.com/v1/forecast"
	SearchURL   = "https://geocoding-api.open-meteo.com/v1/search"
	ZipURL      = "https://api.zippopotam.us/us/"
	ReverseURL  = "https://nominatim.openstreetmap.org/reverse"
	Client      = &http.Client{Timeout: 10 * time.Second}
)

// Place is where the weather is for.
type Place struct {
	Name string  `json:"name"`
	Lat  float64 `json:"lat"`
	Lon  float64 `json:"lon"`
	US   bool    `json:"us,omitempty"` // in the United States (°F by default)
}

type Now struct {
	Temp      float64 `json:"temp"`
	FeelsLike float64 `json:"feelsLike"`
	Humidity  int     `json:"humidity"`
	Wind      float64 `json:"wind"`
	Code      int     `json:"code"`
	Text      string  `json:"text"`
	Icon      string  `json:"icon"`
	Day       bool    `json:"day"`
}

type Day struct {
	Date string  `json:"date"` // 2026-10-17
	High float64 `json:"high"`
	Low  float64 `json:"low"`
	Rain int     `json:"rain"` // chance of rain, %
	Code int     `json:"code"`
	Text string  `json:"text"`
	Icon string  `json:"icon"`
}

type Forecast struct {
	Place Place  `json:"place"`
	Units string `json:"units"` // "f" (°F, mph) or "c" (°C, km/h)
	// Timezone is the place's own (America/New_York), for its local time.
	Timezone string `json:"timezone,omitempty"`
	Now      Now    `json:"now"`
	Days     []Day  `json:"days"`
}

var cache = struct {
	sync.Mutex
	m map[string]cached
}{m: map[string]cached{}}

type cached struct {
	f  Forecast
	at time.Time
}

// round keeps coordinates to about a kilometre (enough for weather, and no more precise than that is kept).
func round(x float64) float64 { return math.Round(x*100) / 100 }

// Get is the weather at p, in units "f" or "c".
func Get(ctx context.Context, p Place, units string) (Forecast, error) {
	if units != "c" {
		units = "f"
	}
	p.Lat, p.Lon = round(p.Lat), round(p.Lon)
	key := fmt.Sprintf("%.2f,%.2f,%s", p.Lat, p.Lon, units)
	cache.Lock()
	if c, ok := cache.m[key]; ok && time.Since(c.at) < 10*time.Minute {
		cache.Unlock()
		c.f.Place = p
		return c.f, nil
	}
	cache.Unlock()
	q := url.Values{
		"latitude":  {strconv.FormatFloat(p.Lat, 'f', 2, 64)},
		"longitude": {strconv.FormatFloat(p.Lon, 'f', 2, 64)},
		"current":   {"temperature_2m,relative_humidity_2m,apparent_temperature,weather_code,wind_speed_10m,is_day"},
		"daily":     {"weather_code,temperature_2m_max,temperature_2m_min,precipitation_probability_max"},
		"timezone":  {"auto"}, "forecast_days": {"6"},
	}
	if units == "f" {
		q.Set("temperature_unit", "fahrenheit")
		q.Set("wind_speed_unit", "mph")
	}
	var raw struct {
		Current struct {
			Temp     float64 `json:"temperature_2m"`
			Humidity int     `json:"relative_humidity_2m"`
			Feels    float64 `json:"apparent_temperature"`
			Code     int     `json:"weather_code"`
			Wind     float64 `json:"wind_speed_10m"`
			IsDay    int     `json:"is_day"`
		} `json:"current"`
		Daily struct {
			Time []string  `json:"time"`
			Code []int     `json:"weather_code"`
			Max  []float64 `json:"temperature_2m_max"`
			Min  []float64 `json:"temperature_2m_min"`
			Rain []*int    `json:"precipitation_probability_max"`
		} `json:"daily"`
		Reason   string `json:"reason"`
		Timezone string `json:"timezone"`
	}
	if err := getJSON(ctx, ForecastURL+"?"+q.Encode(), &raw); err != nil {
		return Forecast{}, fmt.Errorf("couldn't get the forecast: %w", err)
	}
	c := raw.Current
	day := c.IsDay == 1
	f := Forecast{Place: p, Units: units, Timezone: raw.Timezone, Days: []Day{}, Now: Now{Temp: c.Temp, FeelsLike: c.Feels, Humidity: c.Humidity, Wind: c.Wind,
		Code: c.Code, Text: Describe(c.Code), Icon: Icon(c.Code, day), Day: day}}
	d := raw.Daily
	for i := range d.Time {
		if i >= len(d.Code) || i >= len(d.Max) || i >= len(d.Min) {
			break
		}
		x := Day{Date: d.Time[i], High: d.Max[i], Low: d.Min[i], Code: d.Code[i], Text: Describe(d.Code[i]), Icon: Icon(d.Code[i], true)}
		if i < len(d.Rain) && d.Rain[i] != nil {
			x.Rain = *d.Rain[i]
		}
		f.Days = append(f.Days, x)
	}
	cache.Lock()
	if len(cache.m) > 200 {
		cache.m = map[string]cached{}
	}
	cache.m[key] = cached{f: f, at: time.Now()}
	cache.Unlock()
	return f, nil
}

var zipRe = regexp.MustCompile(`^\d{5}(-\d{4})?$`)

// Find looks a place up: a US ZIP code (90210), or a town ("Pasadena", "Pasadena, CA", "Paris, France").
func Find(ctx context.Context, q string) (Place, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return Place{}, errors.New("type a ZIP code or a town")
	}
	if zipRe.MatchString(q) {
		var z struct {
			Places []struct {
				Name  string `json:"place name"`
				Lat   string `json:"latitude"`
				Lon   string `json:"longitude"`
				State string `json:"state abbreviation"`
			} `json:"places"`
		}
		if err := getJSON(ctx, ZipURL+q[:5], &z); err != nil || len(z.Places) == 0 {
			return Place{}, fmt.Errorf("couldn't find the ZIP code %s", q[:5])
		}
		lat, _ := strconv.ParseFloat(z.Places[0].Lat, 64)
		lon, _ := strconv.ParseFloat(z.Places[0].Lon, 64)
		return Place{Name: z.Places[0].Name + ", " + z.Places[0].State, Lat: lat, Lon: lon, US: true}, nil
	}
	name, where, _ := strings.Cut(q, ",")
	where = strings.ToLower(strings.TrimSpace(where))
	var res struct {
		Results []struct {
			Name    string  `json:"name"`
			Lat     float64 `json:"latitude"`
			Lon     float64 `json:"longitude"`
			Country string  `json:"country"`
			CC      string  `json:"country_code"`
			Admin1  string  `json:"admin1"`
		} `json:"results"`
	}
	u := SearchURL + "?" + url.Values{"name": {strings.TrimSpace(name)}, "count": {"10"}, "format": {"json"}}.Encode()
	if err := getJSON(ctx, u, &res); err != nil {
		return Place{}, fmt.Errorf("couldn't look %q up: %w", q, err)
	}
	for _, r := range res.Results {
		region := strings.ToLower(r.Admin1)
		if where == "" || strings.ToLower(r.CC) == where || strings.ToLower(r.Country) == where || region == where ||
			(r.CC == "US" && strings.ToLower(usStates[r.Admin1]) == where) {
			label := r.Name
			if r.CC == "US" && usStates[r.Admin1] != "" {
				label += ", " + usStates[r.Admin1]
			} else if r.Country != "" {
				label += ", " + r.Country
			}
			return Place{Name: label, Lat: r.Lat, Lon: r.Lon, US: r.CC == "US"}, nil
		}
	}
	return Place{}, fmt.Errorf("couldn't find %q: try a ZIP code, or the town with its state or country", q)
}

// Name is a place's name from its coordinates (the town), or "" when that isn't known.
func Name(ctx context.Context, lat, lon float64) (name string, us bool) {
	u := ReverseURL + "?" + url.Values{"lat": {strconv.FormatFloat(round(lat), 'f', 2, 64)}, "lon": {strconv.FormatFloat(round(lon), 'f', 2, 64)},
		"format": {"json"}, "zoom": {"10"}}.Encode()
	var r struct {
		Name    string `json:"name"`
		Address struct {
			City, Town, Village, State string
			CC                         string `json:"country_code"`
		} `json:"address"`
	}
	if getJSON(ctx, u, &r) != nil {
		return "", false
	}
	town := r.Name
	for _, n := range []string{r.Address.City, r.Address.Town, r.Address.Village} {
		if n != "" {
			town = n
			break
		}
	}
	us = strings.EqualFold(r.Address.CC, "us")
	if us && usStates[r.Address.State] != "" {
		return town + ", " + usStates[r.Address.State], true
	}
	return town, us
}

func getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Homeport (self-hosted home server; https://github.com/moreno001-create/homeport)")
	req.Header.Set("Accept", "application/json")
	res, err := Client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("answered %d", res.StatusCode)
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// Describe is a WMO weather code in words.
func Describe(code int) string {
	switch code {
	case 0:
		return "Clear"
	case 1:
		return "Mostly clear"
	case 2:
		return "Partly cloudy"
	case 3:
		return "Cloudy"
	case 45, 48:
		return "Fog"
	case 51, 53, 55:
		return "Drizzle"
	case 56, 57:
		return "Freezing drizzle"
	case 61, 63:
		return "Rain"
	case 65:
		return "Heavy rain"
	case 66, 67:
		return "Freezing rain"
	case 71, 73:
		return "Snow"
	case 75:
		return "Heavy snow"
	case 77:
		return "Snow grains"
	case 80, 81:
		return "Showers"
	case 82:
		return "Heavy showers"
	case 85, 86:
		return "Snow showers"
	case 95:
		return "Thunderstorms"
	case 96, 99:
		return "Thunderstorms with hail"
	}
	return "—"
}

// Icon is an emoji for a WMO weather code.
func Icon(code int, day bool) string {
	switch {
	case code == 0 && day:
		return "☀️"
	case code == 0:
		return "🌙"
	case code == 1 && day, code == 2 && day:
		return "⛅"
	case code <= 2:
		return "☁️"
	case code == 3:
		return "☁️"
	case code == 45 || code == 48:
		return "🌫️"
	case code >= 51 && code <= 67, code >= 80 && code <= 82:
		return "🌧️"
	case code >= 71 && code <= 77, code == 85 || code == 86:
		return "🌨️"
	case code >= 95:
		return "⛈️"
	}
	return "🌡️"
}

var usStates = map[string]string{"Alabama": "AL", "Alaska": "AK", "Arizona": "AZ", "Arkansas": "AR", "California": "CA",
	"Colorado": "CO", "Connecticut": "CT", "Delaware": "DE", "District of Columbia": "DC", "Florida": "FL", "Georgia": "GA",
	"Hawaii": "HI", "Idaho": "ID", "Illinois": "IL", "Indiana": "IN", "Iowa": "IA", "Kansas": "KS", "Kentucky": "KY",
	"Louisiana": "LA", "Maine": "ME", "Maryland": "MD", "Massachusetts": "MA", "Michigan": "MI", "Minnesota": "MN",
	"Mississippi": "MS", "Missouri": "MO", "Montana": "MT", "Nebraska": "NE", "Nevada": "NV", "New Hampshire": "NH",
	"New Jersey": "NJ", "New Mexico": "NM", "New York": "NY", "North Carolina": "NC", "North Dakota": "ND", "Ohio": "OH",
	"Oklahoma": "OK", "Oregon": "OR", "Pennsylvania": "PA", "Rhode Island": "RI", "South Carolina": "SC",
	"South Dakota": "SD", "Tennessee": "TN", "Texas": "TX", "Utah": "UT", "Vermont": "VT", "Virginia": "VA",
	"Washington": "WA", "West Virginia": "WV", "Wisconsin": "WI", "Wyoming": "WY", "Puerto Rico": "PR"}
