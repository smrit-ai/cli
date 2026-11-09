//go:build darwin

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type call struct {
	name  string
	args  []string
	stdin string
}

// recorder is a fake security binary: it records every invocation and
// answers from a canned function.
type recorder struct {
	calls  []call
	answer func(line string) ([]byte, int, error)
}

func (r *recorder) run(_ context.Context, name string, args []string, stdin []byte) ([]byte, int, error) {
	r.calls = append(r.calls, call{name, append([]string(nil), args...), string(stdin)})
	if r.answer == nil {
		return nil, 0, nil
	}
	return r.answer(string(stdin))
}

func TestTokenNeverInArgs(t *testing.T) {
	const secret = "s3cr3tTokenValue_abc"
	r := &recorder{answer: func(line string) ([]byte, int, error) {
		if strings.HasPrefix(line, "find-generic-password") {
			return []byte(secret + "\n"), 0, nil
		}
		return nil, 0, nil
	}}
	k := &Keychain{Run: r.run}
	if err := k.Put("acme:refresh", secret); err != nil {
		t.Fatal(err)
	}
	got, err := k.Get("acme:refresh")
	if err != nil || got != secret {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := k.Delete("acme:refresh"); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 3 {
		t.Fatalf("%d calls, want 3", len(r.calls))
	}
	want := []string{
		"add-generic-password -U -s smrit-cli -a acme:refresh -w " + secret + "\n",
		"find-generic-password -s smrit-cli -a acme:refresh -w\n",
		"delete-generic-password -s smrit-cli -a acme:refresh\n",
	}
	for i, c := range r.calls {
		if c.name != "/usr/bin/security" {
			t.Errorf("call %d ran %q", i, c.name)
		}
		if len(c.args) != 1 || c.args[0] != "-i" {
			t.Errorf("call %d args %q, want exactly [-i]", i, c.args)
		}
		for _, a := range c.args {
			if strings.Contains(a, secret) {
				t.Errorf("call %d carries the secret in its arguments", i)
			}
		}
		if c.stdin != want[i] {
			t.Errorf("call %d stdin %q, want %q", i, c.stdin, want[i])
		}
	}
}

func TestKeychainRefusesUnsafeText(t *testing.T) {
	for _, bad := range []string{`a"b`, `a'b`, `a\b`, "a\nb", "a b", "a\rb", "a\tb", ""} {
		r := &recorder{}
		k := &Keychain{Run: r.run}
		if err := k.Put("acme:refresh", bad); err == nil {
			t.Errorf("Put accepted value %q", bad)
		}
		if err := k.Put(bad, "value"); err == nil {
			t.Errorf("Put accepted key %q", bad)
		}
		if _, err := k.Get(bad); err == nil {
			t.Errorf("Get accepted key %q", bad)
		}
		if len(r.calls) != 0 {
			t.Errorf("security ran for %q", bad)
		}
	}
}

func TestKeychainNotFoundAndFailure(t *testing.T) {
	notFound := &Keychain{Run: (&recorder{answer: func(string) ([]byte, int, error) {
		return nil, 44, nil
	}}).run}
	if _, err := notFound.Get("acme:refresh"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("status 44: %v, want ErrNotFound", err)
	}
	if err := notFound.Delete("acme:refresh"); err != nil {
		t.Fatalf("deleting a missing item: %v", err)
	}

	locked := &Keychain{Run: (&recorder{answer: func(string) ([]byte, int, error) {
		return nil, 36, nil
	}}).run}
	_, err := locked.Get("acme:refresh")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("status 36: %v, want an error that is not ErrNotFound", err)
	}

	cannotRun := &Keychain{Run: (&recorder{answer: func(string) ([]byte, int, error) {
		return nil, -1, errors.New("exec: no such file")
	}}).run}
	if _, err := cannotRun.Get("acme:refresh"); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("cannot run: %v, want an error that is not ErrNotFound", err)
	}
	if err := cannotRun.Put("acme:refresh", "value"); err == nil {
		t.Fatal("Put succeeded although security could not run")
	}
}
