// Package news is the portal's headlines: BBC's own feed, and AP's and Reuters' through Google News (neither has a
// feed of its own any more). Fetched by the server, kept for 15 minutes.
package news

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source is one news source.
type Source struct {
	ID, Name, Feed string
	// Suffix is what Google News adds to its titles (" - AP News"), taken off.
	Suffix string
}

var Sources = []Source{
	{ID: "ap", Name: "AP", Feed: "https://news.google.com/rss/search?q=when:2d+site:apnews.com&hl=en-US&gl=US&ceid=US:en", Suffix: " - AP News"},
	{ID: "bbc", Name: "BBC", Feed: "https://feeds.bbci.co.uk/news/rss.xml"},
	{ID: "reuters", Name: "Reuters", Feed: "https://news.google.com/rss/search?q=when:2d+site:reuters.com&hl=en-US&gl=US&ceid=US:en", Suffix: " - Reuters"},
}

// Client fetches the feeds (tests replace it).
var Client = &http.Client{Timeout: 12 * time.Second}

type Item struct {
	Title string    `json:"title"`
	Link  string    `json:"link"`
	Time  time.Time `json:"time"`
}

var cache = struct {
	sync.Mutex
	m map[string]cached
}{m: map[string]cached{}}

type cached struct {
	items []Item
	at    time.Time
}

// Find is the source with id.
func Find(id string) (Source, bool) {
	for _, s := range Sources {
		if s.ID == id {
			return s, true
		}
	}
	return Source{}, false
}

// Headlines are a source's newest headlines (at most n).
func Headlines(ctx context.Context, id string, n int) ([]Item, error) {
	src, ok := Find(id)
	if !ok {
		return nil, fmt.Errorf("no news source %q", id)
	}
	cache.Lock()
	if c, ok := cache.m[id]; ok && time.Since(c.at) < 15*time.Minute {
		cache.Unlock()
		return first(c.items, n), nil
	}
	cache.Unlock()
	items, err := fetch(ctx, src)
	if err != nil {
		cache.Lock()
		c, ok := cache.m[id]
		cache.Unlock()
		if ok { // an older list beats none
			return first(c.items, n), nil
		}
		return nil, err
	}
	cache.Lock()
	cache.m[id] = cached{items: items, at: time.Now()}
	cache.Unlock()
	return first(items, n), nil
}

func first(items []Item, n int) []Item {
	if n > 0 && len(items) > n {
		return items[:n]
	}
	return items
}

func fetch(ctx context.Context, src Source) ([]Item, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", src.Feed, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Homeport home server)")
	res, err := Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("couldn't reach %s", src.Name)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s's news answered %d", src.Name, res.StatusCode)
	}
	return parse(io.LimitReader(res.Body, 4<<20), src)
}

func parse(r io.Reader, src Source) ([]Item, error) {
	var feed struct {
		Items []struct {
			Title   string `xml:"title"`
			Link    string `xml:"link"`
			PubDate string `xml:"pubDate"`
		} `xml:"channel>item"`
	}
	dec := xml.NewDecoder(r)
	dec.Strict = false
	if err := dec.Decode(&feed); err != nil {
		return nil, fmt.Errorf("couldn't read %s's news: %w", src.Name, err)
	}
	var out []Item
	for _, it := range feed.Items {
		title := strings.TrimSpace(html.UnescapeString(it.Title))
		if src.Suffix != "" {
			title = strings.TrimSuffix(title, src.Suffix)
		}
		link := strings.TrimSpace(it.Link)
		if title == "" || !strings.HasPrefix(link, "https://") {
			continue
		}
		t, _ := time.Parse(time.RFC1123, strings.TrimSpace(it.PubDate))
		if t.IsZero() {
			t, _ = time.Parse(time.RFC1123Z, strings.TrimSpace(it.PubDate))
		}
		out = append(out, Item{Title: title, Link: link, Time: t})
	}
	if len(out) == 0 {
		return nil, errors.New(src.Name + " has no headlines right now")
	}
	// Newest first (Google News lists by relevance).
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}
