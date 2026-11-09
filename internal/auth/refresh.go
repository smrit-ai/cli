package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/smrit-ai/cli/internal/profile"
	"github.com/smrit-ai/cli/internal/store"
)

// tokens is a token endpoint answer.
type tokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
}

// refused is the token endpoint refusing the grant: HTTP 400 with
// invalid_grant or invalid_client. Nothing else signs a person out.
type refused struct{ code string }

func (r *refused) Error() string { return "Smrit refused the sign in (" + r.code + ")." }

// postForm posts to the token endpoint. It returns the token pair, *refused
// for a refusal, and an error matching ErrUnavailable for anything else.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) (tokens, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokens{}, fail(ErrUnavailable, nil, "The token endpoint address is not usable.")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		// A url.Error names the endpoint and the cause; the token is in the
		// body, never the URL, so neither carries it.
		return tokens{}, fail(ErrUnavailable, nil, "Could not reach the Smrit token endpoint.")
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		code := ""
		if rerr == nil && json.Unmarshal(raw, &body) == nil {
			code = body.Error
		}
		if resp.StatusCode == http.StatusBadRequest && (code == "invalid_grant" || code == "invalid_client") {
			return tokens{}, &refused{code}
		}
		return tokens{}, fail(ErrUnavailable, nil, "The Smrit token endpoint answered HTTP %d %s. Your sign in is kept; try again.",
			resp.StatusCode, safeCode(code))
	}
	if rerr != nil {
		return tokens{}, fail(ErrUnavailable, nil, "The Smrit token endpoint answer could not be read. Your sign in is kept; try again.")
	}
	var body struct {
		AccessToken  any `json:"access_token"`
		RefreshToken any `json:"refresh_token"`
		ExpiresIn    any `json:"expires_in"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return tokens{}, fail(ErrUnavailable, nil, "The Smrit token endpoint answered something that is not JSON.")
	}
	access, _ := body.AccessToken.(string)
	refresh, _ := body.RefreshToken.(string)
	if access == "" || refresh == "" {
		return tokens{}, fail(ErrUnavailable, nil, "The Smrit token endpoint answered without a token pair.")
	}
	t := tokens{AccessToken: access, RefreshToken: refresh, ExpiresIn: 3600}
	if n, ok := body.ExpiresIn.(float64); ok && n > 0 && n < 1e9 {
		t.ExpiresIn = int64(n)
	}
	return t, nil
}

// revoke tells the server to end a refresh token's chain (RFC 7009). Only to
// an endpoint on the compiled origin.
func (c *Client) revoke(ctx context.Context, endpoint, token, clientID string) error {
	base, err := c.base()
	if err != nil {
		return err
	}
	if endpoint == "" {
		endpoint = base.String() + "/oauth/revoke"
	}
	if !onServer(endpoint, base) {
		return errors.New("The revocation endpoint is not on the Smrit address.")
	}
	ctx, cancel := bounded(ctx)
	defer cancel()
	form := url.Values{"token": {token}, "token_type_hint": {"refresh_token"}, "client_id": {clientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("The revocation endpoint address is not usable.")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http().Do(req)
	if err != nil {
		return errors.New("Could not reach the Smrit revocation endpoint.")
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("The Smrit revocation endpoint did not accept the request.")
	}
	return nil
}

// AccessToken returns a fresh access token for p, refreshing it when it is
// within a minute of expiry. ErrNotSignedIn means there is no sign in or the
// server refused it; ErrUnavailable means no answer could be had before the
// context's deadline (DefaultTimeout when it has none), the lock wait
// included, and the sign in is kept.
//
// When SMRIT_TOKEN is set, it is returned as it is and nothing is stored or
// refreshed (spec section 6, headless). The caller sends it only to the
// compiled address, server.Address, at the bare /mcp endpoint: see
// HeadlessToken.
func (c *Client) AccessToken(ctx context.Context, p profile.Profile) (string, error) {
	if tok, ok := HeadlessToken(); ok {
		return tok, nil
	}
	return c.accessToken(ctx, p, false)
}

// ForceRefresh refreshes p's access token whatever the cached expiry says,
// for a server that refused a token the cache still held fresh. It takes the
// same lock and follows the same rules as AccessToken, so invalid_grant and
// invalid_client still sign out. With SMRIT_TOKEN set there is nothing to
// refresh and it returns ErrNotSignedIn.
func (c *Client) ForceRefresh(ctx context.Context, p profile.Profile) (string, error) {
	if _, ok := HeadlessToken(); ok {
		return "", ErrNotSignedIn
	}
	return c.accessToken(ctx, p, true)
}

// accessToken is AccessToken; force skips the cache and always refreshes.
func (c *Client) accessToken(ctx context.Context, p profile.Profile, force bool) (string, error) {
	if !profile.ValidSlug(p.Org) || p.ClientID == "" || c.Store == nil {
		return "", ErrNotSignedIn
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	if tok, fresh, err := c.cached(p.Org); err != nil {
		return "", fail(ErrUnavailable, err, "%v", err)
	} else if fresh && !force {
		return tok, nil
	}

	unlock, err := c.lock(ctx, p.Org)
	if err != nil {
		return "", err
	}
	defer unlock()
	// Another process may have refreshed while this one waited. A forced
	// refresh has no way to tell that token from the refused one, so it goes on.
	if tok, fresh, err := c.cached(p.Org); err != nil {
		return "", fail(ErrUnavailable, err, "%v", err)
	} else if fresh && !force {
		return tok, nil
	}
	// The profile as it is now, not the caller's copy: a login that finished
	// while this waited has replaced the client and its token endpoint
	// (oauth.py rereads its config inside the lock for the same reason).
	p, err = profile.Pick(p.Org)
	if errors.Is(err, profile.ErrNoProfile) {
		return "", ErrNotSignedIn
	}
	if err != nil {
		return "", fail(ErrUnavailable, err, "%v", err)
	}
	refresh, err := c.Store.Get(refreshKey(p.Org))
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrNotSignedIn
	}
	if err != nil {
		return "", fail(ErrUnavailable, err, "The sign in could not be read: %v", err)
	}
	base, err := c.base()
	if err != nil || !onServer(p.TokenEndpoint, base) {
		return "", fail(ErrUnavailable, err, "The profile for %s names a token endpoint that is not on the Smrit address. Run smrit login --org %s.", p.Org, p.Org)
	}
	deadline, _ := ctx.Deadline()
	if time.Until(deadline) < MinRefreshBudget {
		return "", fail(ErrUnavailable, nil, "Too little time was left to refresh the sign in safely. Your sign in is kept; try again.")
	}
	// Once sent, the server may rotate: a refresh abandoned on the caller's
	// cancel would leave a spent token in the store, and presenting that
	// kills the chain. So the request runs to the deadline, not the cancel.
	sendCtx, cancelSend := context.WithDeadline(context.WithoutCancel(ctx), deadline)
	defer cancelSend()
	t, err := c.postForm(sendCtx, p.TokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {p.ClientID},
	})
	var r *refused
	if errors.As(err, &r) {
		c.forget(p.Org)
		return "", fail(ErrNotSignedIn, nil, "Smrit ended the sign in for %s. Run smrit login --org %s.", p.Org, p.Org)
	}
	if err != nil {
		return "", err
	}
	c.keep(p.Org, t)
	return t.AccessToken, nil
}

// keep stores a refreshed pair, refresh token first: if the process dies
// between the two writes, the next call refreshes with the live token rather
// than presenting the spent one. When the new refresh token cannot be kept,
// the old one is forgotten too, because the server has already spent it and
// presenting it again would kill the chain; the access token still serves its
// hour.
func (c *Client) keep(org string, t tokens) {
	if err := c.Store.Put(refreshKey(org), t.RefreshToken); err != nil {
		c.Store.Delete(refreshKey(org))
	}
	c.putAccess(org, t)
}
