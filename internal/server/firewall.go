package server

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// MARK: Firewall

func (s *Server) firewallRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/firewall", guard(s.firewallStatus))
	mux.HandleFunc("POST /api/firewall/{state}", guard(s.firewallTurn))
	mux.HandleFunc("POST /api/firewall/rules", guard(s.firewallOpen))
	mux.HandleFunc("DELETE /api/firewall/rules", guard(s.firewallClose))
}

func (s *Server) firewallStatus(w http.ResponseWriter, r *http.Request) {
	if s.Firewall == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "why": "not available on this system"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.Firewall.Status(ctx))
}

func (s *Server) firewallTurn(w http.ResponseWriter, r *http.Request) {
	if s.Firewall == nil {
		writeError(w, http.StatusNotFound, errors.New("not available on this system"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	var err error
	switch r.PathValue("state") {
	case "on":
		err = s.Firewall.TurnOn(ctx)
	case "off":
		err = s.Firewall.TurnOff(ctx)
	default:
		err = errors.New("on or off")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type firewallRule struct {
	Port  string `json:"port"`
	Proto string `json:"proto"`
	From  string `json:"from"`
	Label string `json:"label"`
}

func (s *Server) firewallOpen(w http.ResponseWriter, r *http.Request) {
	var in firewallRule
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.Firewall == nil {
		writeError(w, http.StatusNotFound, errors.New("not available on this system"))
		return
	}
	if err := s.Firewall.Open(r.Context(), in.Port, in.Proto, in.From, in.Label); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) firewallClose(w http.ResponseWriter, r *http.Request) {
	var in firewallRule
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.Firewall == nil {
		writeError(w, http.StatusNotFound, errors.New("not available on this system"))
		return
	}
	if err := s.Firewall.Close(r.Context(), in.Port, in.Proto, in.From); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
