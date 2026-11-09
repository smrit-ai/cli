package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestFileStoreIs0600 proves the fallback file is private from the moment it
// exists: the umask is opened wide, so a file created with looser bits and
// tightened afterwards would show them here.
func TestFileStoreIs0600(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	dir := filepath.Join(t.TempDir(), "smrit")
	s := NewFile(dir)
	if err := s.Put("acme:refresh", "refresh-one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("acme:access", "100:access-one"); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(filepath.Join(dir, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("credentials file mode %o, want 600", got)
	}
	dst, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dst.Mode().Perm(); got != 0o700 {
		t.Fatalf("state directory mode %o, want 700", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	s := NewFile(t.TempDir())
	if _, err := s.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get on an empty store: %v, want ErrNotFound", err)
	}
	if err := s.Put("k", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("k", "v2"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get("k"); err != nil || v != "v2" {
		t.Fatalf("Get = %q, %v; want v2", v, err)
	}
	if err := s.Delete("k"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("k"); err != nil {
		t.Fatalf("deleting a missing key: %v", err)
	}
	if _, err := s.Get("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: %v, want ErrNotFound", err)
	}
}
