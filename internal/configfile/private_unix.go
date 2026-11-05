//go:build !windows

package configfile

import (
	"errors"
	"os"
)

func ProtectFile(f *os.File) error {
	return f.Chmod(0600)
}

func PrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return errors.New("session-dir must be a private directory (mode 0700)")
	}
	return nil
}
