package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"homeport/internal/assist"
	"homeport/internal/state"
)

// MARK: Assistant chat

func (s *Server) chatRoutes(mux *http.ServeMux, guard func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/chat", guard(s.listChats))
	mux.HandleFunc("POST /api/chat", guard(s.sendChat))
	mux.HandleFunc("GET /api/chat/{id}", guard(s.getChat))
	mux.HandleFunc("POST /api/chat/{id}/continue", guard(s.continueChat))
	mux.HandleFunc("POST /api/chat/{id}/stop", guard(s.stopChat))
	mux.HandleFunc("DELETE /api/chat/{id}", guard(s.deleteChat))
	mux.HandleFunc("GET /api/chat-settings", guard(s.chatSettings))
	mux.HandleFunc("PUT /api/chat-settings", guard(s.putChatSettings))
	mux.HandleFunc("POST /api/chat-settings/local-models", guard(s.localModels))
}

func chatError(w http.ResponseWriter, err error) {
	if errors.Is(err, assist.ErrNoConversation) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

func (s *Server) listChats(w http.ResponseWriter, r *http.Request) {
	list := s.Chat.List(actor(r).Name)
	if list == nil {
		list = []assist.Summary{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) sendChat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Conversation string `json:"conversation"`
		Text         string `json:"text"`
		Use          string `json:"use"`
		AllowCloud   bool   `json:"allowCloud"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	conv, err := s.Chat.Send(actor(r).Name, in.Conversation, in.Text, in.Use, in.AllowCloud)
	if err != nil {
		chatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

func (s *Server) getChat(w http.ResponseWriter, r *http.Request) {
	conv, err := s.Chat.Get(actor(r).Name, r.PathValue("id"))
	if err != nil {
		chatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

func (s *Server) continueChat(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Use        string `json:"use"`
		AllowCloud bool   `json:"allowCloud"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	conv, err := s.Chat.Continue(actor(r).Name, r.PathValue("id"), in.Use, in.AllowCloud)
	if err != nil {
		chatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}

func (s *Server) stopChat(w http.ResponseWriter, r *http.Request) {
	if err := s.Chat.Stop(actor(r).Name, r.PathValue("id")); err != nil {
		chatError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteChat(w http.ResponseWriter, r *http.Request) {
	if err := s.Chat.Delete(actor(r).Name, r.PathValue("id")); err != nil {
		chatError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) chatSettings(w http.ResponseWriter, r *http.Request) {
	var out struct {
		Settings     state.ChatSettings `json:"settings"`
		HasKey       bool               `json:"hasKey"`
		KeyHint      string             `json:"keyHint,omitempty"`
		Spent        assist.Usage       `json:"spent"`
		ClaudeModels []string           `json:"claudeModels"`
		Defaults     map[string]string  `json:"defaults"`
	}
	s.Store.Read(func(d *state.Data) { out.Settings = d.Settings().Chat })
	out.HasKey, out.KeyHint, out.Spent = s.Chat.APIKey() != "", s.Chat.KeyHint(), s.Chat.Spent()
	out.ClaudeModels = []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5"}
	out.Defaults = map[string]string{"mode": "auto", "claudeModel": assist.DefaultClaudeModel, "effort": assist.DefaultEffort}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) putChatSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		state.ChatSettings
		// APIKey replaces the saved key when given; RemoveKey deletes it.
		APIKey    string `json:"apiKey"`
		RemoveKey bool   `json:"removeKey"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	st := in.ChatSettings
	switch st.Mode {
	case "", "auto", "local", "claude":
	default:
		writeError(w, http.StatusBadRequest, errors.New(`mode is "auto", "local" or "claude"`))
		return
	}
	switch st.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		writeError(w, http.StatusBadRequest, errors.New("effort is low, medium, high, xhigh or max"))
		return
	}
	if st.MonthlyCapUSD < 0 || st.MonthlyCapUSD > 100000 {
		writeError(w, http.StatusBadRequest, errors.New("the monthly limit is a dollar amount (0 for none)"))
		return
	}
	base, err := assist.LocalBase(st.LocalURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	st.LocalURL = base
	switch {
	case in.RemoveKey:
		err = s.Chat.SetAPIKey("")
	case in.APIKey != "":
		err = s.Chat.SetAPIKey(in.APIKey)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Store.Update(func(d *state.Data) error { d.Settings().Chat = st; return nil }); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.chatSettings(w, r)
}

func (s *Server) localModels(w http.ResponseWriter, r *http.Request) {
	var in struct {
		LocalURL string `json:"localURL"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	base, err := assist.LocalBase(in.LocalURL)
	if err == nil && base == "" {
		err = errors.New("type the local model's address first")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	models, err := assist.LocalModels(ctx, base)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if models == nil {
		models = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"localURL": base, "models": models})
}
