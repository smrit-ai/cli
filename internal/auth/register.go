package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// register makes the CLI an OAuth client (RFC 7591) for one loopback
// redirect URI. A registration confers nothing until a person consents.
func (c *Client) register(ctx context.Context, endpoint, redirectURI string) (string, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	body, _ := json.Marshal(map[string]any{
		"client_name":                ClientName,
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fail(ErrLoginFailed, err, "Could not register this CLI with Smrit.")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http().Do(req)
	if err != nil {
		return "", fail(ErrUnavailable, err, "Could not reach Smrit to register this CLI.")
	}
	defer resp.Body.Close()
	var out struct {
		ClientID string `json:"client_id"`
		Error    string `json:"error"`
	}
	derr := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&out)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return "", fail(ErrUnavailable, nil, "Smrit could not register this CLI just now (HTTP %d). Try again later.", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fail(ErrLoginFailed, nil, "Smrit refused to register this CLI (HTTP %d %s).", resp.StatusCode, safeCode(out.Error))
	}
	if derr != nil || out.ClientID == "" {
		return "", fail(ErrLoginFailed, derr, "Smrit registered this CLI without a client id.")
	}
	return out.ClientID, nil
}

// safeCode keeps an OAuth error code fit to show: the RFC 6749 shape only.
func safeCode(s string) string {
	if len(s) > 64 {
		return ""
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r == '_') {
			return ""
		}
	}
	return s
}
