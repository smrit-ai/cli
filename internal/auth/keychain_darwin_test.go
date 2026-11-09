//go:build darwin

package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/smrit-ai/cli/internal/store"
)

// fakeSecurity stands in for /usr/bin/security -i: it keeps items in a map,
// records every invocation, and fails the commands `fail` picks.
type fakeSecurity struct {
	mu    sync.Mutex
	items map[string]string
	calls []securityCall
	fail  func(line string) (status int, err error)
}

type securityCall struct {
	name  string
	args  []string
	stdin string
}

func (s *fakeSecurity) run(_ context.Context, name string, args []string, stdin []byte) ([]byte, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := string(stdin)
	s.calls = append(s.calls, securityCall{name, append([]string(nil), args...), line})
	if s.fail != nil {
		if status, err := s.fail(line); err != nil || status != 0 {
			return nil, status, err
		}
	}
	f := strings.Fields(line)
	arg := func(flag string) string {
		for i := 0; i+1 < len(f); i++ {
			if f[i] == flag {
				return f[i+1]
			}
		}
		return ""
	}
	if s.items == nil {
		s.items = map[string]string{}
	}
	switch f[0] {
	case "add-generic-password":
		s.items[arg("-a")] = arg("-w")
		return nil, 0, nil
	case "find-generic-password":
		v, ok := s.items[arg("-a")]
		if !ok {
			return nil, 44, nil
		}
		return []byte(v + "\n"), 0, nil
	case "delete-generic-password":
		if _, ok := s.items[arg("-a")]; !ok {
			return nil, 44, nil
		}
		delete(s.items, arg("-a"))
		return nil, 0, nil
	}
	return nil, 1, nil
}

func TestKeychainFailureIsUnavailable(t *testing.T) {
	cases := map[string]func(line string) (int, error){
		"keychain locked": func(line string) (int, error) {
			if strings.HasPrefix(line, "find-generic-password") && strings.Contains(line, "acme:refresh") {
				return 36, nil
			}
			return 0, nil
		},
		"interaction not allowed": func(line string) (int, error) {
			if strings.HasPrefix(line, "find-generic-password") {
				return 51, nil
			}
			return 0, nil
		},
		"cannot run security": func(string) (int, error) {
			return -1, errors.New("fork/exec /usr/bin/security: resource temporarily unavailable")
		},
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			setHome(t)
			f := newFake(t)
			sec := &fakeSecurity{}
			c := &Client{Server: f.srv.URL, Store: &store.Keychain{Run: sec.run}, HTTP: f.srv.Client()}
			p := seed(t, c, f)
			sec.fail = fail

			_, err := c.AccessToken(testContext(t), p)
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("AccessToken: %v, want ErrUnavailable", err)
			}
			if errors.Is(err, ErrNotSignedIn) {
				t.Fatal("a keychain failure reads as signed out")
			}
			if calls, _ := f.counts(); calls != 0 {
				t.Fatalf("%d token requests", calls)
			}
			sec.mu.Lock()
			defer sec.mu.Unlock()
			if sec.items["acme:refresh"] != "refresh-0-y" {
				t.Fatal("the refresh token is gone")
			}
			for _, call := range sec.calls {
				if strings.HasPrefix(call.stdin, "delete-generic-password") {
					t.Fatal("a keychain failure deleted an item")
				}
			}
		})
	}
}

func TestTokenNeverInArgs(t *testing.T) {
	setHome(t)
	f := newFake(t)
	sec := &fakeSecurity{}
	c := &Client{Server: f.srv.URL, Store: &store.Keychain{Run: sec.run}, HTTP: f.srv.Client()}
	ctx := testContext(t)

	p, err := c.Login(ctx, LoginOptions{OpenBrowser: f.browser, Verify: verifyAs("acme", testOrgID)})
	if err != nil {
		t.Fatal(err)
	}
	// Expire the access token so the next call refreshes through the keychain.
	if err := c.Store.Put(accessKey("acme"), "100:"+f.seen().accessTokens[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AccessToken(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := c.Logout(ctx, p); err != nil {
		t.Fatal(err)
	}

	seen := f.seen()
	secrets := append(seen.accessTokens, seen.refreshes...)
	if len(secrets) < 4 {
		t.Fatalf("expected a sign in and a refresh, got tokens %d", len(secrets))
	}
	sec.mu.Lock()
	defer sec.mu.Unlock()
	written := 0
	for i, call := range sec.calls {
		if call.name != "/usr/bin/security" || len(call.args) != 1 || call.args[0] != "-i" {
			t.Errorf("call %d ran %q %q, want /usr/bin/security -i", i, call.name, call.args)
		}
		for _, a := range call.args {
			for _, s := range secrets {
				if strings.Contains(a, s) {
					t.Errorf("call %d carries a token in its arguments", i)
				}
			}
		}
		for _, s := range secrets {
			if strings.Contains(call.stdin, s) {
				written++
			}
		}
	}
	if written == 0 {
		t.Fatal("no token went through the keychain at all")
	}
}
