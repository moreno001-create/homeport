package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"homeport/internal/news"
	"homeport/internal/state"
)

// MARK: Quick links and news on the portal
//
// Each person keeps their own quick links (to the right of the weather), and picks a news source (AP, BBC or Reuters)
// for the headlines under it. Links' little icons come through the server (DuckDuckGo's icon service), since the
// portal only loads pictures from its own address.

const maxLinks = 16

func (s *Server) startRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me/links", s.browserUser(s.myLinks))
	mux.HandleFunc("PUT /api/me/links", s.browserUser(s.setMyLinks))
	mux.HandleFunc("GET /api/me/icon", s.browserUser(s.linkIcon))
	mux.HandleFunc("GET /api/me/news", s.browserUser(s.myNews))
}

func (s *Server) myLinks(w http.ResponseWriter, r *http.Request) {
	links := []state.QuickLink{}
	s.Store.Read(func(d *state.Data) {
		if u, ok := d.User(actor(r).Name); ok && u.Links != nil {
			links = append(links, u.Links...)
		}
	})
	writeJSON(w, http.StatusOK, links)
}

// cleanLink checks a link someone adds: a web address (https:// added when missing) and a short name.
func cleanLink(l state.QuickLink) (state.QuickLink, error) {
	raw := strings.TrimSpace(l.URL)
	if raw == "" {
		return l, errors.New("type the site's address")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.ContainsAny(u.Host, " <>\"'") {
		return l, errors.New("that isn't a web address (like netflix.com or https://example.com/page)")
	}
	name := strings.TrimSpace(l.Name)
	if name == "" {
		name = strings.TrimPrefix(u.Hostname(), "www.")
		if i := strings.Index(name, "."); i > 0 && net.ParseIP(name) == nil {
			name = strings.ToUpper(name[:1]) + name[1:i]
		}
	}
	if r := []rune(name); len(r) > 24 {
		name = string(r[:24])
	}
	return state.QuickLink{Name: name, URL: u.String()}, nil
}

func (s *Server) setMyLinks(w http.ResponseWriter, r *http.Request) {
	var in []state.QuickLink
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(in) > maxLinks {
		writeError(w, http.StatusBadRequest, errors.New("keep it to 16 links"))
		return
	}
	out := make([]state.QuickLink, 0, len(in))
	for _, l := range in {
		c, err := cleanLink(l)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		out = append(out, c)
	}
	err := s.Store.Update(func(d *state.Data) error {
		u, ok := d.User(actor(r).Name)
		if !ok {
			return state.ErrNotFound
		}
		u.Links = out
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// IconURL is where a site's icon comes from (tests point it elsewhere).
var IconURL = "https://icons.duckduckgo.com/ip3/"

var siteIcons = struct {
	sync.Mutex
	m map[string]iconCache
}{m: map[string]iconCache{}}

type iconCache struct {
	data []byte
	typ  string
	at   time.Time
}

// linkIcon sends a site's small icon (host: its name), kept for a day.
func (s *Server) linkIcon(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(strings.TrimPrefix(r.URL.Query().Get("host"), "www."))
	if !hostRe.MatchString(host) || len(host) > 200 {
		http.NotFound(w, r)
		return
	}
	siteIcons.Lock()
	c, ok := siteIcons.m[host]
	siteIcons.Unlock()
	if !ok || time.Since(c.at) > 24*time.Hour {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		c = iconCache{at: time.Now()}
		if req, err := http.NewRequestWithContext(ctx, "GET", IconURL+host+".ico", nil); err == nil {
			if res, err := (&http.Client{Timeout: 8 * time.Second}).Do(req); err == nil {
				b, _ := io.ReadAll(io.LimitReader(res.Body, 256<<10))
				res.Body.Close()
				if t := http.DetectContentType(b); res.StatusCode == http.StatusOK && strings.HasPrefix(t, "image/") && t != "image/svg+xml" {
					c.data, c.typ = b, t
				}
			}
		}
		siteIcons.Lock()
		if len(siteIcons.m) > 500 {
			siteIcons.m = map[string]iconCache{}
		}
		siteIcons.m[host] = c
		siteIcons.Unlock()
	}
	if c.data == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", c.typ)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(c.data)
}

// myNews is the headlines from a source (source; else the one the caller last chose, else AP), and remembers the
// choice.
func (s *Server) myNews(w http.ResponseWriter, r *http.Request) {
	name := actor(r).Name
	id := r.URL.Query().Get("source")
	var saved string
	s.Store.Read(func(d *state.Data) {
		if u, ok := d.User(name); ok {
			saved = u.News
		}
	})
	if _, ok := news.Find(id); !ok {
		id = cmpOr(saved, "ap")
	}
	if id != saved {
		_ = s.Store.Update(func(d *state.Data) error {
			if u, ok := d.User(name); ok {
				u.News = id
			}
			return nil
		})
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	items, err := news.Headlines(ctx, id, 8)
	sources := []map[string]string{}
	for _, src := range news.Sources {
		sources = append(sources, map[string]string{"id": src.ID, "name": src.Name})
	}
	out := map[string]any{"source": id, "sources": sources, "items": items}
	if err != nil {
		out["error"] = err.Error()
		out["items"] = []news.Item{}
	}
	writeJSON(w, http.StatusOK, out)
}
