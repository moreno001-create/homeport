package server

import (
	"errors"
	"net/http"

	"homeport/internal/auth"
	"homeport/internal/state"
)

// Changing your own password from the portal: the current one first (wrong ones count like wrong sign-ins, so it
// can't be guessed from an unlocked computer), then the new one. Every other browser and app signed in as you is
// signed out; this one stays signed in.

func (s *Server) passwordRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/me/password", s.browserUser(s.changeMyPassword))
}

func (s *Server) changeMyPassword(w http.ResponseWriter, r *http.Request) {
	u := actor(r)
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ip := clientIP(r)
	if s.tooManyBadLogins(ip) {
		writeError(w, http.StatusTooManyRequests, errors.New("too many wrong passwords from this address; wait a few minutes and try again"))
		return
	}
	if !auth.CheckPassword(u.Password, in.Current) {
		s.noteBadLogin(ip)
		writeError(w, http.StatusForbidden, errors.New("that isn't your current password"))
		return
	}
	if in.New == in.Current {
		writeError(w, http.StatusBadRequest, errors.New("the new password is the same as the old one"))
		return
	}
	if err := auth.ValidPassword(in.New); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	hash, err := auth.HashPassword(in.New)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	_, keep, _ := s.sessionUser(r)
	err = s.Store.Update(func(d *state.Data) error {
		me, ok := d.User(u.Name)
		if !ok {
			return state.ErrNotFound
		}
		me.Password = hash
		d.SignOutElsewhere(u.Name, keep)
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
