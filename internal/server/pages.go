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
	Title  string
	Main   string
	User   *state.User
	Apps   []state.App
	App    *state.App
	Return string
	Error  string
	Name   string
}

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"initial": func(s string) string {
		if s == "" {
			return "?"
		}
		return strings.ToUpper(s[:1])
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
    <span>{{.User.DisplayName}}</span>
    {{if .User.Admin}}<a class="button" href="/admin">Admin</a>{{end}}
    <form method="post" action="/logout"><button type="submit">Sign out</button></form>
  </div>
</header>
<main class="wide">
  <h1>Your apps</h1>
  {{if .Apps}}
  <ul class="tiles">
    {{range .Apps}}<li><a class="tile" href="https://{{.Domain}}/"><span class="glyph" aria-hidden="true">{{initial .Name}}</span><span class="tname">{{.Name}}</span><span class="tdomain">{{.Domain}}</span></a></li>{{end}}
  </ul>
  {{else}}
  <p class="muted">No apps yet.{{if .User.Admin}} Install one from the <a href="/admin#apps">admin panel</a>.{{else}} Ask an admin to give you access to one.{{end}}</p>
  {{end}}
</main>
{{template "foot" .}}{{end}}

{{define "denied"}}{{template "head" .}}
<main class="narrow">
  <div class="card stack">
    <h1>Not allowed</h1>
    <p>{{if .App}}Your account ({{.User.Name}}) doesn't have access to {{.App.Name}}.{{else}}Only admins can open this page.{{end}} An admin can give you access.</p>
    <p><a class="button" href="https://{{.Main}}/">Back to your apps</a></p>
  </div>
</main>
{{template "foot" .}}{{end}}
`))

func renderPage(w io.Writer, name string, d pageData) {
	_ = pages.ExecuteTemplate(w, name, d)
}
