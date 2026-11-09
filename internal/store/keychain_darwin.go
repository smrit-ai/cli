//go:build darwin

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	// SecurityPath is the macOS keychain tool. Only this absolute path is run.
	SecurityPath = "/usr/bin/security"
	// Service names every item the CLI keeps in the keychain.
	Service = "smrit-cli"
	// statusNotFound is what security exits with when no item matches
	// (errSecItemNotFound, -25300).
	statusNotFound = 44
	keychainWait   = 15 * time.Second
)

// Runner runs a program with stdin and returns its stdout and exit status.
// err is set only when the program could not be run at all.
type Runner func(ctx context.Context, name string, args []string, stdin []byte) (stdout []byte, status int, err error)

// RunCommand is the real Runner. Stderr is discarded: what security writes
// there is not shown to anyone.
func RunCommand(ctx context.Context, name string, args []string, stdin []byte) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() >= 0 {
		return out.Bytes(), exit.ExitCode(), nil
	}
	if err != nil {
		return nil, -1, err
	}
	return out.Bytes(), 0, nil
}

// Keychain keeps each value as a generic password in the login keychain,
// service smrit-cli, account the key. It runs `security -i` and writes the
// command to its standard input, so a value is never a process argument and
// never shows in a process listing.
type Keychain struct {
	// Run runs security. Nil means RunCommand.
	Run Runner
}

// KeychainError is a keychain that answered with a failure other than "no
// such item": locked, denied, or not runnable. It says nothing about whether
// a sign in exists.
type KeychainError struct {
	Op     string
	Status int
	Err    error
}

func (e *KeychainError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("The keychain could not %s: security could not be run.", e.Op)
	}
	return fmt.Sprintf("The keychain could not %s (security exited with status %d).", e.Op, e.Status)
}

func (e *KeychainError) Unwrap() error { return e.Err }

// errUnsafe refuses text the security command line cannot carry as one word.
var errUnsafe = errors.New("That text cannot be kept in the keychain: it is empty or holds a quote, a backslash, a space or a line break.")

// safe reports whether s can be written as one word on a security -i line:
// security splits that line on blanks and reads quotes and backslashes, so
// any of them would change the command rather than the value.
func safe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f || r == '"' || r == '\'' || r == '\\' {
			return false
		}
	}
	return true
}

func (k *Keychain) run(op, line string) ([]byte, int, error) {
	run := k.Run
	if run == nil {
		run = RunCommand
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainWait)
	defer cancel()
	out, status, err := run(ctx, SecurityPath, []string{"-i"}, []byte(line+"\n"))
	if err != nil {
		return nil, status, &KeychainError{Op: op, Status: status, Err: err}
	}
	return out, status, nil
}

// Get returns the value under key, or ErrNotFound.
func (k *Keychain) Get(key string) (string, error) {
	if !safe(key) {
		return "", errUnsafe
	}
	out, status, err := k.run("read the sign in", "find-generic-password -s "+Service+" -a "+key+" -w")
	if err != nil {
		return "", err
	}
	if status == statusNotFound {
		return "", ErrNotFound
	}
	if status != 0 {
		return "", &KeychainError{Op: "read the sign in", Status: status}
	}
	v := strings.TrimSuffix(string(out), "\n")
	if !safe(v) {
		return "", &KeychainError{Op: "read the sign in", Status: status}
	}
	return v, nil
}

// Put stores value under key, replacing any value already there.
func (k *Keychain) Put(key, value string) error {
	if !safe(key) || !safe(value) {
		return errUnsafe
	}
	_, status, err := k.run("keep the sign in", "add-generic-password -U -s "+Service+" -a "+key+" -w "+value)
	if err != nil {
		return err
	}
	if status != 0 {
		return &KeychainError{Op: "keep the sign in", Status: status}
	}
	return nil
}

// Delete removes key. A missing item is not an error.
func (k *Keychain) Delete(key string) error {
	if !safe(key) {
		return errUnsafe
	}
	_, status, err := k.run("forget the sign in", "delete-generic-password -s "+Service+" -a "+key)
	if err != nil {
		return err
	}
	if status != 0 && status != statusNotFound {
		return &KeychainError{Op: "forget the sign in", Status: status}
	}
	return nil
}
