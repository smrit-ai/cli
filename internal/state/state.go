// Package state is where the CLI keeps what it remembers between runs: the
// profiles, the fallback credentials file and the refresh locks.
package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Dir is the state directory: ~/Library/Application Support/smrit on macOS,
// ${XDG_STATE_HOME:-~/.local/state}/smrit elsewhere. It may not exist yet.
func Dir() (string, error) {
	if runtime.GOOS != "darwin" {
		// The XDG specification ignores a relative path, and so does this.
		if x := os.Getenv("XDG_STATE_HOME"); x != "" && filepath.IsAbs(x) {
			return filepath.Join(x, "smrit"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errors.New("Could not find your home directory to keep the sign in.")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "smrit"), nil
	}
	return filepath.Join(home, ".local", "state", "smrit"), nil
}

// Ensure returns the state directory, creating it with mode 0700 when it is
// missing and narrowing it to 0700 when it is not.
func Ensure() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := EnsureDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// EnsureDir makes dir, mode 0700, with its parents.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("Could not create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("Could not make %s private: %w", dir, err)
	}
	return nil
}

// WriteFile replaces path with data atomically. The new file is created with
// mode 0600 by the open itself, never widened and narrowed afterwards, so
// there is no moment at which another user could read it.
func WriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	// CreateTemp opens with 0600 and O_EXCL; check rather than assume.
	if st, err := tmp.Stat(); err != nil || st.Mode().Perm()&0o077 != 0 {
		return errors.New("Could not create a private file in the state directory.")
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}
