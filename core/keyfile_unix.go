//go:build !windows

package core

import (
	"os"
	"syscall"
)

// openKeyFile opens a key file for reading without blocking: opening a FIFO
// would otherwise wait for a writer. O_NOCTTY keeps a terminal device from
// becoming the controlling terminal. The caller checks the file type.
func openKeyFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
}
