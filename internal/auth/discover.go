package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// metadata is the part of RFC 8414 discovery the CLI uses.
type metadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
	RevocationEndpoint    string `json:"revocation_endpoint"`
}

func (c *Client) getJSON(ctx context.Context, u string, v any) error {
	ctx, cancel := bounded(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(v)
}

// discover reads the authorization server metadata at the compiled address.
// Every endpoint it names must be on that same origin: the CLI sends a token
// to the compiled address and nowhere else.
func (c *Client) discover(ctx context.Context, base *url.URL) (metadata, error) {
	var m metadata
	if err := c.getJSON(ctx, base.String()+"/.well-known/oauth-authorization-server", &m); err != nil {
		return m, fail(ErrUnavailable, err, "Could not reach Smrit at %s to sign in.", base)
	}
	for name, v := range map[string]string{
		"authorization": m.AuthorizationEndpoint,
		"token":         m.TokenEndpoint,
		"registration":  m.RegistrationEndpoint,
	} {
		if !onServer(v, base) {
			return m, fail(ErrLoginFailed, nil, "Smrit published no usable %s endpoint on %s.", name, base)
		}
	}
	if m.RevocationEndpoint == "" || !onServer(m.RevocationEndpoint, base) {
		m.RevocationEndpoint = base.String() + "/oauth/revoke"
	}
	return m, nil
}

// resource is the canonical RFC 8707 resource the server publishes for path
// (RFC 9728): the bare /mcp, which leaves the choice of organization to the
// consent screen, or an organization endpoint, which pins the consent to it.
// It must be on the compiled origin with exactly that path; anything else
// fails here, at once, rather than in the browser as invalid_target while the
// CLI waits for a callback.
func (c *Client) resource(ctx context.Context, base *url.URL, path string) (string, error) {
	var doc struct {
		Resource string `json:"resource"`
	}
	if err := c.getJSON(ctx, base.String()+"/.well-known/oauth-protected-resource"+path, &doc); err != nil {
		return "", fail(ErrUnavailable, err, "Could not reach Smrit at %s to sign in.", base)
	}
	u, err := url.Parse(doc.Resource)
	if err != nil || !onServer(doc.Resource, base) || u.Path != path {
		return "", fail(ErrLoginFailed, nil, "Smrit at %s does not publish %s as its own endpoint.", base, path)
	}
	return doc.Resource, nil
}
