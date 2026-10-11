package catalog

import (
	"cmp"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var idRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,40}$`)

// Parse reads a manifest that didn't come with Homeport (an app from a repository).
func Parse(text string) (App, error) {
	var a App
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(&a); err != nil {
		return App{}, fmt.Errorf("the manifest isn't valid: %v", err)
	}
	switch {
	case !idRe.MatchString(a.ID):
		return App{}, fmt.Errorf("the manifest's id %q should be 2–41 lowercase letters, digits or dashes, starting with a letter", a.ID)
	case strings.TrimSpace(a.Name) == "":
		return App{}, fmt.Errorf("the manifest has no name")
	case strings.TrimSpace(a.Compose) == "":
		return App{}, fmt.Errorf("the manifest has no compose template")
	}
	if a.SignIn == "" {
		a.SignIn = SignInGate
	}
	if a.SignIn != SignInGate && a.SignIn != SignInHeaders && a.SignIn != SignInOwn {
		return App{}, fmt.Errorf("signIn is gate, headers or own, not %q", a.SignIn)
	}
	return a, nil
}

// Review checks an app that isn't from the catalog before it's installed. Apps run as root-equivalent containers, so a
// compose file can take over the server; Problems are what Homeport refuses (anything that reaches outside the
// app's own folder and port), Warnings are what the admin should know.
type Review struct {
	Problems []string `json:"problems"`
	Warnings []string `json:"warnings"`
}

func (r *Review) problem(f string, a ...any) { r.Problems = append(r.Problems, fmt.Sprintf(f, a...)) }
func (r *Review) warn(f string, a ...any)    { r.Warnings = append(r.Warnings, fmt.Sprintf(f, a...)) }

// OK says nothing blocks installing it.
func (r Review) OK() bool { return len(r.Problems) == 0 }

// Capabilities that amount to root on the host.
var dangerousCaps = map[string]bool{"ALL": true, "SYS_ADMIN": true, "SYS_MODULE": true, "SYS_PTRACE": true, "SYS_RAWIO": true,
	"DAC_READ_SEARCH": true, "SYS_BOOT": true, "MAC_ADMIN": true, "MAC_OVERRIDE": true, "BPF": true, "PERFMON": true}

// Check reviews a manifest by filling it in with stand-in values and looking at the compose file it makes.
func (a App) Check() Review {
	var r Review
	if a.OIDC != nil {
		r.problem("apps from outside the catalog can't register for \"Sign in with Homeport\" yet (remove oidc)")
	}
	if a.SignIn == SignInHeaders {
		r.warn("signIn is \"headers\": the app will trust Homeport's Remote-User header to sign people in")
	}
	if len(a.AfterInstall) > 0 {
		r.warn("it runs %d command(s) inside the app after installing", len(a.AfterInstall))
	}
	const data = "/hp-data"
	secrets := map[string]string{}
	for _, s := range a.AllSecrets() {
		secrets[s] = "stand-in-secret-value"
	}
	v := Vars{ID: a.ID, Domain: "app.example.test", Port: 18123, Data: data, Secrets: secrets, AdminUser: "admin",
		AdminEmail: "admin@example.test", TimeZone: "UTC", MainDomain: "example.test", Issuer: "https://example.test",
		ClientID: a.ID, CABundle: "/hp-ca/ca-bundle.crt", CALocal: "/hp-ca/ca-local.crt", HostIP: "192.0.2.1"}
	text, err := a.Render(v)
	if err != nil {
		r.problem("%v", err)
		return r
	}
	var file struct {
		Services map[string]map[string]any `yaml:"services"`
		Volumes  map[string]any            `yaml:"volumes"`
		Networks map[string]any            `yaml:"networks"`
		Configs  map[string]any            `yaml:"configs"`
		Secrets  map[string]any            `yaml:"secrets"`
	}
	if err := yaml.Unmarshal([]byte(text), &file); err != nil {
		r.problem("the compose file isn't valid YAML: %v", err)
		return r
	}
	allowedMount := func(src string) bool {
		src = path.Clean(src)
		return src == data || strings.HasPrefix(src, data+"/") || src == v.CABundle || src == v.CALocal
	}
	webPort := false
	names := make([]string, 0, len(file.Services))
	for n := range file.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		svc := file.Services[name]
		at := "service " + name + ": "
		if img, _ := svc["image"].(string); img == "" {
			r.problem("%shas no image (Homeport only runs published images, not builds from source)", at)
		} else if !strings.Contains(path.Base(img), ":") || strings.HasSuffix(img, ":latest") {
			r.warn(at+"%s has no fixed version; updates take whatever is newest", img)
		}
		if _, ok := svc["build"]; ok {
			r.problem("%sbuilds from source; use a published image instead", at)
		}
		if b, _ := svc["privileged"].(bool); b {
			r.problem("%sruns privileged (full control of the server)", at)
		}
		for _, k := range []string{"network_mode", "pid", "ipc", "userns_mode", "uts", "cgroup"} {
			if s, _ := svc[k].(string); s == "host" || (k == "network_mode" && s != "" && s != "bridge" && s != "none" && !strings.HasPrefix(s, "service:")) {
				r.problem(at+"%s: %s shares the server's own %s", k, s, strings.TrimSuffix(k, "_mode"))
			}
		}
		for _, k := range []string{"devices", "device_cgroup_rules", "cgroup_parent", "volumes_from", "env_file", "runtime"} {
			if _, ok := svc[k]; ok {
				r.problem(at+"%s isn't allowed for apps from outside the catalog", k)
			}
		}
		for _, c := range list(svc["cap_add"]) {
			c = strings.TrimPrefix(strings.ToUpper(c), "CAP_")
			if dangerousCaps[c] {
				r.problem(at+"cap_add %s gives control of the server", c)
			} else {
				r.warn(at+"asks for the extra capability %s", c)
			}
		}
		for _, o := range list(svc["security_opt"]) {
			if strings.Contains(o, "unconfined") || strings.Contains(o, "no-new-privileges:false") {
				r.problem(at+"security_opt %s turns off the container's protections", o)
			}
		}
		if s, ok := svc["sysctls"]; ok && s != nil {
			r.warn("%schanges kernel settings (sysctls) inside its container", at)
		}
		for _, p := range ports(svc["ports"]) {
			host, ip := p[0], p[1]
			switch {
			case ip == "127.0.0.1" && host == fmt.Sprint(v.Port):
				webPort = true
			case ip == "127.0.0.1":
				r.problem(at+"port %s: only {{.Port}} is given to the app on 127.0.0.1", host)
			default:
				r.problem(at+"publishes port %s to the network; apps are reached through Homeport (use 127.0.0.1:{{.Port}}:<port>)", cmp.Or(host, "(random)"))
			}
		}
		for _, m := range mounts(svc["volumes"]) {
			src, kind := m[0], m[1]
			switch {
			case kind == "volume" || (kind == "" && src != "" && !strings.ContainsAny(src[:1], "/.~$")):
				// A named volume Docker manages for the app.
			case kind == "tmpfs":
			case src == "":
				// An anonymous volume.
			case allowedMount(src):
			case strings.HasPrefix(src, ".") || strings.HasPrefix(src, "~"):
				r.problem(at+"mounts %s from the repository or a home folder; keep files under {{.Data}}", src)
			default:
				r.problem(at+"mounts %s from the server; only folders under {{.Data}} are allowed", src)
			}
		}
	}
	if len(file.Services) == 0 {
		r.problem("the compose file has no services")
	} else if !webPort {
		r.problem("no service publishes its web page on 127.0.0.1:{{.Port}}, so Homeport can't reach it")
	}
	for n, vol := range file.Volumes {
		if m, ok := vol.(map[string]any); ok {
			if _, ok := m["driver_opts"]; ok {
				r.problem("volume %s uses driver_opts (it can mount anything from the server)", n)
			}
			if ext, _ := m["external"].(bool); ext {
				r.problem("volume %s is external (another app's data)", n)
			}
		}
	}
	for n, nw := range file.Networks {
		if m, ok := nw.(map[string]any); ok {
			if ext, _ := m["external"].(bool); ext {
				r.problem("network %s is external (it would join other apps' networks)", n)
			}
			if d, _ := m["driver"].(string); d == "host" || d == "macvlan" || d == "ipvlan" {
				r.problem("network %s uses the %s driver (straight onto the server's network)", n, d)
			}
		}
	}
	for kind, items := range map[string]map[string]any{"config": file.Configs, "secret": file.Secrets} {
		for n, it := range items {
			if m, ok := it.(map[string]any); ok {
				if f, ok := m["file"].(string); ok && !allowedMount(f) {
					r.problem("%s %s reads %s from the server; only files under {{.Data}} are allowed", kind, n, f)
				}
			}
		}
	}
	return r
}

// list reads a compose list of strings.
func list(v any) []string {
	var out []string
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			out = append(out, fmt.Sprint(x))
		}
	}
	return out
}

// ports reads a service's published ports as (host port, host IP) pairs, from either the short or the long form.
func ports(v any) [][2]string {
	var out [][2]string
	xs, _ := v.([]any)
	for _, x := range xs {
		switch p := x.(type) {
		case map[string]any:
			out = append(out, [2]string{fmt.Sprint(nz(p["published"])), fmt.Sprint(nz(p["host_ip"]))})
		default:
			s := strings.TrimSuffix(strings.TrimSuffix(fmt.Sprint(p), "/tcp"), "/udp")
			parts := strings.Split(s, ":")
			switch {
			case strings.HasPrefix(s, "["): // [::1]:8080:80
				i := strings.Index(s, "]")
				rest := strings.Split(strings.TrimPrefix(s[i+1:], ":"), ":")
				out = append(out, [2]string{rest[0], s[1:i]})
			case len(parts) == 3: // 127.0.0.1:8080:80
				out = append(out, [2]string{parts[1], parts[0]})
			case len(parts) == 2:
				out = append(out, [2]string{parts[0], ""})
			default:
				out = append(out, [2]string{"", ""}) // container port only: Docker picks a host port
			}
		}
	}
	return out
}

func nz(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// mounts reads a service's volumes as (source, type) pairs, from either the short or the long form.
func mounts(v any) [][2]string {
	var out [][2]string
	xs, _ := v.([]any)
	for _, x := range xs {
		switch m := x.(type) {
		case map[string]any:
			out = append(out, [2]string{fmt.Sprint(nz(m["source"])), fmt.Sprint(nz(m["type"]))})
		default:
			parts := strings.Split(fmt.Sprint(m), ":")
			if len(parts) == 1 {
				out = append(out, [2]string{"", ""}) // anonymous
			} else {
				out = append(out, [2]string{parts[0], ""})
			}
		}
	}
	return out
}
