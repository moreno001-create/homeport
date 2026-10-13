package assist

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// The troubleshooting guide is built into Homeport: problems seen on real servers, how to tell them, and the fix, so
// assistants on every server start from what was learned on the others. One Markdown file per problem:
//
//	# Title
//	Signs: words someone (or a log) would use
//	Applies: any, linux or macos
//	## Why … ## Fix …
//
//go:embed guide/*.md
var guideFiles embed.FS

// GuideEntry is one problem in the guide.
type GuideEntry struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Signs   string `json:"signs"`
	Applies string `json:"applies"`
	Text    string `json:"text"` // the whole entry, Markdown
}

// Guide is every entry that applies to this system ("linux" or "macos").
func Guide(osName string) []GuideEntry {
	var out []GuideEntry
	files, _ := fs.Glob(guideFiles, "guide/*.md")
	for _, f := range files {
		b, err := guideFiles.ReadFile(f)
		if err != nil {
			continue
		}
		e := GuideEntry{ID: strings.TrimSuffix(strings.TrimPrefix(f, "guide/"), ".md"), Text: string(b), Applies: "any"}
		for _, line := range strings.Split(string(b), "\n") {
			switch {
			case strings.HasPrefix(line, "# ") && e.Title == "":
				e.Title = strings.TrimPrefix(line, "# ")
			case strings.HasPrefix(line, "Signs: "):
				e.Signs = strings.TrimPrefix(line, "Signs: ")
			case strings.HasPrefix(line, "Applies: "):
				e.Applies = strings.TrimPrefix(line, "Applies: ")
			}
		}
		if e.Applies != "any" && osName != "" && e.Applies != osName {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

var guideWord = regexp.MustCompile(`[a-z0-9.]+`)

// SearchGuide ranks entries by the words they share with q (title and signs count most). q "" lists them all.
func SearchGuide(osName, q string) []GuideEntry {
	all := Guide(osName)
	words := guideWord.FindAllString(strings.ToLower(q), -1)
	if len(words) == 0 {
		return all
	}
	type scored struct {
		e GuideEntry
		n int
	}
	var hits []scored
	for _, e := range all {
		head := strings.ToLower(e.Title + " " + e.Signs + " " + e.ID)
		body := strings.ToLower(e.Text)
		n := 0
		for _, w := range words {
			if len(w) < 3 {
				continue
			}
			if strings.Contains(head, w) {
				n += 3
			} else if strings.Contains(body, w) {
				n++
			}
		}
		if n > 0 {
			hits = append(hits, scored{e, n})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].n > hits[j].n })
	var out []GuideEntry
	for _, h := range hits {
		out = append(out, h.e)
	}
	return out
}

func (a *Assistant) guideTool() Tool {
	return Tool{Name: "troubleshooting_guide", Level: Look,
		Description: "Homeport's built-in troubleshooting guide: known problems, how to recognise them, and the fix. Search it with the symptom or an error message before diagnosing; with no search, lists every topic.",
		Params:      map[string]any{"search": str("The symptom or error, in a few words (e.g. \"certificate warning\", \"502\", \"no space left\"). Empty: list the topics.")},
		Run: func(_ context.Context, args Args) (string, error) {
			q := args.Str("search")
			found := SearchGuide(a.Platform.Name, q)
			var b strings.Builder
			if q == "" {
				b.WriteString("Topics (search with a few words to read one):\n")
				for _, e := range found {
					fmt.Fprintf(&b, "- %s (signs: %s)\n", e.Title, e.Signs)
				}
				return b.String(), nil
			}
			if len(found) == 0 {
				return "Nothing in the guide matches that. Diagnose with the look tools (logs, health, check results).", nil
			}
			for i, e := range found {
				if i == 2 {
					fmt.Fprintf(&b, "\nAlso related: ")
					for _, more := range found[2:min(len(found), 6)] {
						fmt.Fprintf(&b, "%q ", more.Title)
					}
					break
				}
				b.WriteString(e.Text + "\n")
			}
			return b.String(), nil
		}}
}
