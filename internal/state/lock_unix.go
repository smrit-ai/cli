//go:build unix

package state

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// ErrBusy is returned when the lock is still held by someone else when the
// context ends.
var ErrBusy = errors.New("Another sign in or refresh is still running.")

// lockPoll is how often a waiting taker tries the lock again. A blocking
// flock cannot be abandoned when the context ends, so the wait polls.
const lockPoll = 20 * time.Millisecond

// Lock takes an exclusive flock on path, creating the file with mode 0600,
// and waits for it until ctx ends. busy, when not nil, is called once if the
// lock was found held. flock binds the lock to the open file, so two takers
// in one process exclude each other exactly as two processes do.
func Lock(ctx context.Context, path string, busy func()) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())
	told := false
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				syscall.Flock(fd, syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		if !told && busy != nil {
			told = true
			busy()
		}
		timer := time.NewTimer(lockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ErrBusy
		case <-timer.C:
		}
	}
}
