package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smrit-ai/cli/internal/profile"
	"github.com/smrit-ai/cli/internal/store"
)

// hang is a hang limit only: no test asserts on how long anything took.
const hang = 60 * time.Second

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), hang)
	t.Cleanup(cancel)
	return ctx
}

func TestLoginStoresProfile(t *testing.T) {
	setHome(t)
	f := newFake(t)
	c := newClient(t, f)
	ctx := testContext(t)

	var verified string
	p, err := c.Login(ctx, LoginOptions{
		OpenBrowser: f.browser,
		Verify: func(_ context.Context, access string) (string, string, error) {
			verified = access
			return "acme", testOrgID, nil
		},
	})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	seen := f.seen()
	access, refresh := seen.accessTokens[0], seen.refreshes[0]
	if verified != access {
		t.Errorf("Verify was given %q, want the new access token", verified)
	}
	want := profile.Profile{
		Org:                "acme",
		OrgID:              testOrgID,
		Endpoint:           "/mcp/o/acme/" + testOrgID,
		ClientID:           "client-1",
		RedirectURI:        p.RedirectURI,
		TokenEndpoint:      f.srv.URL + "/oauth/token",
		RevocationEndpoint: f.srv.URL + "/oauth/revoke",
	}
	if p != want {
		t.Errorf("profile %+v\nwant    %+v", p, want)
	}
	if !strings.HasPrefix(p.RedirectURI, "http://127.0.0.1:") || !strings.HasSuffix(p.RedirectURI, "/callback") {
		t.Errorf("redirect uri %q is not a loopback callback", p.RedirectURI)
	}

	if len(seen.registered) != 1 || seen.registered[0]["client_name"] != "Smrit CLI" {
		t.Errorf("registration %+v, want one as Smrit CLI", seen.registered)
	}
	q := f.lastAuthorize()
	for k, v := range map[string]string{
		"response_type":         "code",
		"client_id":             "client-1",
		"code_challenge_method": "S256",
		"scope":                 "write",
		"resource":              f.srv.URL + "/mcp",
		"redirect_uri":          p.RedirectURI,
	} {
		if q.Get(k) != v {
			t.Errorf("authorize %s = %q, want %q", k, q.Get(k), v)
		}
	}
	if q.Get("state") == "" || len(q.Get("code_challenge")) < 43 {
		t.Errorf("authorize state %q challenge %q", q.Get("state"), q.Get("code_challenge"))
	}

	saved, err := profile.Load()
	if err != nil || len(saved) != 1 || saved[0] != want {
		t.Fatalf("saved profiles %+v, %v", saved, err)
	}
	path, _ := profile.Path()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(access)) || bytes.Contains(raw, []byte(refresh)) {
		t.Fatal("the profile file holds a token")
	}
	if got, err := c.Store.Get(refreshKey("acme")); err != nil || got != refresh {
		t.Fatalf("stored refresh token %q, %v", got, err)
	}

	tok, err := c.AccessToken(ctx, p)
	if err != nil || tok != access {
		t.Fatalf("AccessToken = %q, %v; want the access token from the sign in", tok, err)
	}
	if _, refreshes := f.counts(); refreshes != 0 {
		t.Fatalf("%d refreshes for a fresh token", refreshes)
	}
}

func TestKnownProfileSignsInAtItsOwnEndpoint(t *testing.T) {
	setHome(t)
	f := newFake(t)
	c := newClient(t, f)
	ctx := testContext(t)
	first, err := c.Login(ctx, LoginOptions{OpenBrowser: f.browser, Verify: verifyAs("acme", testOrgID)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Login(ctx, LoginOptions{Org: "acme", OpenBrowser: f.browser, Verify: verifyAs("acme", testOrgID)})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.lastAuthorize().Get("resource"); got != f.srv.URL+"/mcp/o/acme/"+testOrgID {
		t.Errorf("second sign in resource %q, want the org endpoint", got)
	}
	seen := f.seen()
	if len(seen.registered) != 1 || second.ClientID != first.ClientID {
		t.Errorf("a known profile registered again: %d registrations, client %q then %q",
			len(seen.registered), first.ClientID, second.ClientID)
	}
	if got, _ := c.Store.Get(refreshKey("acme")); got != seen.refreshes[1] {
		t.Errorf("stored refresh token is not the newest")
	}
	if len(seen.revoked) != 1 || seen.revoked[0].Get("token") != seen.refreshes[0] {
		t.Errorf("the previous sign in was not revoked: %+v", seen.revoked)
	}
}

func TestOrgMismatchRevokesAndExits4(t *testing.T) {
	setHome(t)
	f := newFake(t)
	c := newClient(t, f)
	_, err := c.Login(testContext(t), LoginOptions{
		Org:         "beta",
		OpenBrowser: f.browser,
		Verify:      verifyAs("acme", testOrgID),
	})
	if !errors.Is(err, ErrOrgMismatch) {
		t.Fatalf("Login: %v, want ErrOrgMismatch", err)
	}
	seen := f.seen()
	if len(seen.revoked) != 1 {
		t.Fatalf("%d revocations, want 1", len(seen.revoked))
	}
	if got := seen.revoked[0]; got.Get("token") != seen.refreshes[0] || got.Get("client_id") != "client-1" {
		t.Errorf("revoked %v, want the refresh token for client-1", got)
	}
	if strings.Contains(err.Error(), seen.refreshes[0]) || strings.Contains(err.Error(), seen.accessTokens[0]) {
		t.Error("the error carries a token")
	}
	if all, err := profile.Load(); err != nil || len(all) != 0 {
		t.Errorf("profiles saved: %+v, %v", all, err)
	}
	for _, org := range []string{"acme", "beta"} {
		for _, key := range []string{refreshKey(org), accessKey(org)} {
			if _, err := c.Store.Get(key); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s is stored: %v", key, err)
			}
		}
	}
}

// TestRefreshUnderConcurrency holds the one refresh at the server until the
// other nine callers have each found the lock taken, so all ten overlap
// without a sleep. Any second refresh is a replay, which the fake (like the
// real server) answers invalid_grant, and the test fails on it at once.
func TestRefreshUnderConcurrency(t *testing.T) {
	setHome(t)
	f := newFake(t)
	c := newClient(t, f)
	p := seed(t, c, f)
	ctx := testContext(t)

	first := make(chan struct{})
	release := make(chan struct{})
	replay := make(chan int, 16)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	f.setOnRefresh(func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 1 {
			close(first)
			<-release
			return false
		}
		replay <- n
		return false
	})
	busy := make(chan struct{}, 16)
	c.lockBusy = func() { busy <- struct{}{} }

	type result struct {
		token string
		err   error
	}
	results := make(chan result, 10)
	call := func() {
		tok, err := c.AccessToken(ctx, p)
		results <- result{tok, err}
	}
	go call()
	<-first
	for i := 0; i < 9; i++ {
		go call()
	}
	for waiting := 0; waiting < 9; {
		select {
		case <-busy:
			waiting++
		case n := <-replay:
			t.Fatalf("refresh %d reached the server while the first was in flight", n)
		case <-ctx.Done():
			t.Fatal("hang limit reached waiting for the callers to queue on the lock")
		}
	}
	releaseOnce.Do(func() { close(release) })

	for i := 0; i < 10; i++ {
		r := <-results
		if r.err != nil || r.token != "access-1-x" {
			t.Errorf("caller got %q, %v; want access-1-x", r.token, r.err)
		}
	}
	if _, refreshes := f.counts(); refreshes != 1 {
		t.Fatalf("%d refresh requests reached the server, want 1", refreshes)
	}
}

// TestRefreshAcrossProcesses runs the same race between two processes, each
// a copy of this test binary: the second is started only once the first's
// refresh is at the server, and the server answers only once the second has
// found the lock taken.
func TestRefreshAcrossProcesses(t *testing.T) {
	home := setHome(t)
	f := newFake(t)
	storeDir := t.TempDir()
	c := &Client{Server: f.srv.URL, Store: store.NewFile(storeDir), HTTP: f.srv.Client()}
	seed(t, c, f)
	ctx := testContext(t)

	first := make(chan struct{})
	release := make(chan struct{})
	replay := make(chan int, 4)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	f.setOnRefresh(func(n int, w http.ResponseWriter, r *http.Request) bool {
		if n == 1 {
			close(first)
			<-release
			return false
		}
		replay <- n
		return false
	})

	start := func(role string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperProcess$")
		cmd.Env = append(os.Environ(),
			"SMRIT_AUTH_HELPER="+role,
			"SMRIT_HELPER_SERVER="+f.srv.URL,
			"SMRIT_HELPER_STORE="+storeDir,
			"HOME="+home,
		)
		var out, errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errOut
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd, &out, &errOut
	}
	a, aOut, aErr := start("first")
	select {
	case <-first:
	case <-ctx.Done():
		t.Fatal("hang limit reached waiting for the first process to refresh")
	}
	b, bOut, bErr := start("second")
	select {
	case <-f.busy:
	case n := <-replay:
		t.Fatalf("refresh %d reached the server while the first was in flight", n)
	case <-ctx.Done():
		t.Fatal("hang limit reached waiting for the second process to queue on the lock")
	}
	releaseOnce.Do(func() { close(release) })
	if err := a.Wait(); err != nil {
		t.Fatalf("first process: %v\n%s", err, aErr)
	}
	if err := b.Wait(); err != nil {
		t.Fatalf("second process: %v\n%s", err, bErr)
	}
	if aOut.String() != "access-1-x" || bOut.String() != "access-1-x" {
		t.Errorf("processes printed %q and %q, want access-1-x from both", aOut, bOut)
	}
	if _, refreshes := f.counts(); refreshes != 1 {
		t.Fatalf("%d refresh requests reached the server, want 1", refreshes)
	}
}

// TestHelperProcess is the body of TestRefreshAcrossProcesses's children. It
// does nothing in a normal run.
func TestHelperProcess(t *testing.T) {
	role := os.Getenv("SMRIT_AUTH_HELPER")
	if role == "" {
		return
	}
	server := os.Getenv("SMRIT_HELPER_SERVER")
	c := &Client{Server: server, Store: store.NewFile(os.Getenv("SMRIT_HELPER_STORE")), HTTP: http.DefaultClient}
	if role == "second" {
		var once sync.Once
		c.lockBusy = func() {
			once.Do(func() {
				if resp, err := http.Get(server + "/test/busy"); err == nil {
					resp.Body.Close()
				}
			})
		}
	}
	p, err := profile.Pick("acme")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	ctx, cancel := context.WithTimeout(context.Background(), hang)
	defer cancel()
	tok, err := c.AccessToken(ctx, p)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	fmt.Print(tok)
	os.Exit(0)
}

func TestOnlyInvalidGrantSignsOut(t *testing.T) {
	answer := func(status int, body string) func(context.CancelFunc) func(int, http.ResponseWriter, *http.Request) bool {
		return func(context.CancelFunc) func(int, http.ResponseWriter, *http.Request) bool {
			return func(_ int, w http.ResponseWriter, _ *http.Request) bool {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				w.Write([]byte(body))
				return true
			}
		}
	}
	cases := []struct {
		name      string
		handler   func(cancel context.CancelFunc) func(int, http.ResponseWriter, *http.Request) bool
		want      error
		signedOut bool
	}{
		{"invalid_grant", answer(400, `{"error":"invalid_grant"}`), ErrNotSignedIn, true},
		{"invalid_client", answer(400, `{"error":"invalid_client"}`), ErrNotSignedIn, true},
		{"server error", answer(500, `{"error":"server_error"}`), ErrUnavailable, false},
		{"rate limited", answer(429, `{"error":"temporarily_unavailable"}`), ErrUnavailable, false},
		{"invalid_request", answer(400, `{"error":"invalid_request"}`), ErrUnavailable, false},
		{"invalid_grant not on a 400", answer(401, `{"error":"invalid_grant"}`), ErrUnavailable, false},
		{"unreadable refusal", answer(400, `invalid_grant`), ErrUnavailable, false},
		{"unreadable body", answer(200, `not json`), ErrUnavailable, false},
		{"no token pair", answer(200, `{"access_token":"a"}`), ErrUnavailable, false},
		{"timeout", func(context.CancelFunc) func(int, http.ResponseWriter, *http.Request) bool {
			return func(_ int, _ http.ResponseWriter, r *http.Request) bool {
				// No answer: the client gives up at its deadline and closes.
				<-r.Context().Done()
				return true
			}
		}, ErrUnavailable, false},
		{"network", func(context.CancelFunc) func(int, http.ResponseWriter, *http.Request) bool {
			return func(_ int, w http.ResponseWriter, _ *http.Request) bool {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
				return true
			}
		}, ErrUnavailable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setHome(t)
			f := newFake(t)
			c := newClient(t, f)
			p := seed(t, c, f)
			limit := hang
			if tc.name == "timeout" {
				// A deadline, not a cancel: a refresh once sent runs to its
				// deadline. Five seconds leaves the two second budget intact.
				limit = 5 * time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), limit)
			defer cancel()
			f.setOnRefresh(tc.handler(cancel))

			tok, err := c.AccessToken(ctx, p)
			if !errors.Is(err, tc.want) {
				t.Fatalf("AccessToken = %q, %v; want %v", tok, err, tc.want)
			}
			if tc.want == ErrUnavailable && errors.Is(err, ErrNotSignedIn) {
				t.Fatal("an unavailable answer also reads as signed out")
			}
			if _, refreshes := f.counts(); refreshes != 1 {
				t.Fatalf("%d refresh requests, want 1", refreshes)
			}
			if strings.Contains(err.Error(), "refresh-0-y") {
				t.Fatal("the error carries the refresh token")
			}
			got, gerr := c.Store.Get(refreshKey("acme"))
			if tc.signedOut {
				if !errors.Is(gerr, store.ErrNotFound) {
					t.Errorf("refresh token kept after a refusal: %q, %v", got, gerr)
				}
				if _, aerr := c.Store.Get(accessKey("acme")); !errors.Is(aerr, store.ErrNotFound) {
					t.Errorf("access token kept after a refusal: %v", aerr)
				}
			} else if gerr != nil || got != "refresh-0-y" {
				t.Errorf("refresh token %q, %v; want it kept", got, gerr)
			}
		})
	}
}

func TestTooLittleTimeSendsNothing(t *testing.T) {
	setHome(t)
	f := newFake(t)
	c := newClient(t, f)
	p := seed(t, c, f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.AccessToken(ctx, p); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("AccessToken: %v, want ErrUnavailable", err)
	}
	if calls, _ := f.counts(); calls != 0 {
		t.Fatalf("%d token requests sent with under two seconds left", calls)
	}
	if got, _ := c.Store.Get(refreshKey("acme")); got != "refresh-0-y" {
		t.Fatal("refresh token not kept")
	}
}

func TestNoSignInIsNotSignedIn(t *testing.T) {
	setHome(t)
	f := newFake(t)
	c := newClient(t, f)
	p := seed(t, c, f)
	if err := c.Store.Delete(refreshKey("acme")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AccessToken(testContext(t), p); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("AccessToken: %v, want ErrNotSignedIn", err)
	}
	if _, err := c.AccessToken(testContext(t), profile.Profile{}); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("AccessToken with an empty profile: %v, want ErrNotSignedIn", err)
	}
}

func TestHeadlessToken(t *testing.T) {
	setHome(t)
	f := newFake(t)
	t.Setenv(HeadlessEnv, "headless-token-z")
	tok, ok := HeadlessToken()
	if !ok || tok != "headless-token-z" {
		t.Fatalf("HeadlessToken = %q, %v", tok, ok)
	}
	c := &Client{Server: f.srv.URL, HTTP: f.srv.Client()} // no store at all
	got, err := c.AccessToken(testContext(t), profile.Profile{})
	if err != nil || got != "headless-token-z" {
		t.Fatalf("AccessToken = %q, %v", got, err)
	}
	if calls, _ := f.counts(); calls != 0 {
		t.Fatal("a headless token went through the token endpoint")
	}
	t.Setenv(HeadlessEnv, "")
	if _, ok := HeadlessToken(); ok {
		t.Fatal("an empty SMRIT_TOKEN reads as headless")
	}
}

func TestLogout(t *testing.T) {
	for _, revokeStatus := range []int{200, 500} {
		t.Run(fmt.Sprint(revokeStatus), func(t *testing.T) {
			setHome(t)
			f := newFake(t)
			f.setRevokeStatus(revokeStatus)
			c := newClient(t, f)
			p := seed(t, c, f)
			err := c.Logout(testContext(t), p)
			if revokeStatus == 200 && err != nil {
				t.Fatalf("Logout: %v", err)
			}
			if revokeStatus != 200 && !errors.Is(err, ErrNotRevoked) {
				t.Fatalf("Logout with a failing server: %v, want ErrNotRevoked", err)
			}
			revoked := f.seen().revoked
			if len(revoked) != 1 || revoked[0].Get("token") != "refresh-0-y" || revoked[0].Get("client_id") != "client-1" {
				t.Errorf("revoked %+v", revoked)
			}
			for _, key := range []string{refreshKey("acme"), accessKey("acme")} {
				if _, err := c.Store.Get(key); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("%s still stored: %v", key, err)
				}
			}
			if all, _ := profile.Load(); len(all) != 0 {
				t.Errorf("profile kept: %+v", all)
			}
		})
	}
}

func TestEndpointsMustBeOnTheCompiledAddress(t *testing.T) {
	setHome(t)
	f := newFake(t)
	c := newClient(t, f)
	p := seed(t, c, f)
	p.TokenEndpoint = "https://elsewhere.example/oauth/token"
	if err := profile.Save(p); err != nil { // the refresh reads the saved profile
		t.Fatal(err)
	}
	if _, err := c.AccessToken(testContext(t), p); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("AccessToken: %v, want ErrUnavailable", err)
	}
	if calls, _ := f.counts(); calls != 0 {
		t.Fatal("a refresh token was sent")
	}
	if got, _ := c.Store.Get(refreshKey("acme")); got != "refresh-0-y" {
		t.Fatal("refresh token not kept")
	}
}

func TestForceRefreshRefreshesAFreshToken(t *testing.T) {
	setHome(t)
	f := newFake(t)
	c := newClient(t, f)
	p := seed(t, c, f)
	ctx := testContext(t)
	cached, err := c.AccessToken(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	_, before := f.counts()
	tok, err := c.ForceRefresh(ctx, p)
	if err != nil || tok == "" || tok == cached {
		t.Fatalf("ForceRefresh = %q, %v; cached %q", tok, err, cached)
	}
	if _, n := f.counts(); n != before+1 {
		t.Fatalf("%d refreshes after %d, want one more", n, before)
	}
	if got, err := c.AccessToken(ctx, p); err != nil || got != tok {
		t.Fatalf("the cache does not hold the new token: %q %v", got, err)
	}
}

func TestForceRefreshHeadlessHasNothingToRefresh(t *testing.T) {
	setHome(t)
	t.Setenv(HeadlessEnv, "tok_headless")
	f := newFake(t)
	c := newClient(t, f)
	if _, err := c.ForceRefresh(testContext(t), profile.Profile{Org: "acme"}); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("%v", err)
	}
	if _, n := f.counts(); n != 0 {
		t.Fatalf("%d refreshes", n)
	}
}
