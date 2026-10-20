//go:build !linux

package airplay

import "os/exec"

// setChildDeathSignal has no Pdeathsig equivalent outside Linux. On Windows
// the child gets no console window; exec.CommandContext still kills it on
// cancellation, and the capture Stop methods kill it explicitly.
func setChildDeathSignal(cmd *exec.Cmd) {
	cmd.SysProcAttr = hiddenWindowAttr()
}
