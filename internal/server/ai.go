package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"homeport/internal/assist"
	"homeport/internal/state"
)

// MARK: AI page
//
// The AI page is at ai.<main domain>: a chat with the local model for everyone with an account. People who aren't admins
// get a plain chat (the local model only, no tools: it can't see or change the server, and nothing goes to Claude);
// admins get the full assistant, as in the admin panel. Signing in works as for an app: the portal hands the sign-in
// over to this domain.

// address is a name with the port it's reached on (state.Data.Address).
func (s *Server) address(domain string) string {
	if domain == "" {
		return ""
	}
	a := domain
	s.Store.Read(func(d *state.Data) { a = d.Address(domain) })
	return a
}

// aiHost is the AI page's name, or "" when there isn't one.
func (s *Server) aiHost() string {
	var h string
	s.Store.Read(func(d *state.Data) { h = d.AIDomain() })
	return h
}

func (s *Server) aiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.aiPage)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", assets()))
	mux.HandleFunc("GET /.homeport/callback", s.callback)
	mux.HandleFunc("GET /.homeport/signout", s.appSignOut)
	mux.HandleFunc("GET /api/ai/me", s.aiUser(s.aiMe))
	mux.HandleFunc("GET /api/ai/model", s.aiUser(s.aiModel))
	mux.HandleFunc("POST /api/ai/model/load", s.aiUser(s.aiLoad))
	mux.HandleFunc("GET /api/ai/busy", s.aiUser(s.aiBusy))
	mux.HandleFunc("GET /api/ai/chat", s.aiUser(s.listChats))
	mux.HandleFunc("POST /api/ai/chat", s.aiUser(s.aiSend))
	mux.HandleFunc("GET /api/ai/chat/{id}", s.aiUser(s.getChat))
	mux.HandleFunc("POST /api/ai/chat/{id}/stop", s.aiUser(s.stopChat))
	mux.HandleFunc("DELETE /api/ai/chat/{id}", s.aiUser(s.deleteChat))
	// Admins: Claude when asked first, and approving changes, right on the page.
	mux.HandleFunc("POST /api/ai/chat/{id}/continue", s.aiUser(s.aiAdmin(s.continueChat)))
	mux.HandleFunc("GET /api/ai/approvals", s.aiUser(s.aiAdmin(s.listApprovals)))
	mux.HandleFunc("POST /api/ai/approvals/{id}/{decision}", s.aiUser(s.aiAdmin(s.decideApproval)))
	return mux
}

// aiUser lets anyone signed in on the AI page's domain through (as the actor), like browserAdmin does for admins.
func (s *Server) aiUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) aiAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !actor(r).Admin {
			writeError(w, http.StatusForbidden, errors.New("only admins can do this"))
			return
		}
		next(w, r)
	}
}

func (s *Server) aiPage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.sessionUser(r); !ok {
		back := "https://" + host(r) + "/"
		http.Redirect(w, r, "https://"+s.address(s.settings().MainDomain)+"/sso/continue?return="+url.QueryEscape(back), http.StatusFound)
		return
	}
	b, err := webFiles.ReadFile("web/ai.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func (s *Server) aiMe(w http.ResponseWriter, r *http.Request) {
	u := actor(r)
	writeJSON(w, http.StatusOK, map[string]any{"name": u.Name, "displayName": u.DisplayName, "admin": u.Admin,
		"mainDomain": s.settings().MainDomain, "mainAddress": s.address(s.settings().MainDomain)})
}

// localChoices is the local server's chat models, and the one the settings pick.
func (s *Server) localChoices(ctx context.Context) (models []string, chosen string) {
	st := s.settings().Chat
	if base, err := assist.LocalBase(st.LocalURL); err == nil && base != "" {
		models, _ = assist.LocalModels(ctx, base)
	}
	chosen = st.LocalModel
	if chosen == "" && len(models) > 0 {
		chosen = models[0]
	}
	return models, chosen
}

type modelChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// aiModel describes a local model (?model=, default the one in the settings) and lists the ones to choose from
// (configured: false when there's no local model). Admins can also choose Claude, or automatic, as in the settings.
func (s *Server) aiModel(w http.ResponseWriter, r *http.Request) {
	st := s.settings().Chat
	if st.LocalURL == "" && !actor(r).Admin {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	models, chosen := s.localChoices(ctx)
	out := struct {
		Configured bool          `json:"configured"`
		Default    string        `json:"default"`
		Choices    []modelChoice `json:"choices"`
		Claude     bool          `json:"claude,omitempty"` // admins: Claude can be chosen
		assist.ModelInfo
	}{Configured: st.LocalURL != "", Default: chosen, Claude: actor(r).Admin && s.Chat.APIKey() != ""}
	for _, m := range models {
		out.Choices = append(out.Choices, modelChoice{m, assist.PrettyModel(m)})
	}
	if want := r.URL.Query().Get("model"); want != "" && slices.Contains(models, want) {
		chosen = want
	}
	if chosen != "" {
		out.ModelInfo = assist.LocalModelInfo(ctx, st.LocalURL, chosen, actor(r).Admin)
	}
	writeJSON(w, http.StatusOK, out)
}

// aiLoad has the local server load the model now, so the first answer comes sooner. It returns when it's loaded.
func (s *Server) aiLoad(w http.ResponseWriter, r *http.Request) {
	st := s.settings().Chat
	if st.LocalURL == "" {
		writeError(w, http.StatusBadRequest, errors.New("there's no local model yet"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	models, chosen := s.localChoices(ctx)
	if want := r.URL.Query().Get("model"); slices.Contains(models, want) {
		chosen = want
	}
	if err := assist.WarmLocal(ctx, st.LocalURL, chosen); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// aiBusy is how busy the machine is right now (processor, memory, graphics), for the page's live meter.
func (s *Server) aiBusy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.load.Read(ctx))
}

// aiSend: admins get the full assistant, everyone else a plain chat with the local model. model is one of the local
// server's models, "" (as in the settings) or, for admins, "claude".
func (s *Server) aiSend(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Conversation string `json:"conversation"`
		Text         string `json:"text"`
		Model        string `json:"model"`
	}
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u := actor(r)
	if in.Model != "" && in.Model != "claude" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		models, _ := s.localChoices(ctx)
		cancel()
		if !slices.Contains(models, in.Model) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("there's no model %q on the local model server", in.Model))
			return
		}
	}
	var conv assist.Conversation
	var err error
	switch {
	case !u.Admin && in.Model == "claude":
		err = errors.New("only admins can use Claude")
	case !u.Admin:
		conv, err = s.Chat.SendPlain(u.Name, in.Conversation, in.Text, in.Model)
	case in.Model == "claude":
		conv, err = s.Chat.Send(u.Name, in.Conversation, in.Text, "claude", true)
	case in.Model != "":
		conv, err = s.Chat.SendLocal(u.Name, in.Conversation, in.Text, in.Model)
	default:
		conv, err = s.Chat.Send(u.Name, in.Conversation, in.Text, "", false)
	}
	if err != nil {
		chatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conv)
}
