//go:build !linux && !windows

package airplay

import "syscall"

func hiddenWindowAttr() *syscall.SysProcAttr { return nil }
