package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/smrit-ai/cli/internal/state"
)

const (
	fileName = "credentials.json"
	lockName = "credentials.lock"
	// fileLockWait bounds the wait for another process's write.
	fileLockWait = 10 * time.Second
)

// File is the fallback store: one JSON object of key to value, mode 0600, in
// a 0700 directory. Writes are atomic and serialised by a flock, so two
// profiles refreshing at once cannot lose each other's update.
type File struct {
	// Dir holds the file. Empty means the state directory.
	Dir string
}

// NewFile returns a file store in dir.
func NewFile(dir string) *File { return &File{Dir: dir} }

func (f *File) dir() (string, error) {
	if f.Dir == "" {
		return state.Ensure()
	}
	if err := state.EnsureDir(f.Dir); err != nil {
		return "", err
	}
	return f.Dir, nil
}

func (f *File) read(dir string) (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, fileName))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("Could not read the credentials file: %w", err)
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New("The credentials file is not readable JSON.")
	}
	return m, nil
}

// Get returns the value under key, or ErrNotFound.
func (f *File) Get(key string) (string, error) {
	dir, err := f.dir()
	if err != nil {
		return "", err
	}
	m, err := f.read(dir)
	if err != nil {
		return "", err
	}
	v, ok := m[key]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (f *File) update(change func(m map[string]string) bool) error {
	dir, err := f.dir()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), fileLockWait)
	defer cancel()
	unlock, err := state.Lock(ctx, filepath.Join(dir, lockName), nil)
	if err != nil {
		return fmt.Errorf("Could not lock the credentials file: %w", err)
	}
	defer unlock()
	m, err := f.read(dir)
	if err != nil {
		return err
	}
	if !change(m) {
		return nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := state.WriteFile(filepath.Join(dir, fileName), data); err != nil {
		return fmt.Errorf("Could not write the credentials file: %w", err)
	}
	return nil
}

// Put stores value under key.
func (f *File) Put(key, value string) error {
	return f.update(func(m map[string]string) bool {
		m[key] = value
		return true
	})
}

// Delete removes key. A missing key is not an error.
func (f *File) Delete(key string) error {
	return f.update(func(m map[string]string) bool {
		if _, ok := m[key]; !ok {
			return false
		}
		delete(m, key)
		return true
	})
}
