//go:build darwin

package platform

import (
	"fmt"
	"os"
	"path/filepath"
)

// On a Mac everything belongs to the signed-in user (launchd agents): macOS lets any user serve on ports 80 and 443,
// and the Docker runtime (OrbStack, Colima, Docker Desktop) is per user anyway.
func current() Platform {
	home, _ := os.UserHomeDir()
	return withStateDir(Platform{
		Name:      "macos",
		SystemCA:  "/etc/ssl/cert.pem",
		CaddyRoot: filepath.Join(home, "Library/Application Support/Caddy/pki/authorities/local/root.crt"),
		Health: [][]string{{"uptime"}, {"memory_pressure", "-Q"}, {"df", "-h", "/", "/System/Volumes/Data"},
			{"docker", "system", "df"}},
		Admin:       "homeport",
		ServiceHint: "launchctl print gui/$(id -u)/dev.homeport.homeport",
	}, filepath.Join(home, "Library/Application Support/Homeport"))
}

func withStateDir(p Platform, d string) Platform {
	p.StateDir = d
	p.Socket = filepath.Join(d, "homeport.sock")
	// macOS allows socket paths of about 104 bytes; past that, use the user's private temporary folder.
	if len(p.Socket) > 100 {
		p.Socket = filepath.Join(os.TempDir(), fmt.Sprintf("homeport-%d.sock", os.Getuid()))
	}
	p.Caddyfile = filepath.Join(d, "Caddyfile")
	p.ReloadCaddy = []string{"caddy", "reload", "--config", p.Caddyfile, "--adapter", "caddyfile", "--address", "localhost:2019"}
	logs := filepath.Join(d, "logs")
	p.LogsCommand = func(svc string, n int) []string {
		if svc == "docker" {
			return []string{"docker", "info", "--format", "{{.Name}} {{.ServerVersion}}: {{.Containers}} containers, {{.ContainersRunning}} running"}
		}
		return []string{"tail", "-n", fmt.Sprint(n), filepath.Join(logs, svc+".log")}
	}
	return p
}
