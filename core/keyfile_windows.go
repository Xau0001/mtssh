//go:build windows

package core

import "os"

// openKeyFile opens a key file for reading. The caller checks the file type.
func openKeyFile(path string) (*os.File, error) {
	return os.Open(path)
}
