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
	} else if fresh {
		return tok, nil
	}

	unlock, err := c.lock(ctx, p.Org)
	if err != nil {
		return "", err
	}
	defer unlock()
	// Another process may have refreshed while this one waited.
	if tok, fresh, err := c.cached(p.Org); err != nil {
		return "", fail(ErrUnavailable, err, "%v", err)
	} else if fresh {
		return tok, nil
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
	t, err := c.postForm(ctx, p.TokenEndpoint, url.Values{
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
