package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".local", "state", "smrit")
	if runtime.GOOS == "darwin" {
		want = filepath.Join(home, "Library", "Application Support", "smrit")
	}
	if got != want {
		t.Fatalf("Dir = %q, want %q", got, want)
	}
	if runtime.GOOS != "darwin" {
		xdg := t.TempDir()
		t.Setenv("XDG_STATE_HOME", xdg)
		if got, _ := Dir(); got != filepath.Join(xdg, "smrit") {
			t.Fatalf("Dir with XDG_STATE_HOME = %q", got)
		}
	}
}

func TestEnsureMakesA0700Directory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", "")
	dir, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("mode %o, want 700", st.Mode().Perm())
	}
}

func TestLockIsExclusiveAndGivesUpAtTheDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Lock(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	busy := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := Lock(ctx, path, func() { close(busy) })
		result <- err
	}()
	<-busy // the second taker found the lock held
	cancel()
	if err := <-result; !errors.Is(err, ErrBusy) {
		t.Fatalf("second Lock: %v, want ErrBusy", err)
	}
	unlock()
	unlock2, err := Lock(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("Lock after unlock: %v", err)
	}
	unlock2()
}
