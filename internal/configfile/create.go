// Package configfile provides private, non-overwriting configuration creation.
package configfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	if err = f.Chmod(0600); err == nil {
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
	dir, err := os.Open(filepath.Dir(path))
	if err == nil {
		err = dir.Sync()
		_ = dir.Close()
	}
	if err != nil {
		return true, fmt.Errorf("cannot synchronize configuration directory: %w", err)
	}
	return true, nil
}
