// Package platform holds what differs between the systems Homeport runs on: where its files live, how Caddy is
// reloaded, and how health and logs are read. Linux (a server, everything as root under systemd) and macOS (a Mac,
// everything as the signed-in user under launchd) each fill it in.
package platform

import "os"

type Platform struct {
	Name string // "linux" or "macos"
	// StateDir holds state.json, the apps' folders, keys, the audit log and the assistants' memory.
	StateDir string
	// Socket is the local socket the `homeport` command uses (only its owner can open it).
	Socket string
	// Caddyfile is the Caddy configuration Homeport writes.
	Caddyfile string
	// SystemCA is the system's trusted certificates; CaddyRoot is Caddy's own root (used with --tls internal).
	SystemCA, CaddyRoot string
	// ReloadCaddy is the command that makes Caddy load the new configuration.
	ReloadCaddy []string
	// Health lists the commands whose output describes the machine's health.
	Health [][]string
	// LogsCommand returns the command that prints a service's recent log lines (homeport, caddy or docker).
	LogsCommand func(service string, lines int) []string
	// Admin says how to run admin commands here ("sudo homeport" on Linux, "homeport" on a Mac).
	Admin string
	// ServiceHint says how to check the service is running.
	ServiceHint string
}

// Current is the platform Homeport is running on. HOMEPORT_STATE_DIR moves the state (tests, a second instance).
func Current() Platform {
	p := current()
	if d := os.Getenv("HOMEPORT_STATE_DIR"); d != "" {
		p = withStateDir(p, d)
	}
	return p
}
