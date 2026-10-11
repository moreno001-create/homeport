// Package terminal runs the admin panel's web terminal: ttyd on the loopback interface, showing the system's own login
// prompt, reached through Caddy at /terminal/ on the main domain after Homeport checks the browser belongs to an
// admin. So it takes a Homeport admin session and a Linux account's password. Linux only.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"homeport/internal/runner"
)

const (
	// Port is where ttyd listens, on 127.0.0.1 only (not 7681: Ubuntu's own ttyd service uses that).
	Port = 7682
	// Path is where it's served on the main domain.
	Path = "/terminal"

	unitPath = "/etc/systemd/system/homeport-terminal.service"
	unitName = "homeport-terminal.service"
)

// -i lo: loopback only; -O: refuse WebSocket connections from other sites; -W: typing allowed; login: the system's own
// sign-in, so a Homeport session alone doesn't give a shell.
const unit = `# Written by Homeport: the admin panel's web terminal. Turn it on or off with "homeport terminal on|off".
[Unit]
Description=Homeport web terminal
After=network.target

[Service]
ExecStart=/usr/bin/ttyd -i lo -p %d -b %s -O -W -m 4 -t fontSize=15 -t "titleFixed=Homeport terminal" /bin/login
Restart=on-failure

[Install]
WantedBy=multi-user.target
`

// Set turns the terminal on (installing ttyd when it's missing) or off.
func Set(ctx context.Context, r runner.Runner, on bool) error {
	if !on {
		if _, err := os.Stat(unitPath); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if out, err := r.Output(ctx, "", "systemctl", "disable", "--now", unitName); err != nil {
			return fmt.Errorf("couldn't stop the terminal: %v\n%s", err, out)
		}
		return nil
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("the web terminal needs Linux with systemd")
	}
	if _, err := os.Stat("/usr/bin/ttyd"); err != nil {
		if out, err := r.Output(ctx, "", "env", "DEBIAN_FRONTEND=noninteractive", "apt-get", "install", "-y", "-q", "ttyd"); err != nil {
			return fmt.Errorf("couldn't install ttyd: %v\n%s", err, out)
		}
	}
	// The package starts its own ttyd (a login prompt on 7681); Homeport's replaces it.
	_, _ = r.Output(ctx, "", "systemctl", "disable", "--now", "ttyd.service")
	if err := os.WriteFile(unitPath, []byte(fmt.Sprintf(unit, Port, Path)), 0o644); err != nil {
		return err
	}
	_ = os.Chmod(unitPath, 0o644)
	for _, args := range [][]string{{"daemon-reload"}, {"enable", unitName}, {"restart", unitName}} {
		if out, err := r.Output(ctx, "", "systemctl", args...); err != nil {
			return fmt.Errorf("couldn't start the terminal (systemctl %s): %v\n%s", args[0], err, out)
		}
	}
	return nil
}
