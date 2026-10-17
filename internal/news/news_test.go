package news

import (
	"strings"
	"testing"
)

const feed = `<?xml version="1.0" encoding="UTF-8"?><rss><channel><title>Google News</title>
<item><title>Older story - AP News</title><link>https://news.google.com/rss/articles/1</link><pubDate>Fri, 16 Oct 2026 20:37:00 GMT</pubDate></item>
<item><title>Newer &amp; bigger story - AP News</title><link>https://news.google.com/rss/articles/2</link><pubDate>Sat, 17 Oct 2026 05:36:00 GMT</pubDate></item>
<item><title>No link</title><link>javascript:alert(1)</link></item>
</channel></rss>`

func TestParse(t *testing.T) {
	items, err := parse(strings.NewReader(feed), Source{Name: "AP", Suffix: " - AP News"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Title != "Newer & bigger story" || items[1].Title != "Older story" || items[0].Time.IsZero() {
		t.Fatalf("newest first, suffix off, bad links left out: %+v", items)
	}
	if _, err := parse(strings.NewReader(`<rss><channel></channel></rss>`), Source{Name: "X"}); err == nil {
		t.Fatal("an empty feed is an error")
	}
}
