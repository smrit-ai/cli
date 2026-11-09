// Package auth signs the CLI in to Smrit as its own OAuth client and keeps an
// access token fresh. It follows the same rules as Smrit's memory plugin
// client, so both behave alike.
//
// The CLI registers itself (RFC 7591), signs the person in through their
// browser with PKCE and a loopback redirect (RFC 8252), and holds one refresh
// token per profile. The server kills the whole chain when a rotated refresh
// token is presented twice, so every
// refresh happens under one exclusive flock per profile and rereads the store
// first.
//
// Only a refusal signs the person out: HTTP 400 with invalid_grant (the token
// is spent, expired or revoked) or invalid_client (the registration was
// reaped). Anything else, a 5xx, a 429, an unreadable body, a timeout, no
// network, or a keychain that will not answer, is ErrUnavailable and keeps
// the refresh token for next time.
//
// A refresh the client gives up on after the server has rotated leaves the
// client holding a spent token, and the next refresh kills the chain. So a
// refresh is never started with less than MinRefreshBudget left before the
// context's deadline; without that, AccessToken returns ErrUnavailable and
// sends nothing.
//
// No error, log line or output from this package contains a token.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/smrit-ai/cli/internal/profile"
	"github.com/smrit-ai/cli/internal/server"
	"github.com/smrit-ai/cli/internal/state"
	"github.com/smrit-ai/cli/internal/store"
)

const (
	// ClientName is what the CLI registers as.
	ClientName = "Smrit CLI"
	// HeadlessEnv names the variable a headless run passes its token in.
	HeadlessEnv = "SMRIT_TOKEN"
	// DefaultTimeout bounds a call whose context has no deadline.
	DefaultTimeout = 15 * time.Second
	// MinRefreshBudget is the time a refresh needs before it is sent at all.
	MinRefreshBudget = 2 * time.Second
	// early is how long before expiry an access token is refreshed.
	early = 60 * time.Second
	// maxBody bounds what is read from any OAuth response.
	maxBody = 1 << 20
	// RequestTimeout bounds one OAuth request whatever its context says.
	RequestTimeout = 30 * time.Second
)

var (
	// ErrNotSignedIn means there is no sign in, or the server refused it.
	ErrNotSignedIn = errors.New("Not signed in. Run smrit login.")
	// ErrUnavailable means Smrit or the keychain could not give an answer.
	// The sign in, if there is one, is kept.
	ErrUnavailable = errors.New("Smrit could not be reached. Your sign in is kept; try again.")
	// ErrOrgMismatch means the browser signed in to another organization
	// than the one asked for. That sign in was revoked and nothing was kept.
	ErrOrgMismatch = errors.New("Signed in to a different organization than the one asked for. Nothing was kept.")
	// ErrLoginFailed means the sign in did not complete.
	ErrLoginFailed = errors.New("The sign in did not complete.")
	// ErrNotRevoked comes from Logout when the server could not be told. The
	// sign in is forgotten on this machine all the same.
	ErrNotRevoked = errors.New("Signed out on this machine, but Smrit could not be told; the sign in will expire on its own.")
)

// detailed is an error whose message is specific and which still matches its
// sentinel with errors.Is.
type detailed struct {
	kind  error
	msg   string
	cause error
}

func (e *detailed) Error() string        { return e.msg }
func (e *detailed) Is(target error) bool { return target == e.kind }
func (e *detailed) Unwrap() error        { return e.cause }

func fail(kind error, cause error, format string, a ...any) error {
	return &detailed{kind: kind, msg: fmt.Sprintf(format, a...), cause: cause}
}

// Client signs in and refreshes against one Smrit server.
type Client struct {
	// Server is the base address. Every request goes to this origin.
	Server string
	// Store keeps the tokens.
	Store store.Store
	// HTTP makes the requests. Nil means a client with RequestTimeout. Its
	// redirect policy is always replaced: see oauthHTTP.
	HTTP *http.Client
	// Now is the clock. Nil means time.Now.
	Now func() time.Time

	// lockBusy, in tests, is called when a refresh finds the lock held.
	lockBusy func()
}

// Default is the client for the compiled address and the platform's store.
func Default() *Client {
	return &Client{Server: server.Address, Store: store.Default()}
}

// Login signs in with the default client. See Client.Login.
func Login(ctx context.Context, opts LoginOptions) (profile.Profile, error) {
	return Default().Login(ctx, opts)
}

// AccessToken returns a fresh access token with the default client. See
// Client.AccessToken.
func AccessToken(ctx context.Context, p profile.Profile) (string, error) {
	return Default().AccessToken(ctx, p)
}

// Logout signs out with the default client. See Client.Logout.
func Logout(ctx context.Context, p profile.Profile) error {
	return Default().Logout(ctx, p)
}

// ForceRefresh refreshes the access token with the default client. See
// Client.ForceRefresh.
func ForceRefresh(ctx context.Context, p profile.Profile, refused string) (string, error) {
	return Default().ForceRefresh(ctx, p, refused)
}

// HeadlessToken returns the token in SMRIT_TOKEN and true when it is set. A
// headless token is used as it is, for the bare /mcp endpoint, and only ever
// sent to the compiled address: nothing stores it, refreshes it or sends it
// anywhere else.
func HeadlessToken() (string, bool) {
	tok := os.Getenv(HeadlessEnv)
	return tok, tok != ""
}

// bounded gives one request at most DefaultTimeout, or less when ctx ends
// sooner, so no request waits forever on a context without a deadline.
func bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, DefaultTimeout)
}

// http is the client every OAuth request goes through. It never follows a
// redirect: on a 307 or 308 Go resends a POST body to the new location, which
// would hand a refresh token, a code or a revocation to whatever host the
// answer names. A redirect is returned as the response, and the caller treats
// it as the failure it is.
func (c *Client) http() *http.Client {
	var hc http.Client
	if c.HTTP != nil {
		hc = *c.HTTP
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if hc.Timeout == 0 {
		hc.Timeout = RequestTimeout
	}
	return &hc
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func refreshKey(org string) string { return org + ":refresh" }
func accessKey(org string) string  { return org + ":access" }

// base is the server address, checked: https, or http to this machine. A
// token is never sent in the clear to another host.
func (c *Client) base() (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(c.Server, "/"))
	if err != nil || !safeURL(u) || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return nil, fail(ErrLoginFailed, nil, "The Smrit address this CLI was built with is not usable.")
	}
	u.Path = ""
	return u, nil
}

func loopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func safeURL(u *url.URL) bool {
	switch u.Scheme {
	case "https":
		return u.Hostname() != ""
	case "http":
		return loopbackHost(u.Hostname())
	}
	return false
}

func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// onServer reports whether raw is a URL on the same origin as base.
func onServer(raw string, base *url.URL) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return false
	}
	return u.Scheme == base.Scheme && strings.EqualFold(u.Hostname(), base.Hostname()) && port(u) == port(base)
}

// lock takes the profile's refresh lock.
func (c *Client) lock(ctx context.Context, org string) (func(), error) {
	dir, err := state.Ensure()
	if err != nil {
		return nil, fail(ErrUnavailable, err, "%v", err)
	}
	unlock, err := state.Lock(ctx, filepath.Join(dir, "refresh-"+org+".lock"), c.lockBusy)
	if err != nil {
		return nil, fail(ErrUnavailable, err, "Another sign in or refresh for %s is still running. Try again.", org)
	}
	return unlock, nil
}

// cached reads the stored access token: the token, and whether it is still
// fresh. A missing token is not an error.
func (c *Client) cached(org string) (string, bool, error) {
	v, err := c.Store.Get(accessKey(org))
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	at, tok, ok := strings.Cut(v, ":")
	expires, perr := strconv.ParseInt(at, 10, 64)
	if !ok || perr != nil || tok == "" {
		return "", false, nil
	}
	return tok, time.Unix(expires, 0).Add(-early).After(c.now()), nil
}

func (c *Client) putAccess(org string, t tokens) error {
	expires := c.now().Add(time.Duration(t.ExpiresIn) * time.Second).Unix()
	return c.Store.Put(accessKey(org), strconv.FormatInt(expires, 10)+":"+t.AccessToken)
}

// forget drops both tokens of org. The caller holds the refresh lock.
func (c *Client) forget(org string) error {
	errA := c.Store.Delete(accessKey(org))
	errR := c.Store.Delete(refreshKey(org))
	return errors.Join(errR, errA)
}

// Logout revokes the profile's refresh token, then forgets the tokens and the
// profile, under the refresh lock so a refresh in flight cannot write its
// rotated token back afterwards. A revocation that fails still forgets
// locally and returns an error matching ErrNotRevoked. ErrUnavailable means
// the lock or the store would not answer and the sign in may still be here.
func (c *Client) Logout(ctx context.Context, p profile.Profile) error {
	if !profile.ValidSlug(p.Org) {
		return ErrNotSignedIn
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	unlock, err := c.lock(ctx, p.Org)
	if err != nil {
		return err
	}
	defer unlock()

	// The profile as it is now: a login that finished while this waited for
	// the lock has replaced the client the caller's copy names.
	if now, err := profile.Pick(p.Org); err == nil {
		p = now
	}
	var revokeErr error
	refresh, err := c.Store.Get(refreshKey(p.Org))
	switch {
	case err == nil:
		revokeErr = c.revoke(ctx, p.RevocationEndpoint, refresh, p.ClientID)
	case !errors.Is(err, store.ErrNotFound):
		revokeErr = err
	}
	if err := c.forget(p.Org); err != nil {
		return fail(ErrUnavailable, err, "The sign in for %s could not be removed from this machine: %v", p.Org, err)
	}
	if err := profile.Remove(p.Org); err != nil {
		return fail(ErrUnavailable, err, "%v", err)
	}
	if revokeErr != nil {
		return fail(ErrNotRevoked, revokeErr, "Signed out of %s on this machine, but Smrit could not be told; the sign in will expire on its own.", p.Org)
	}
	return nil
}
