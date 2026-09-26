//go:build !windows

package main

import (
	"context"
	"os/exec"
	"syscall"
)

func shellCommand(ctx context.Context, c string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", c)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	return cmd
}
