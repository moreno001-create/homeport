package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
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
	writeJSON(w, http.StatusOK, map[string]any{"forecast": f, "auto": p.Auto, "query": p.Query})
}

// setMyWeather changes the caller's place (query: a ZIP code or a town; auto: where the browser says), units or
// whether the weather shows at all.
func (s *Server) setMyWeather(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Query *string `json:"query"`
		Auto  bool    `json:"auto"`
		Units *string `json:"units"`
		Off   *bool   `json:"off"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u := actor(r)
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
