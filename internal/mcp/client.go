// Package mcp calls tools on the Smrit MCP server over its stateless JSON
// transport: one POST per call, no session, no initialize, no tools/list.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/smrit-ai/cli/internal/profile"
)

const (
	// CallsPerMinute is the most one process sends.
	CallsPerMinute = 120
	// MaxRetries is how many times a 429 is waited out before giving up.
	MaxRetries = 3
	// MaxWait bounds a Retry-After the server asks for.
	MaxWait = 60 * time.Second
	// RequestTimeout bounds one request.
	RequestTimeout = 60 * time.Second
	maxBody        = 16 << 20
)

var (
	// ErrSignedOut means the server refused the credential.
	ErrSignedOut = errors.New("Not signed in. Run smrit login.")
	// ErrUnavailable means Smrit could not give an answer.
	ErrUnavailable = errors.New("Smrit could not be reached. Try again.")
	// ErrUpgrade means the server needs a newer CLI.
	ErrUpgrade = errors.New("This version of smrit is too old for the server. Upgrade smrit.")
)

// errRateLimited is what a 429 that outlasted the retries says. It is an
// ErrUnavailable with a sentence of its own.
var errRateLimited = &rateLimited{}

type rateLimited struct{}

func (*rateLimited) Error() string {
	return "Smrit is limiting how fast this client can call. Try again in a minute."
}
func (*rateLimited) Is(target error) bool { return target == ErrUnavailable }

// ToolError is a refusal from the tool or the protocol, with the server's words.
type ToolError struct{ Message string }

func (e *ToolError) Error() string { return e.Message }

// Client calls tools at Origin+Path. Origin is the compiled address and Path
// a validated /mcp path; the URL is built by concatenation, never resolved.
type Client struct {
	Origin  string
	Path    string
	Version string
	// Token returns the bearer token for one call.
	Token func(ctx context.Context) (string, error)
	HTTP  *http.Client
	// Now and Sleep are the clock; nil means the real ones.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error

	mu    sync.Mutex
	calls []time.Time // when each call in the last minute was sent
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

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

// pace takes one slot of the budget: at most CallsPerMinute calls in any
// 60 second window, waiting for the oldest call to leave the window when full.
func (c *Client) pace(ctx context.Context) error {
	for {
		c.mu.Lock()
		now := c.now()
		keep := c.calls[:0]
		for _, t := range c.calls {
			if now.Sub(t) < time.Minute {
				keep = append(keep, t)
			}
		}
		c.calls = keep
		if len(c.calls) < CallsPerMinute {
			c.calls = append(c.calls, now)
			c.mu.Unlock()
			return nil
		}
		wait := c.calls[0].Add(time.Minute).Sub(now)
		c.mu.Unlock()
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		if err := c.sleep(ctx, wait); err != nil {
			return stopped(err)
		}
	}
}

// stopped is the sentence for a cancelled or timed out context.
type stoppedError struct{ cause error }

func (e *stoppedError) Error() string { return "Stopped before Smrit answered." }
func (e *stoppedError) Unwrap() error { return e.cause }

func stopped(cause error) error { return &stoppedError{cause} }

func validPath(p string) bool {
	if p == "/mcp" {
		return true
	}
	rest, ok := strings.CutPrefix(p, "/mcp/o/")
	if !ok {
		return false
	}
	slug, id, ok := strings.Cut(rest, "/")
	return ok && profile.ValidSlug(slug) && profile.ValidID(id) && p == profile.EndpointPath(slug, id)
}

// retryAfter reads Retry-After as seconds or as an HTTP date, at least one
// second and at most MaxWait.
func retryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	d := time.Second
	if n, err := strconv.Atoi(h); err == nil {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(h); err == nil {
		d = t.Sub(now)
	}
	if d < time.Second {
		d = time.Second
	}
	if d > MaxWait {
		d = MaxWait
	}
	return d
}

// Call runs one tool and returns its structured result: structuredContent
// when the server sent it, otherwise the first text block parsed as JSON,
// otherwise that text as a JSON string.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	if !validPath(c.Path) {
		return nil, errors.New("The Smrit endpoint is not usable. Sign in again.")
	}
	if args == nil {
		args = map[string]any{}
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return nil, fmt.Errorf("The arguments could not be encoded: %v", err)
	}
	for attempt := 0; ; attempt++ {
		if err := c.pace(ctx); err != nil {
			return nil, err
		}
		tok, err := c.Token(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Origin+c.Path, bytes.NewReader(body))
		if err != nil {
			return nil, ErrUnavailable
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Smrit-CLI-Version", c.Version)
		resp, err := c.http().Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, stopped(ctx.Err())
			}
			return nil, ErrUnavailable
		}
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			if attempt >= MaxRetries {
				return nil, errRateLimited
			}
			if err := c.sleep(ctx, retryAfter(resp.Header.Get("Retry-After"), c.now())); err != nil {
				return nil, stopped(err)
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized:
			return nil, ErrSignedOut
		case resp.StatusCode == http.StatusUpgradeRequired:
			return nil, ErrUpgrade
		case resp.StatusCode != http.StatusOK || rerr != nil:
			return nil, ErrUnavailable
		}
		return decode(raw, resp.Header.Get("Content-Type"))
	}
}

func decode(raw []byte, contentType string) (json.RawMessage, error) {
	if strings.HasPrefix(contentType, "text/event-stream") {
		sc := bufio.NewScanner(bytes.NewReader(raw))
		sc.Buffer(make([]byte, 64<<10), maxBody)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
				raw = []byte(strings.TrimSpace(data))
				break
			}
		}
	}
	var env struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Structured json.RawMessage `json:"structuredContent"`
			IsError    bool            `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%w Smrit sent an answer this CLI cannot read.", ErrUnavailable)
	}
	if env.Error != nil {
		return nil, &ToolError{Message: env.Error.Message}
	}
	if env.Result == nil {
		return nil, fmt.Errorf("%w Smrit sent an answer this CLI cannot read.", ErrUnavailable)
	}
	text := ""
	for _, b := range env.Result.Content {
		if b.Type == "text" {
			text = b.Text
			break
		}
	}
	if env.Result.IsError {
		if text == "" {
			text = "The tool refused the call."
		}
		return nil, &ToolError{Message: text}
	}
	if len(env.Result.Structured) > 0 && string(env.Result.Structured) != "null" {
		return env.Result.Structured, nil
	}
	if json.Valid([]byte(text)) {
		return json.RawMessage(text), nil
	}
	b, _ := json.Marshal(text)
	return b, nil
}
