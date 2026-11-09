package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/smrit-ai/cli/internal/profile"
	"github.com/smrit-ai/cli/internal/store"
)

// countingBrowser wraps the fake's browser and counts how often it was asked
// to open anything, keeping the raw URLs.
type countingBrowser struct {
	mu   sync.Mutex
	urls []string
	f    *fakeOAuth
}

func (b *countingBrowser) open(u string) error {
	b.mu.Lock()
	b.urls = append(b.urls, u)
	b.mu.Unlock()
	return b.f.browser(u)
}

func (b *countingBrowser) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.urls)
}

// TestRedirectsAreNeverFollowed: a 307 or 308 makes Go resend a POST body to
// the new location, so following one would hand a refresh token, a code or a
// revocation to another host. Every OAuth request stops at the redirect.
func TestRedirectsAreNeverFollowed(t *testing.T) {
	clients := map[string]func(f *fakeOAuth) *http.Client{
		"default client":  func(*fakeOAuth) *http.Client { return nil },
		"injected client": func(f *fakeOAuth) *http.Client { return f.srv.Client() },
	}
	for name, httpClient := range clients {
		t.Run("token "+name, func(t *testing.T) {
			setHome(t)
			f := newFake(t)
			e := newElsewhere(t)
			c := &Client{Server: f.srv.URL, Store: store.NewFile(t.TempDir()), HTTP: httpClient(f)}
			p := seed(t, c, f)
			f.set(func(f *fakeOAuth) { f.redirect = map[string]string{"/oauth/token": e.srv.URL + "/steal"} })

			_, err := c.AccessToken(testContext(t), p)
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("AccessToken: %v, want ErrUnavailable", err)
			}
			if n := e.count(); n != 0 {
				t.Fatalf("the redirect target received %d requests", n)
			}
			if got, _ := c.Store.Get(refreshKey("acme")); got != "refresh-0-y" {
				t.Fatal("refresh token not kept")
			}
		})
	}
	t.Run("revoke", func(t *testing.T) {
		setHome(t)
		f := newFake(t)
		e := newElsewhere(t)
		c := newClient(t, f)
		p := seed(t, c, f)
		f.set(func(f *fakeOAuth) { f.redirect = map[string]string{"/oauth/revoke": e.srv.URL + "/steal"} })
		if err := c.Logout(testContext(t), p); !errors.Is(err, ErrNotRevoked) {
			t.Fatalf("Logout: %v, want ErrNotRevoked", err)
		}
		if n := e.count(); n != 0 {
			t.Fatalf("the redirect target received %d requests", n)
		}
	})
	for _, path := range []string{"/oauth/register", "/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/mcp"} {
		t.Run("login "+path, func(t *testing.T) {
			setHome(t)
			f := newFake(t)
			e := newElsewhere(t)
			c := newClient(t, f)
			b := &countingBrowser{f: f}
			f.set(func(f *fakeOAuth) { f.redirect = map[string]string{path: e.srv.URL + path} })
			_, err := c.Login(testContext(t), LoginOptions{OpenBrowser: b.open, Verify: verifyAs("acme", testOrgID)})
			if err == nil {
				t.Fatal("Login followed a redirect and succeeded")
			}
			if n := e.count(); n != 0 {
				t.Fatalf("the redirect target received %d requests", n)
			}
			if b.count() != 0 {
				t.Fatal("the browser was opened")
			}
		})
	}
}

func TestDiscoveryOffServerIsRefusedBeforeTheBrowser(t *testing.T) {
	for _, key := range []string{"authorization_endpoint", "token_endpoint", "registration_endpoint"} {
		t.Run(key, func(t *testing.T) {
			setHome(t)
			f := newFake(t)
			e := newElsewhere(t)
			c := newClient(t, f)
			b := &countingBrowser{f: f}
			f.set(func(f *fakeOAuth) { f.meta = map[string]string{key: e.srv.URL + "/oauth/x"} })
			_, err := c.Login(testContext(t), LoginOptions{OpenBrowser: b.open, Verify: verifyAs("acme", testOrgID)})
			if !errors.Is(err, ErrLoginFailed) {
				t.Fatalf("Login: %v, want ErrLoginFailed", err)
			}
			if b.count() != 0 || e.count() != 0 {
				t.Fatalf("browser opened %d times, other host reached %d times", b.count(), e.count())
			}
		})
	}
}

func TestResourceMustBeTheServersOwn(t *testing.T) {
	cases := map[string]func(f *fakeOAuth, e *elsewhere) func(string) string{
		"another host": func(_ *fakeOAuth, e *elsewhere) func(string) string {
			return func(path string) string { return e.srv.URL + path }
		},
		"another path": func(f *fakeOAuth, _ *elsewhere) func(string) string {
			return func(string) string { return f.srv.URL + "/mcp/o/other/" + testOrgID }
		},
	}
	for name, resource := range cases {
		t.Run(name, func(t *testing.T) {
			setHome(t)
			f := newFake(t)
			e := newElsewhere(t)
			c := newClient(t, f)
			b := &countingBrowser{f: f}
			f.set(func(f *fakeOAuth) { f.resourceFor = resource(f, e) })
			_, err := c.Login(testContext(t), LoginOptions{OpenBrowser: b.open, Verify: verifyAs("acme", testOrgID)})
			if !errors.Is(err, ErrLoginFailed) {
				t.Fatalf("Login: %v, want ErrLoginFailed", err)
			}
			if b.count() != 0 {
				t.Fatal("the browser was opened")
			}
		})
	}
}

func TestCallbackWithTheWrongStateIsIgnored(t *testing.T) {
	setHome(t)
	f := newFake(t)
	c := newClient(t, f)
	wrongStatus := 0
	browser := func(authURL string) error {
		u, _ := url.Parse(authURL)
		back := u.Query().Get("redirect_uri") + "?" + url.Values{"code": {"forged"}, "state": {"wrong"}}.Encode()
		resp, err := http.Get(back)
		if err != nil {
			return err
		}
		resp.Body.Close()
		wrongStatus = resp.StatusCode
		return f.browser(authURL)
	}
	p, err := c.Login(testContext(t), LoginOptions{OpenBrowser: browser, Verify: verifyAs("acme", testOrgID)})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if wrongStatus != http.StatusBadRequest {
		t.Errorf("a callback with the wrong state got %d, want 400", wrongStatus)
	}
	if p.Org != "acme" {
		t.Errorf("profile %+v", p)
	}
}

func TestRevokeOnlyGoesToTheServer(t *testing.T) {
	setHome(t)
	f := newFake(t)
	e := newElsewhere(t)
	c := newClient(t, f)
	p := seed(t, c, f)
	p.RevocationEndpoint = e.srv.URL + "/oauth/revoke"
	if err := profile.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := c.Logout(testContext(t), p); !errors.Is(err, ErrNotRevoked) {
		t.Fatalf("Logout: %v, want ErrNotRevoked", err)
	}
	if n := e.count(); n != 0 {
		t.Fatalf("another host received %d revocations", n)
	}
	if _, err := c.Store.Get(refreshKey("acme")); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the sign in was not forgotten locally")
	}
}

// TestRefreshUsesTheProfileKeptNow: a login that finished while this caller
// waited for the lock has replaced the client; the refresh must use the
// profile as it is now, not the copy the caller loaded before.
func TestRefreshUsesTheProfileKeptNow(t *testing.T) {
	setHome(t)
	f := newFake(t)
	c := newClient(t, f)
	p := seed(t, c, f)
	stale := p
	stale.ClientID = "client-stale"
	if _, err := c.AccessToken(testContext(t), stale); err != nil {
		t.Fatalf("AccessToken: %v", err)
	}
	if got := f.seen().refreshClients; len(got) != 1 || got[0] != "client-1" {
		t.Fatalf("refreshed as %q, want client-1", got)
	}

	if err := c.Store.Put(accessKey("acme"), "100:expired"); err != nil {
		t.Fatal(err)
	}
	if err := profile.Remove("acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AccessToken(testContext(t), p); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("AccessToken after the profile went: %v, want ErrNotSignedIn", err)
	}
	if got := f.seen().refreshClients; len(got) != 1 {
		t.Fatalf("%d refreshes, want no new one", len(got))
	}
}

// cancelOnSend cancels the caller's context as the token request goes out,
// and, like the real transport, sends nothing for a request whose context is
// already done.
type cancelOnSend struct {
	cancel context.CancelFunc
	base   http.RoundTripper
}

func (c cancelOnSend) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/oauth/token" {
		c.cancel()
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	return c.base.RoundTrip(r)
}

// TestRefreshOutlivesTheCallersCancel: once a refresh is sent the server may
// rotate, so abandoning it on a cancel would strand a spent token. It runs to
// its deadline and keeps what the server answered.
func TestRefreshOutlivesTheCallersCancel(t *testing.T) {
	setHome(t)
	f := newFake(t)
	ctx, cancel := context.WithTimeout(context.Background(), hang)
	defer cancel()
	c := &Client{Server: f.srv.URL, Store: store.NewFile(t.TempDir()),
		HTTP: &http.Client{Transport: cancelOnSend{cancel, f.srv.Client().Transport}}}
	p := seed(t, c, f)

	tok, err := c.AccessToken(ctx, p)
	if err != nil || tok != "access-1-x" {
		t.Fatalf("AccessToken = %q, %v; want access-1-x", tok, err)
	}
	if got, _ := c.Store.Get(refreshKey("acme")); got != "refresh-1-y" {
		t.Fatalf("stored refresh token %q, want the rotated one", got)
	}
}

func TestAuthorizeEndpointWithAQuery(t *testing.T) {
	setHome(t)
	f := newFake(t)
	c := newClient(t, f)
	b := &countingBrowser{f: f}
	f.set(func(f *fakeOAuth) {
		f.meta = map[string]string{"authorization_endpoint": f.srv.URL + "/oauth/authorize?tenant=x"}
	})
	if _, err := c.Login(testContext(t), LoginOptions{OpenBrowser: b.open, Verify: verifyAs("acme", testOrgID)}); err != nil {
		t.Fatalf("Login: %v", err)
	}
	raw := b.urls[0]
	if strings.Count(raw, "?") != 1 {
		t.Errorf("authorize URL %q has more than one ?", raw)
	}
	q := f.lastAuthorize()
	if q.Get("tenant") != "x" || q.Get("response_type") != "code" {
		t.Errorf("authorize query tenant %q response_type %q", q.Get("tenant"), q.Get("response_type"))
	}
}
