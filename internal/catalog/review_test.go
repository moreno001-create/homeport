package catalog

import (
	"strings"
	"testing"
)

const goodManifest = `id: notes
name: Notes
description: A notes app.
signIn: gate
secrets: [db_password]
compose: |
  services:
    db:
      image: docker.io/library/postgres:17
      environment:
        POSTGRES_PASSWORD: "{{secret "db_password"}}"
      volumes: ["{{.Data}}/db:/var/lib/postgresql/data"]
    app:
      image: ghcr.io/example/notes:1.2
      ports: ["127.0.0.1:{{.Port}}:8080"]
      volumes:
        - cache:/cache
        - type: bind
          source: "{{.Data}}/files"
          target: /files
  volumes:
    cache: {}
`

func TestCheckPassesASafeApp(t *testing.T) {
	a, err := Parse(goodManifest)
	if err != nil {
		t.Fatal(err)
	}
	if r := a.Check(); !r.OK() || len(r.Warnings) != 0 {
		t.Fatalf("a safe app should pass cleanly: %+v", r)
	}
}

func TestCheckRefusesWhatReachesOutside(t *testing.T) {
	cases := map[string][2]string{
		"privileged":     {"      ports: [\"127.0.0.1:{{.Port}}:8080\"]\n", "      ports: [\"127.0.0.1:{{.Port}}:8080\"]\n      privileged: true\n"},
		"host network":   {"      ports: [\"127.0.0.1:{{.Port}}:8080\"]\n", "      ports: [\"127.0.0.1:{{.Port}}:8080\"]\n      network_mode: host\n"},
		"docker socket":  {"        - cache:/cache\n", "        - /var/run/docker.sock:/var/run/docker.sock\n"},
		"server root":    {"source: \"{{.Data}}/files\"", "source: /"},
		"data escape":    {"\"{{.Data}}/db:", "\"{{.Data}}/../../etc:"},
		"repo files":     {"        - cache:/cache\n", "        - ./config:/config\n"},
		"public port":    {"127.0.0.1:{{.Port}}:8080", "8080:8080"},
		"extra port":     {"[\"127.0.0.1:{{.Port}}:8080\"]", "[\"127.0.0.1:{{.Port}}:8080\", \"0.0.0.0:53:53/udp\"]"},
		"no web port":    {"[\"127.0.0.1:{{.Port}}:8080\"]", "[]"},
		"build":          {"      image: ghcr.io/example/notes:1.2\n", "      build: .\n"},
		"sys_admin":      {"      ports: [\"127.0.0.1:{{.Port}}:8080\"]\n", "      ports: [\"127.0.0.1:{{.Port}}:8080\"]\n      cap_add: [SYS_ADMIN]\n"},
		"devices":        {"      ports: [\"127.0.0.1:{{.Port}}:8080\"]\n", "      ports: [\"127.0.0.1:{{.Port}}:8080\"]\n      devices: [/dev/sda]\n"},
		"driver_opts":    {"    cache: {}\n", "    cache:\n      driver_opts: {type: none, o: bind, device: /etc}\n"},
		"external net":   {"    cache: {}\n", "    cache: {}\n  networks:\n    other:\n      external: true\n"},
		"unknown secret": {"\"{{secret \"db_password\"}}\"", "\"{{secret \"nope\"}}\""},
		"unconfined":     {"      ports: [\"127.0.0.1:{{.Port}}:8080\"]\n", "      ports: [\"127.0.0.1:{{.Port}}:8080\"]\n      security_opt: [\"seccomp:unconfined\"]\n"},
	}
	for name, c := range cases {
		if !strings.Contains(goodManifest, c[0]) {
			t.Fatalf("%s: test text %q not in the manifest", name, c[0])
		}
		a, err := Parse(strings.Replace(goodManifest, c[0], c[1], 1))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r := a.Check(); r.OK() {
			t.Errorf("%s should be refused", name)
		}
	}
}

func TestCheckWarns(t *testing.T) {
	a, _ := Parse(strings.Replace(strings.Replace(goodManifest, "notes:1.2", "notes:latest", 1),
		"      ports: [\"127.0.0.1:{{.Port}}:8080\"]\n", "      ports: [\"127.0.0.1:{{.Port}}:8080\"]\n      cap_add: [NET_ADMIN]\n", 1))
	r := a.Check()
	if !r.OK() || len(r.Warnings) != 2 {
		t.Fatalf("want two warnings (no fixed version, NET_ADMIN) and no problems: %+v", r)
	}
}

func TestParse(t *testing.T) {
	for name, text := range map[string]string{
		"bad id":        strings.Replace(goodManifest, "id: notes", "id: Notes!", 1),
		"no compose":    "id: notes\nname: Notes\n",
		"unknown field": goodManifest + "rootAccess: true\n",
		"bad signIn":    strings.Replace(goodManifest, "signIn: gate", "signIn: magic", 1),
	} {
		if _, err := Parse(text); err == nil {
			t.Errorf("%s: should fail", name)
		}
	}
}
