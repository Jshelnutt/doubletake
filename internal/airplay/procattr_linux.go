//go:build linux

package airplay

import (
	"os/exec"
	"syscall"
)

func setChildDeathSignal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}

func hiddenWindowAttr() *syscall.SysProcAttr { return nil }
