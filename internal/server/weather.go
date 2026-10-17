package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"homeport/internal/state"
	"homeport/internal/weather"
)

// MARK: Weather on the portal
//
// Each person's portal shows the weather where they are: where their browser says (when they allow it), or a ZIP code
// or town they typed. The place is kept with their account (to about a kilometre), so the portal shows it straight away.

func (s *Server) weatherRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me/weather", s.browserUser(s.myWeather))
	mux.HandleFunc("PUT /api/me/weather", s.browserUser(s.setMyWeather))
	mux.HandleFunc("GET /api/me/clocks", s.browserUser(s.myClocks))
	mux.HandleFunc("PUT /api/me/clocks", s.browserUser(s.setMyClocks))
}

// browserUser lets anyone signed in on the main domain through (as the actor).
func (s *Server) browserUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isMainHost(host(r)) {
			http.NotFound(w, r)
			return
		}
		u, _, ok := s.sessionUser(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errors.New("sign in first"))
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Homeport") != "1" {
			writeError(w, http.StatusForbidden, errors.New("missing X-Homeport header"))
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, u)))
	}
}

func (s *Server) myPlace(name string) state.WeatherPlace {
	var p state.WeatherPlace
	s.Store.Read(func(d *state.Data) {
		if u, ok := d.User(name); ok && u.Weather != nil {
			p = *u.Weather
		}
	})
	return p
}

func (s *Server) saveMyPlace(name string, p state.WeatherPlace) error {
	return s.Store.Update(func(d *state.Data) error {
		u, ok := d.User(name)
		if !ok {
			return state.ErrNotFound
		}
		u.Weather = &p
		return nil
	})
}

// myWeather is the forecast for the caller's place. lat and lon (from the browser) set the place when it's automatic;
// units ("f" or "c", from the browser's language) is the first guess at units.
func (s *Server) myWeather(w http.ResponseWriter, r *http.Request) {
	u := actor(r)
	p := s.myPlace(u.Name)
	if p.Off {
		writeJSON(w, http.StatusOK, map[string]any{"off": true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	q := r.URL.Query()
	lat, errLat := strconv.ParseFloat(q.Get("lat"), 64)
	lon, errLon := strconv.ParseFloat(q.Get("lon"), 64)
	if errLat == nil && errLon == nil && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180 && (p.Query == "" || p.Auto) {
		lat, lon = float64(int(lat*100))/100, float64(int(lon*100))/100
		if p.Name == "" || p.Lat != lat || p.Lon != lon {
			name, us := weather.Name(ctx, lat, lon)
			if p.Units == "" && us {
				p.Units = "f"
			}
			p = state.WeatherPlace{Name: name, Lat: lat, Lon: lon, Auto: true, Units: p.Units}
			_ = s.saveMyPlace(u.Name, p)
		}
	}
	if p.Name == "" && p.Lat == 0 && p.Lon == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"needPlace": true, "auto": p.Auto || p.Query == ""})
		return
	}
	units := p.Units
	if units == "" {
		units = q.Get("units")
	}
	f, err := weather.Get(ctx, weather.Place{Name: cmpOr(p.Name, "Here"), Lat: p.Lat, Lon: p.Lon}, units)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	// The other places, side by side (a place that doesn't answer is left out this time).
	more := s.myPlaces(u.Name)
	forecasts := make([]*weather.Forecast, len(more))
	var wg sync.WaitGroup
	for i, mp := range more {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if mf, err := weather.Get(ctx, weather.Place{Name: mp.Name, Lat: mp.Lat, Lon: mp.Lon}, f.Units); err == nil {
				forecasts[i] = &mf
			}
		}()
	}
	wg.Wait()
	type other struct {
		Index    int               `json:"index"`
		Forecast *weather.Forecast `json:"forecast"`
	}
	others := []other{}
	for i, mf := range forecasts {
		if mf != nil {
			others = append(others, other{Index: i, Forecast: mf})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"forecast": f, "auto": p.Auto, "query": p.Query, "more": others, "max": maxPlaces})
}

// setMyWeather changes the caller's place (query: a ZIP code or a town; auto: where the browser says), units or
// whether the weather shows at all.
func (s *Server) setMyWeather(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query *string `json:"query"`
		Auto  bool    `json:"auto"`
		Units *string `json:"units"`
		Off   *bool   `json:"off"`
		// Add is another place (a ZIP code or a town), Remove the index of one to take away.
		Add    *string `json:"add"`
		Remove *int    `json:"remove"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u := actor(r)
	if in.Add != nil || in.Remove != nil {
		s.changeMyPlaces(w, r, u.Name, in.Add, in.Remove)
		return
	}
	p := s.myPlace(u.Name)
	switch {
	case in.Auto:
		p = state.WeatherPlace{Auto: true, Units: p.Units}
	case in.Query != nil:
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		found, err := weather.Find(ctx, *in.Query)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if p.Units == "" && found.US {
			p.Units = "f"
		}
		p = state.WeatherPlace{Name: found.Name, Lat: found.Lat, Lon: found.Lon, Query: *in.Query, Units: p.Units}
	}
	if in.Units != nil && (*in.Units == "f" || *in.Units == "c") {
		p.Units = *in.Units
	}
	if in.Off != nil {
		p.Off = *in.Off
	}
	if err := s.saveMyPlace(u.Name, p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxPlaces is how many places the weather shows: the main one and two more.
const maxPlaces = 3

func (s *Server) myPlaces(name string) []state.WeatherPlace {
	var out []state.WeatherPlace
	s.Store.Read(func(d *state.Data) {
		if u, ok := d.User(name); ok {
			out = append(out, u.Places...)
		}
	})
	return out
}

func (s *Server) changeMyPlaces(w http.ResponseWriter, r *http.Request, name string, add *string, remove *int) {
	list := s.myPlaces(name)
	switch {
	case remove != nil:
		if *remove < 0 || *remove >= len(list) {
			writeError(w, http.StatusBadRequest, errors.New("no such place"))
			return
		}
		list = append(list[:*remove], list[*remove+1:]...)
	case add != nil:
		if len(list) >= maxPlaces-1 {
			writeError(w, http.StatusBadRequest, errors.New("the weather shows three places at most: remove one first"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		found, err := weather.Find(ctx, *add)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		list = append(list, state.WeatherPlace{Name: found.Name, Lat: found.Lat, Lon: found.Lon, Query: *add})
	}
	err := s.Store.Update(func(d *state.Data) error {
		u, ok := d.User(name)
		if !ok {
			return state.ErrNotFound
		}
		u.Places = list
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// MARK: World clock

// usualClocks are the world clock's cities until someone picks their own.
var usualClocks = []string{"America/New_York", "Europe/London", "Asia/Tokyo"}

const maxClocks = 6

func (s *Server) myClocks(w http.ResponseWriter, r *http.Request) {
	var zones []string
	s.Store.Read(func(d *state.Data) {
		if u, ok := d.User(actor(r).Name); ok {
			zones = u.Clocks
		}
	})
	if zones == nil {
		zones = usualClocks
	}
	writeJSON(w, http.StatusOK, map[string]any{"zones": zones, "max": maxClocks})
}

func (s *Server) setMyClocks(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Zones []string `json:"zones"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(in.Zones) > maxClocks {
		writeError(w, http.StatusBadRequest, errors.New("six clocks at most"))
		return
	}
	zones := []string{}
	for _, z := range in.Zones {
		if _, err := time.LoadLocation(z); err != nil || z == "" || z == "Local" {
			writeError(w, http.StatusBadRequest, errors.New(z+" isn't a time zone (pick one from the list)"))
			return
		}
		if !slices.Contains(zones, z) {
			zones = append(zones, z)
		}
	}
	err := s.Store.Update(func(d *state.Data) error {
		u, ok := d.User(actor(r).Name)
		if !ok {
			return state.ErrNotFound
		}
		u.Clocks = zones
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"zones": zones, "max": maxClocks})
}
