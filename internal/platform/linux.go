//go:build !darwin

package platform

import "fmt"

func current() Platform {
	return Platform{
		Name:        "linux",
		StateDir:    "/var/lib/homeport",
		Socket:      "/run/homeport/homeport.sock",
		Caddyfile:   "/etc/caddy/Caddyfile",
		SystemCA:    "/etc/ssl/certs/ca-certificates.crt",
		CaddyRoot:   "/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt",
		ReloadCaddy: []string{"systemctl", "reload", "caddy"},
		Health: [][]string{{"uptime"}, {"free", "-h"}, {"df", "-h", "-x", "tmpfs", "-x", "overlay"},
			{"docker", "system", "df"}, {"systemctl", "--failed", "--no-legend", "--no-pager"}},
		LogsCommand: func(svc string, n int) []string {
			return []string{"journalctl", "-u", svc, "-n", fmt.Sprint(n), "--no-pager", "-o", "short-iso"}
		},
		Admin:       "sudo homeport",
		ServiceHint: "systemctl status homeport",
	}
}

func withStateDir(p Platform, d string) Platform {
	p.StateDir = d
	p.Socket = d + "/homeport.sock"
	return p
}
