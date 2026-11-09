package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os/exec"
	"runtime"
	"time"

	"github.com/smrit-ai/cli/internal/profile"
)

// DefaultWait is how long Login waits for the browser by default.
const DefaultWait = 5 * time.Minute

// LoginOptions configures one sign in.
type LoginOptions struct {
	// Org, when set, is a check: a sign in to any other organization is
	// revoked, nothing is kept, and Login returns ErrOrgMismatch. A profile
	// already kept for Org signs in again at its own organization endpoint.
	Org string
	// OpenBrowser opens the authorize URL. Nil means OpenSystemBrowser.
	OpenBrowser func(url string) error
	// Verify names the organization an access token belongs to; the token
	// response does not. Required.
	Verify func(ctx context.Context, accessToken string) (orgSlug, orgID string, err error)
	// Notice, when set, is told the authorize URL if the browser cannot be
	// opened, so the person can open it themselves. The URL holds no secret.
	Notice io.Writer
	// Wait bounds the wait for the browser. Zero means DefaultWait.
	Wait time.Duration
}

// OpenSystemBrowser opens url with the platform's opener.
func OpenSystemBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, url).Run()
}

// Login signs in through the browser and keeps the result.
//
// It discovers the server at the compiled address, then registers this CLI
// (as "Smrit CLI") for a loopback redirect on 127.0.0.1 with a random port,
// or reuses the client of the profile already kept for opts.Org when that
// profile's port can be opened again. A new profile authorizes at the bare
// /mcp resource, so the consent screen picks the organization; a known one
// authorizes at its own organization endpoint. The code is exchanged with
// PKCE (S256) and opts.Verify names the organization. On success the tokens
// are kept in the store and the profile is saved, keyed by the organization
// slug, and a previous sign in for that organization is revoked.
func (c *Client) Login(ctx context.Context, opts LoginOptions) (profile.Profile, error) {
	if opts.Verify == nil {
		return profile.Profile{}, fail(ErrLoginFailed, nil, "Sign in needs a way to name the organization.")
	}
	if opts.Org != "" && !profile.ValidSlug(opts.Org) {
		return profile.Profile{}, fail(ErrLoginFailed, nil, "%q is not an organization name.", opts.Org)
	}
	if c.Store == nil {
		return profile.Profile{}, fail(ErrLoginFailed, nil, "There is nowhere to keep the sign in.")
	}
	open := opts.OpenBrowser
	if open == nil {
		open = OpenSystemBrowser
	}
	wait := opts.Wait
	if wait <= 0 {
		wait = DefaultWait
	}

	base, err := c.base()
	if err != nil {
		return profile.Profile{}, err
	}
	meta, err := c.discover(ctx, base)
	if err != nil {
		return profile.Profile{}, err
	}

	var prior *profile.Profile
	if opts.Org != "" {
		if p, err := profile.Pick(opts.Org); err == nil {
			prior = &p
		} else if !errors.Is(err, profile.ErrNoProfile) {
			return profile.Profile{}, fail(ErrLoginFailed, err, "%v", err)
		}
	}

	var (
		ln          net.Listener
		redirectURI string
		clientID    string
	)
	if prior != nil && prior.ClientID != "" && prior.RedirectURI != "" {
		if l, uri, err := listen(prior.RedirectURI); err == nil {
			ln, redirectURI, clientID = l, uri, prior.ClientID
		}
	}
	if ln == nil {
		ln, redirectURI, err = listen("")
		if err != nil {
			return profile.Profile{}, fail(ErrLoginFailed, err, "Could not open a local port for the browser to return to.")
		}
		clientID, err = c.register(ctx, meta.RegistrationEndpoint, redirectURI)
		if err != nil {
			ln.Close()
			return profile.Profile{}, err
		}
	}

	resourcePath := "/mcp"
	if prior != nil && prior.Endpoint == profile.EndpointPath(prior.Org, prior.OrgID) {
		resourcePath = prior.Endpoint
	}
	resource, err := c.resource(ctx, base, resourcePath)
	if err != nil {
		ln.Close()
		return profile.Profile{}, err
	}

	verifier, challenge := pkce()
	state := randomString(24)
	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"write"},
		"state":                 {state},
		"resource":              {resource},
	}
	authURL := meta.AuthorizationEndpoint + "?" + query.Encode()

	got, stop := serveCallback(ln, state)
	defer stop()
	if err := open(authURL); err != nil {
		if opts.Notice == nil {
			return profile.Profile{}, fail(ErrLoginFailed, err, "Could not open a browser to sign in.")
		}
		fmt.Fprintf(opts.Notice, "Open this address in a browser to sign in:\n%s\n", authURL)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var cb callback
	select {
	case cb = <-got:
	case <-ctx.Done():
		return profile.Profile{}, fail(ErrLoginFailed, ctx.Err(), "The sign in was stopped before the browser came back.")
	case <-timer.C:
		return profile.Profile{}, fail(ErrLoginFailed, nil, "The browser did not come back within %s.", wait)
	}
	stop()
	if cb.code == "" {
		return profile.Profile{}, fail(ErrLoginFailed, nil, "Smrit did not sign you in (%s).", safeCode(cb.error))
	}

	t, err := c.postForm(ctx, meta.TokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {cb.code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})
	var r *refused
	if errors.As(err, &r) {
		return profile.Profile{}, fail(ErrLoginFailed, nil, "Smrit refused the sign in (%s). Try again.", r.code)
	}
	if err != nil {
		return profile.Profile{}, err
	}
	// From here a live sign in exists on the server; every way out that does
	// not keep it revokes it.
	drop := func() { c.revoke(context.WithoutCancel(ctx), meta.RevocationEndpoint, t.RefreshToken, clientID) }

	slug, id, err := opts.Verify(ctx, t.AccessToken)
	if err != nil {
		drop()
		return profile.Profile{}, fail(ErrUnavailable, err, "Could not confirm which organization you signed in to: %v", err)
	}
	if !profile.ValidSlug(slug) || !profile.ValidID(id) {
		drop()
		return profile.Profile{}, fail(ErrLoginFailed, nil, "Smrit named an organization this CLI cannot keep.")
	}
	if opts.Org != "" && slug != opts.Org {
		drop()
		return profile.Profile{}, fail(ErrOrgMismatch, nil,
			"You signed in to %s, not %s. That sign in was revoked and nothing was kept.", slug, opts.Org)
	}

	p := profile.Profile{
		Org:                slug,
		OrgID:              id,
		Endpoint:           profile.EndpointPath(slug, id),
		ClientID:           clientID,
		RedirectURI:        redirectURI,
		TokenEndpoint:      meta.TokenEndpoint,
		RevocationEndpoint: meta.RevocationEndpoint,
	}
	old, err := c.save(ctx, p, t)
	if err != nil {
		drop()
		return profile.Profile{}, err
	}
	if old.token != "" && old.token != t.RefreshToken {
		// The sign in this one replaces would otherwise live on, unused,
		// until it expired.
		c.revoke(context.WithoutCancel(ctx), old.endpoint, old.token, old.clientID)
	}
	return p, nil
}

type previous struct{ token, clientID, endpoint string }

// save keeps a new sign in under the profile's refresh lock, so a refresh of
// the previous sign in cannot write its rotated token over this one. The
// refresh token goes first: if it cannot be kept, the sign in has failed and
// the previous one, if any, is left as it was. If the access token or the
// profile cannot be written after it, the new tokens are dropped again, so no
// refresh token is left beside a profile naming another client. It returns
// the sign in it replaced, for the caller to revoke.
func (c *Client) save(ctx context.Context, p profile.Profile, t tokens) (previous, error) {
	lockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	unlock, err := c.lock(lockCtx, p.Org)
	if err != nil {
		return previous{}, err
	}
	defer unlock()

	var old previous
	if tok, err := c.Store.Get(refreshKey(p.Org)); err == nil {
		old.token = tok
		if q, err := profile.Pick(p.Org); err == nil {
			old.clientID, old.endpoint = q.ClientID, q.RevocationEndpoint
		} else {
			old.token = "" // no client to revoke it as
		}
	}
	if err := c.Store.Put(refreshKey(p.Org), t.RefreshToken); err != nil {
		return previous{}, fail(ErrLoginFailed, err, "The sign in could not be kept: %v", err)
	}
	if err := c.putAccess(p.Org, t); err != nil {
		c.forget(p.Org)
		return previous{}, fail(ErrLoginFailed, err, "The sign in could not be kept: %v", err)
	}
	if err := profile.Save(p); err != nil {
		c.forget(p.Org)
		return previous{}, fail(ErrLoginFailed, err, "The sign in could not be saved: %v", err)
	}
	return old, nil
}
