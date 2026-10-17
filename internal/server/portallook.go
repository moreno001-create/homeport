package server

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"homeport/internal/state"
)

// MARK: Customizing the portal
//
// Each person picks their portal's background (one of a few, or a picture of their own) and which parts it shows.
// A picture is kept in Homeport's state folder, one per person, and served only to them.

// Backgrounds are the ready-made backgrounds (style.css draws them).
var Backgrounds = []string{"sunrise", "ocean", "forest", "lavender", "night", "slate", "plain"}

// PortalParts are the parts of the portal that can be hidden.
var PortalParts = []string{"search", "mail", "weather", "links", "clocks", "news", "apps", "reach"}

const maxBackground = 8 << 20

func (s *Server) lookRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me/portal", s.browserUser(s.myLook))
	mux.HandleFunc("PUT /api/me/portal", s.browserUser(s.setMyLook))
	mux.HandleFunc("GET /api/me/background", s.browserUser(s.myBackground))
	mux.HandleFunc("PUT /api/me/background", s.browserUser(s.setMyBackground))
}

func (s *Server) lookOf(name string) state.PortalLook {
	var l state.PortalLook
	s.Store.Read(func(d *state.Data) {
		if u, ok := d.User(name); ok && u.Portal != nil {
			l = *u.Portal
			l.Hide = append([]string(nil), u.Portal.Hide...)
		}
	})
	return l
}

func (s *Server) backgroundPath(name string) string {
	return filepath.Join(filepath.Dir(s.Store.Path()), "backgrounds", filepath.Base(name)) // next to state.json
}

func (s *Server) myLook(w http.ResponseWriter, r *http.Request) {
	l := s.lookOf(actor(r).Name)
	if l.Hide == nil {
		l.Hide = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"background": l.Background, "hide": l.Hide,
		"backgrounds": Backgrounds, "parts": PortalParts})
}

func (s *Server) setMyLook(w http.ResponseWriter, r *http.Request) {
	var in state.PortalLook
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := actor(r).Name
	if in.Background != "" && in.Background != "picture" && !slices.Contains(Backgrounds, in.Background) {
		writeError(w, http.StatusBadRequest, errors.New("no background called "+in.Background))
		return
	}
	if in.Background == "picture" {
		if _, err := os.Stat(s.backgroundPath(name)); err != nil {
			writeError(w, http.StatusBadRequest, errors.New("send the picture first"))
			return
		}
	}
	hide := []string{}
	for _, p := range in.Hide {
		if !slices.Contains(PortalParts, p) {
			writeError(w, http.StatusBadRequest, errors.New("no part of the portal called "+p))
			return
		}
		if !slices.Contains(hide, p) {
			hide = append(hide, p)
		}
	}
	err := s.Store.Update(func(d *state.Data) error {
		u, ok := d.User(name)
		if !ok {
			return state.ErrNotFound
		}
		u.Portal = &state.PortalLook{Background: in.Background, Hide: hide}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if in.Background != "picture" {
		_ = os.Remove(s.backgroundPath(name)) // a picture they've moved away from isn't kept
	}
	w.WriteHeader(http.StatusNoContent)
}

// setMyBackground keeps a picture (JPEG, PNG or WebP, the page shrinks it first) as the caller's background.
func (s *Server) setMyBackground(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBackground))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("the picture is too big (8 MB at most)"))
		return
	}
	if t := http.DetectContentType(b); !slices.Contains([]string{"image/jpeg", "image/png", "image/webp"}, t) {
		writeError(w, http.StatusBadRequest, errors.New("send a JPEG, PNG or WebP picture"))
		return
	}
	path := s.backgroundPath(actor(r).Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) myBackground(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(s.backgroundPath(actor(r).Name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(b))
	w.Header().Set("Cache-Control", "private, no-cache")
	_, _ = w.Write(b)
}

// lookClass is the body's class for someone's background, and hidden says which parts they've hidden.
func lookClass(l state.PortalLook) string {
	if l.Background == "" {
		return ""
	}
	return "bg-" + strings.ToLower(l.Background)
}
