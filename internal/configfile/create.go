// Package configfile provides private, non-overwriting configuration creation.
package configfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// CreatePrivate publishes a complete file atomically. Concurrent initializers
// never truncate or replace a file created by another process.
func CreatePrivate(path string, data []byte) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, fmt.Errorf("cannot create configuration directory: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".sgate-config-*")
	if err != nil {
		return false, fmt.Errorf("cannot prepare configuration file: %w", err)
	}
	defer os.Remove(f.Name())
	if err = ProtectFile(f); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return false, fmt.Errorf("cannot write configuration file: %w", err)
	}
	if err = os.Link(f.Name(), path); errors.Is(err, os.ErrExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("cannot create configuration file: %w", err)
	}
	if err := SyncDirectory(filepath.Dir(path)); err != nil {
		return true, fmt.Errorf("cannot synchronize configuration directory: %w", err)
	}
	return true, nil
}

// SyncDirectory flushes directory metadata where the OS supports it.
func SyncDirectory(path string) error {
	// Windows does not support FlushFileBuffers on directory handles.
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
