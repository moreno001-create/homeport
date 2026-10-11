// Package runner runs system commands (docker, caddy, systemctl). Tests swap in a fake.
package runner

import (
	"bytes"
	"context"
	"io"
	"os/exec"
)

type Runner interface {
	// Output runs a command and returns what it printed (stdout and stderr together).
	Output(ctx context.Context, dir, name string, args ...string) (string, error)
	// Stream runs a command, copying what it prints to w as it goes (for install logs).
	Stream(ctx context.Context, dir string, w io.Writer, name string, args ...string) error
}

// System runs real commands.
type System struct{}

func (System) Output(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return buf.String(), err
}

func (System) Stream(ctx context.Context, dir string, w io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}
