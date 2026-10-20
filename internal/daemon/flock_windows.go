//go:build windows

package daemon

import "os"

// Windows: daemon mode is not used by doubletake-win; no advisory lock.
func lockFileExclusive(f *os.File) error { return nil }

func unlockFile(f *os.File) {}
