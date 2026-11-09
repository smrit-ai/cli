package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/smrit-ai/cli/internal/profile"
	"github.com/smrit-ai/cli/internal/store"
)

const testOrgID = "AbCdEfGhIjKlMnOpQrStU"

// fakeOAuth is the Smrit OAuth server, as much of it as the CLI touches:
// discovery, registration, the token endpoint (code and refresh grants, with
// rotation and replay detection) and revocation. The authorize step is played
// by the browser function, which reads the URL and redirects to the loopback.
type fakeOAuth struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	registered    []map[string]any
	authorizeURLs []url.Values
	challenges    map[string]string // code -> challenge
	redirects     map[string]string // code -> redirect_uri
	clients       int
	issued        int
	current       string // the live refresh token
	spent         map[string]bool
	tokenCalls    int
	refreshCalls  int
	revoked       []url.Values
	accessTokens  []string
	refreshTokens []string

	// onRefresh, when set, runs inside the refresh handler with the call's
	// ordinal before the default answer; returning true means it answered.
	onRefresh func(n int, w http.ResponseWriter, r *http.Request) bool
	// revokeStatus, when not zero, is what /oauth/revoke answers.
	revokeStatus int
	// busy receives a value for every GET /test/busy.
	busy chan struct{}
	// meta overrides discovery fields; resourceFor overrides the published
	// resource for a path; refreshClients records each refresh's client_id;
	// redirect, when set, answers that path with a 307 to its value.
	meta           map[string]string
	resourceFor    func(path string) string
	refreshClients []string
	redirect       map[string]string
}

func newFake(t *testing.T) *fakeOAuth {
	t.Helper()
	f := &fakeOAuth{
		t:          t,
		challenges: map[string]string{},
		redirects:  map[string]string{},
		spent:      map[string]bool{},
		busy:       make(chan struct{}, 16),
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeOAuth) serve(w http.ResponseWriter, r *http.Request) {
	origin := f.srv.URL
	f.mu.Lock()
	to, redirected := f.redirect[r.URL.Path]
	overrides := f.meta
	resourceFor := f.resourceFor
	f.mu.Unlock()
	if redirected {
		w.Header().Set("Location", to)
		w.WriteHeader(http.StatusTemporaryRedirect)
		return
	}
	switch {
	case r.URL.Path == "/.well-known/oauth-authorization-server":
		meta := map[string]any{
			"issuer":                           origin,
			"authorization_endpoint":           origin + "/oauth/authorize",
			"token_endpoint":                   origin + "/oauth/token",
			"registration_endpoint":            origin + "/oauth/register",
			"revocation_endpoint":              origin + "/oauth/revoke",
			"code_challenge_methods_supported": []string{"S256"},
		}
		for k, v := range overrides {
			meta[k] = v
		}
		writeJSON(w, 200, meta)
	case strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource"):
		path := strings.TrimPrefix(r.URL.Path, "/.well-known/oauth-protected-resource")
		if path == "" {
			path = "/mcp"
		}
		resource := origin + path
		if resourceFor != nil {
			resource = resourceFor(path)
		}
		writeJSON(w, 200, map[string]any{"resource": resource})
	case r.URL.Path == "/oauth/register" && r.Method == http.MethodPost:
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.registered = append(f.registered, body)
		f.clients++
		id := fmt.Sprintf("client-%d", f.clients)
		f.mu.Unlock()
		writeJSON(w, 201, map[string]any{"client_id": id, "redirect_uris": body["redirect_uris"]})
	case r.URL.Path == "/oauth/token" && r.Method == http.MethodPost:
		f.token(w, r)
	case r.URL.Path == "/oauth/revoke" && r.Method == http.MethodPost:
		r.ParseForm()
		f.mu.Lock()
		f.revoked = append(f.revoked, r.PostForm)
		status := f.revokeStatus
		f.mu.Unlock()
		if status == 0 {
			status = 200
		}
		writeJSON(w, status, map[string]any{})
	case r.URL.Path == "/test/busy":
		f.busy <- struct{}{}
		w.WriteHeader(204)
	default:
		http.NotFound(w, r)
	}
}

// pair issues a new access and refresh token and makes the refresh token the
// live one. The caller holds f.mu.
func (f *fakeOAuth) pair(expiresIn int) map[string]any {
	f.issued++
	access := fmt.Sprintf("access-%d-x", f.issued)
	refresh := fmt.Sprintf("refresh-%d-y", f.issued)
	if f.current != "" {
		f.spent[f.current] = true
	}
	f.current = refresh
	f.accessTokens = append(f.accessTokens, access)
	f.refreshTokens = append(f.refreshTokens, refresh)
	return map[string]any{
		"access_token": access, "refresh_token": refresh, "token_type": "Bearer",
		"expires_in": expiresIn, "scope": "write",
	}
}

func (f *fakeOAuth) token(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	form := r.PostForm
	f.mu.Lock()
	f.tokenCalls++
	switch form.Get("grant_type") {
	case "authorization_code":
		code := form.Get("code")
		challenge, ok := f.challenges[code]
		delete(f.challenges, code)
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge ||
			form.Get("redirect_uri") != f.redirects[code] || form.Get("client_id") == "" {
			f.mu.Unlock()
			writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
			return
		}
		tokens := f.pair(3600)
		f.mu.Unlock()
		writeJSON(w, 200, tokens)
	case "refresh_token":
		f.refreshCalls++
		f.refreshClients = append(f.refreshClients, form.Get("client_id"))
		n := f.refreshCalls
		hook := f.onRefresh
		f.mu.Unlock()
		if hook != nil && hook(n, w, r) {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		presented := form.Get("refresh_token")
		if presented != f.current || f.spent[presented] {
			writeJSON(w, 400, map[string]any{"error": "invalid_grant"})
			return
		}
		writeJSON(w, 200, f.pair(3600))
	default:
		f.mu.Unlock()
		writeJSON(w, 400, map[string]any{"error": "unsupported_grant_type"})
	}
}

// browser plays the person at the consent screen: it records the authorize
// query, mints a code bound to the challenge and follows the redirect.
func (f *fakeOAuth) browser(authURL string) error {
	u, err := url.Parse(authURL)
	if err != nil {
		return err
	}
	q := u.Query()
	f.mu.Lock()
	f.authorizeURLs = append(f.authorizeURLs, q)
	code := fmt.Sprintf("code-%d", len(f.authorizeURLs))
	f.challenges[code] = q.Get("code_challenge")
	f.redirects[code] = q.Get("redirect_uri")
	f.mu.Unlock()
	back := q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	resp, err := http.Get(back)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (f *fakeOAuth) setOnRefresh(h func(n int, w http.ResponseWriter, r *http.Request) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onRefresh = h
}

// set runs change under the fake's lock.
func (f *fakeOAuth) set(change func(f *fakeOAuth)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeOAuth) setRevokeStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokeStatus = status
}

// seen is a copy of what the fake has recorded, taken under its lock.
type seen struct {
	registered              []map[string]any
	revoked                 []url.Values
	accessTokens, refreshes []string
	refreshClients          []string
}

func (f *fakeOAuth) seen() seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return seen{
		registered:     append([]map[string]any(nil), f.registered...),
		revoked:        append([]url.Values(nil), f.revoked...),
		accessTokens:   append([]string(nil), f.accessTokens...),
		refreshes:      append([]string(nil), f.refreshTokens...),
		refreshClients: append([]string(nil), f.refreshClients...),
	}
}

func (f *fakeOAuth) lastAuthorize() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authorizeURLs[len(f.authorizeURLs)-1]
}

func (f *fakeOAuth) counts() (token, refresh int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls, f.refreshCalls
}

// elsewhere is a second server that must never be reached: it counts every
// request and the body it carried.
type elsewhere struct {
	srv    *httptest.Server
	mu     sync.Mutex
	hits   int
	bodies []string
}

func newElsewhere(t *testing.T) *elsewhere {
	t.Helper()
	e := &elsewhere{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.hits++
		e.bodies = append(e.bodies, string(b))
		e.mu.Unlock()
		writeJSON(w, 200, map[string]any{"access_token": "stolen-a", "refresh_token": "stolen-r", "client_id": "stolen", "resource": "x"})
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *elsewhere) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hits
}

func verifyAs(slug, id string) func(context.Context, string) (string, string, error) {
	return func(context.Context, string) (string, string, error) { return slug, id, nil }
}

// setHome points the state directory at a fresh temporary home.
func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv(HeadlessEnv, "")
	return home
}

func newClient(t *testing.T, f *fakeOAuth) *Client {
	t.Helper()
	return &Client{Server: f.srv.URL, Store: store.NewFile(t.TempDir()), HTTP: f.srv.Client()}
}

// seed signs acme in without a browser: a profile, a live refresh token on the
// fake server and an access token that has already expired.
func seed(t *testing.T, c *Client, f *fakeOAuth) profile.Profile {
	t.Helper()
	f.mu.Lock()
	f.current = "refresh-0-y"
	f.mu.Unlock()
	p := profile.Profile{
		Org:                "acme",
		OrgID:              testOrgID,
		Endpoint:           profile.EndpointPath("acme", testOrgID),
		ClientID:           "client-1",
		RedirectURI:        "http://127.0.0.1:1/callback",
		TokenEndpoint:      f.srv.URL + "/oauth/token",
		RevocationEndpoint: f.srv.URL + "/oauth/revoke",
	}
	if err := profile.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.Put(refreshKey("acme"), "refresh-0-y"); err != nil {
		t.Fatal(err)
	}
	if err := c.Store.Put(accessKey("acme"), "100:access-0-x"); err != nil {
		t.Fatal(err)
	}
	return p
}
