package server

import (
	"embed"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strings"

	"homeport/internal/state"
)

//go:embed web
var webFiles embed.FS

func assets() http.Handler {
	sub, _ := fs.Sub(webFiles, "web/assets")
	return http.FileServer(http.FS(sub))
}

func serveAdmin(w http.ResponseWriter, r *http.Request) {
	b, err := webFiles.ReadFile("web/admin.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

type pageData struct {
	Title string
	Main  string
	User  *state.User
	Apps  []state.App
	Links map[string]string // app id → its address (with the internet port when it has one)
	// MainAddr is the main domain with the port it's reached on (for links back to it).
	MainAddr string
	App      *state.App
	Return   string
	Error    string
	Name     string
	AI       string // the AI page's name, on the portal
	Greeting string // "Good morning", on the portal
}

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"initial": func(s string) string {
		if s == "" {
			return "?"
		}
		return strings.ToUpper(s[:1])
	},
	"icon": iconFor,
	// hue picks one of six colours for an app's initial, the same each time.
	"hue": func(s string) int {
		n := 0
		for _, c := range s {
			n += int(c)
		}
		return n % 6
	},
}).Parse(`
{{define "head"}}<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}} · {{.Main}}</title><link rel="stylesheet" href="/assets/style.css"></head><body class="page">{{end}}
{{define "foot"}}</body></html>{{end}}

{{define "login"}}{{template "head" .}}
<main class="narrow">
  <div class="brand"><span class="mark" aria-hidden="true"></span><span>{{.Main}}</span></div>
  <form class="card stack" method="post" action="/login">
    <h1>Sign in</h1>
    {{if .Error}}<p class="error" role="alert">{{.Error}}</p>{{end}}
    <label>User name<input name="name" autocomplete="username" autocapitalize="none" spellcheck="false" required value="{{.Name}}" autofocus></label>
    <label>Password<input name="password" type="password" autocomplete="current-password" required></label>
    <input type="hidden" name="return" value="{{.Return}}">
    <button class="primary" type="submit">Sign in</button>
  </form>
  <p class="muted small">One sign-in for every app on this server.</p>
</main>
{{template "foot" .}}{{end}}

{{define "portal"}}{{template "head" .}}
<header class="bar">
  <div class="brand"><span class="mark" aria-hidden="true"></span><span>{{.Main}}</span></div>
  <div class="who">
    <span>{{or .User.DisplayName .User.Name}}</span>
    {{if .User.Admin}}<a class="button" href="/admin">Admin</a>{{end}}
    <form method="post" action="/logout"><button type="submit">Sign out</button></form>
  </div>
</header>
<main class="wide portal">
  <section class="hero">
    <p class="greet">{{.Greeting}}, {{or .User.DisplayName .User.Name}}</p>
    <form class="search" action="https://www.google.com/search" method="get" role="search">
      <svg class="search-icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="11" cy="11" r="7"/><path d="m20 20-4-4"/></svg>
      <input name="q" type="search" placeholder="Search Google" aria-label="Search Google" autocomplete="off" autofocus>
      <button class="primary" type="submit">Search</button>
    </form>
    <div class="hero-row">
      <nav class="mail" aria-label="Webmail">
        <a href="https://mail.google.com/"><img src="/assets/icons/web/gmail.svg" alt=""><span>Gmail</span></a>
        <a href="https://outlook.live.com/mail/"><img src="/assets/icons/web/microsoft-outlook.svg" alt=""><span>Outlook</span></a>
        <a href="https://mail.yahoo.com/"><img src="/assets/icons/web/yahoo-mail.png" alt=""><span>Yahoo</span></a>
        <a href="https://www.icloud.com/mail"><img src="/assets/icons/web/icloud.svg" alt=""><span>iCloud</span></a>
        <a href="https://mail.proton.me/"><img src="/assets/icons/web/proton-mail.svg" alt=""><span>Proton</span></a>
        <a href="https://mail.aol.com/"><span class="mail-glyph" aria-hidden="true">Aol.</span><span>AOL</span></a>
      </nav>
      <div class="weather" id="weather" aria-live="polite" hidden></div>
      <section class="links" id="links" aria-label="Your links"></section>
    </div>
    <section class="news" id="news" aria-label="News" hidden></section>
  </section>
  <h2>Your apps</h2>
  {{if or .Apps .AI}}
  <ul class="tiles">
    {{if .AI}}<li><a class="tile" href="https://{{.AI}}/"><span class="glyph ai-glyph" aria-hidden="true">✦</span><span class="tname">AI</span><span class="tdomain">{{.AI}}</span></a></li>{{end}}
    {{range .Apps}}<li><a class="tile" href="https://{{index $.Links .ID}}/">{{with icon .Catalog}}<img class="glyph logo" src="{{.}}" alt="">{{else}}<span class="glyph c{{hue .Name}}" aria-hidden="true">{{initial .Name}}</span>{{end}}<span class="tname">{{.Name}}</span><span class="tdomain">{{index $.Links .ID}}</span></a></li>{{end}}
  </ul>
  {{else}}
  <p class="muted">No apps yet.{{if .User.Admin}} Install one from the <a href="/admin#apps">admin panel</a>.{{else}} Ask an admin to give you access to one.{{end}}</p>
  {{end}}
  <div id="reach"></div>
</main>
<script src="/assets/common.js" defer></script>
<script src="/assets/portal.js" defer></script>
{{template "foot" .}}{{end}}

{{define "denied"}}{{template "head" .}}
<main class="narrow">
  <div class="card stack">
    <h1>Not allowed</h1>
    <p>{{if .App}}Your account ({{.User.Name}}) doesn't have access to {{.App.Name}}.{{else}}Only admins can open this page.{{end}} An admin can give you access.</p>
    <p><a class="button" href="https://{{if .MainAddr}}{{.MainAddr}}{{else}}{{.Main}}{{end}}/">Back to your apps</a></p>
  </div>
</main>
{{template "foot" .}}{{end}}
`))

func renderPage(w io.Writer, name string, d pageData) {
	_ = pages.ExecuteTemplate(w, name, d)
}
