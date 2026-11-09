package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smrit-ai/cli/internal/auth"
	"github.com/smrit-ai/cli/internal/profile"
)

const (
	fakeToken = "tok_SECRET_123"
	orgID     = "AbCdEfGhIjKlMnOpQrStU"
	orgID2    = "ZyXwVuTsRqPoNmLkJiHgF"
)

type mcpFake struct {
	mu       sync.Mutex
	paths    []string
	auths    []string
	bodies   []map[string]any
	status   int
	rpcError bool
	tool     func(name string, args map[string]any) string
}

func newMCP(t *testing.T) (*mcpFake, *httptest.Server) {
	f := &mcpFake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.bodies = append(f.bodies, m)
		status := f.status
		f.mu.Unlock()
		echo := r.Header.Get("Authorization")
		if status != 0 {
			w.WriteHeader(status)
			io.WriteString(w, "denied for "+echo)
			return
		}
		if f.rpcError {
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Unknown tool.","data":"seen `+echo+`"}}`)
			return
		}
		p := m["params"].(map[string]any)
		args, _ := p["arguments"].(map[string]any)
		out := `{"organization":{"slug":"acme","id":"` + orgID + `","name":"Acme"},"memory":{"you":{"name":"Pat","kind":"user","org_role":"admin"}},"spaces":["handbook"]}`
		if f.tool != nil {
			out = f.tool(p["name"].(string), args)
		}
		txt, _ := json.Marshal(out)
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":`+string(txt)+`}]}}`)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func setup(t *testing.T, srv *httptest.Server, profiles ...profile.Profile) *Services {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SMRIT_TOKEN", "")
	for _, p := range profiles {
		if err := profile.Save(p); err != nil {
			t.Fatal(err)
		}
	}
	return &Services{
		Origin: srv.URL,
		Now:    func() time.Time { return time.Unix(1000, 0) },
		Sleep:  func(context.Context, time.Duration) error { return nil },
		Token:  func(context.Context, profile.Profile) (string, error) { return fakeToken, nil },
		Logout: func(context.Context, profile.Profile) error { return nil },
		Login: func(ctx context.Context, o auth.LoginOptions) (profile.Profile, error) {
			return profile.Profile{}, errors.New("no login in this test")
		},
		Headless: func() (string, bool) { return "", false },
	}
}

func prof(slug, id string) profile.Profile {
	return profile.Profile{Org: slug, OrgID: id, Endpoint: profile.EndpointPath(slug, id), ClientID: "c"}
}

func runCmd(t *testing.T, svc *Services, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), Commands(svc), args, &out, &errb, false)
	return code, out.String(), errb.String()
}

func TestCallPassesHiddenArgs(t *testing.T) {
	f, srv := newMCP(t)
	f.tool = func(string, map[string]any) string { return `{"ok":true}` }
	svc := setup(t, srv, prof("acme", orgID))
	code, out, errs := runCmd(t, svc, "call", "get_document",
		"--json-args", `{"document_id":"a","hidden":{"x":1},"n":1}`,
		"--arg", "document_id=smrit:onboarding", "--arg", "n=2", "--arg", "flag=true", "--arg", "s=007", "--arg", "o={\"k\":[1]}")
	if code != ExitOK {
		t.Fatalf("exit %d %s", code, errs)
	}
	if strings.TrimSpace(out) != `{"ok":true}` {
		t.Fatalf("out %q", out)
	}
	p := f.bodies[0]["params"].(map[string]any)
	if p["name"] != "get_document" {
		t.Fatal(p)
	}
	a := p["arguments"].(map[string]any)
	if a["document_id"] != "smrit:onboarding" || a["n"] != float64(2) || a["flag"] != true || a["s"] != "007" || a["hidden"].(map[string]any)["x"] != float64(1) {
		t.Fatalf("args %v", a)
	}
	if o, _ := a["o"].(map[string]any); o == nil {
		t.Fatalf("object arg %v", a["o"])
	}
	if f.paths[0] != "/mcp/o/acme/"+orgID || f.auths[0] != "Bearer "+fakeToken {
		t.Fatalf("%v %v", f.paths, f.auths)
	}
	if len(f.bodies) != 1 {
		t.Fatal("call must not consult tools/list")
	}
}

func TestCallUsageErrors(t *testing.T) {
	_, srv := newMCP(t)
	svc := setup(t, srv, prof("acme", orgID))
	for _, args := range [][]string{
		{"call"},
		{"call", "a", "b"},
		{"call", "t", "--arg", "novalue"},
		{"call", "t", "--arg", "=v"},
		{"call", "t", "--json-args", "[1]"},
		{"call", "t", "--json-args", "{bad"},
		{"call", "bad name"},
	} {
		if code, _, _ := runCmd(t, svc, args...); code != ExitUsage {
			t.Errorf("%v exit %d", args, code)
		}
	}
}

func TestCallToolErrorExits4(t *testing.T) {
	f, srv := newMCP(t)
	svc := setup(t, srv, prof("acme", orgID))
	_ = f
	f.tool = nil
	f2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"No such document."}]}}`)
	}))
	defer f2.Close()
	svc.Origin = f2.URL
	code, _, errs := runCmd(t, svc, "call", "get_document")
	if code != ExitRefused || !strings.Contains(errs, "No such document.") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestStatusSignedOutExits3(t *testing.T) {
	_, srv := newMCP(t)
	svc := setup(t, srv) // no profile
	code, _, errs := runCmd(t, svc, "status")
	if code != ExitNotSignedIn || !strings.Contains(errs, "smrit login") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestStatus401Exits3(t *testing.T) {
	f, srv := newMCP(t)
	f.status = 401
	svc := setup(t, srv, prof("acme", orgID))
	code, _, errs := runCmd(t, svc, "status")
	if code != ExitNotSignedIn || !strings.Contains(errs, "smrit login") {
		t.Fatalf("%d %q", code, errs)
	}
}

func TestStatusReportsOrganization(t *testing.T) {
	_, srv := newMCP(t)
	svc := setup(t, srv, prof("acme", orgID))
	code, out, errs := runCmd(t, svc, "status")
	if code != ExitOK {
		t.Fatalf("%d %s", code, errs)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(out)
	}
	if m["signed_in"] != true || m["version"] == nil || m["spaces"] == nil || m["you"].(map[string]any)["name"] != "Pat" || m["organization"].(map[string]any)["slug"] != "acme" {
		t.Fatalf("%v", m)
	}
	// text mode is readable
	var o, e bytes.Buffer
	if c := Run(context.Background(), Commands(svc), []string{"--json=false", "status"}, &o, &e, true); c != ExitOK || !strings.Contains(o.String(), "Acme") || !strings.Contains(o.String(), "Pat") {
		t.Fatalf("%d %q", c, o.String())
	}
}

func TestStatusWithoutMemoryHasNoYou(t *testing.T) {
	f, srv := newMCP(t)
	f.tool = func(string, map[string]any) string {
		return `{"organization":{"slug":"acme","id":"` + orgID + `","name":"Acme"},"spaces":["handbook"]}`
	}
	svc := setup(t, srv, prof("acme", orgID))
	code, out, errs := runCmd(t, svc, "status")
	var m map[string]any
	if code != ExitOK || json.Unmarshal([]byte(out), &m) != nil || m["you"] != nil {
		t.Fatalf("%d %q %q", code, out, errs)
	}
	var o, e bytes.Buffer
	if c := Run(context.Background(), Commands(svc), []string{"--json=false", "status"}, &o, &e, true); c != ExitOK || strings.Contains(o.String(), "You:") {
		t.Fatalf("%d %q", c, o.String())
	}
}

func TestSeveralProfilesNeedOrg(t *testing.T) {
	f, srv := newMCP(t)
	svc := setup(t, srv, prof("acme", orgID), prof("beta", orgID2))
	for _, cmd := range []string{"status", "logout", "call"} {
		args := []string{cmd}
		if cmd == "call" {
			args = append(args, "start")
		}
		code, _, errs := runCmd(t, svc, args...)
		if code != ExitUsage || !strings.Contains(errs, "acme") || !strings.Contains(errs, "beta") {
			t.Errorf("%s: %d %q", cmd, code, errs)
		}
	}
	if len(f.paths) != 0 {
		t.Fatal("nothing should be sent")
	}
	code, _, errs := runCmd(t, svc, "status", "--org", "beta")
	if code != ExitOK || f.paths[0] != "/mcp/o/beta/"+orgID2 {
		t.Fatalf("%d %q %v", code, errs, f.paths)
	}
}

func TestHeadlessTokenUsesBareMCP(t *testing.T) {
	f, srv := newMCP(t)
	svc := setup(t, srv)
	svc.Headless = func() (string, bool) { return fakeToken, true }
	code, _, errs := runCmd(t, svc, "status")
	if code != ExitOK || f.paths[0] != "/mcp" || f.auths[0] != "Bearer "+fakeToken {
		t.Fatalf("%d %q %v", code, errs, f.paths)
	}
}

func TestLogout(t *testing.T) {
	_, srv := newMCP(t)
	svc := setup(t, srv, prof("acme", orgID))
	var got string
	svc.Logout = func(_ context.Context, p profile.Profile) error { got = p.Org; return nil }
	code, out, _ := runCmd(t, svc, "logout")
	if code != ExitOK || got != "acme" || !strings.Contains(out, "acme") {
		t.Fatalf("%d %q %q", code, got, out)
	}
	svc.Logout = func(context.Context, profile.Profile) error { return auth.ErrNotRevoked }
	if code, _, errs := runCmd(t, svc, "logout"); code != ExitOK || errs == "" {
		t.Fatalf("not revoked: %d %q", code, errs)
	}
	svc.Logout = func(context.Context, profile.Profile) error { return auth.ErrUnavailable }
	if code, _, _ := runCmd(t, svc, "logout"); code != ExitUnavailable {
		t.Fatalf("unavailable: %d", code)
	}
	svc2 := setup(t, srv)
	if code, _, _ := runCmd(t, svc2, "logout"); code != ExitNotSignedIn {
		t.Fatalf("none: %d", code)
	}
}

func TestLoginVerifiesWithBareMCP(t *testing.T) {
	f, srv := newMCP(t)
	svc := setup(t, srv)
	var opened string
	svc.OpenBrowser = func(u string) error { opened = u; return nil }
	svc.Login = func(ctx context.Context, o auth.LoginOptions) (profile.Profile, error) {
		if o.Org != "acme" {
			t.Errorf("org %q", o.Org)
		}
		_ = o.OpenBrowser("https://example.test/authorize?x=1")
		slug, id, err := o.Verify(ctx, fakeToken)
		if err != nil {
			return profile.Profile{}, err
		}
		return prof(slug, id), nil
	}
	code, out, errs := runCmd(t, svc, "login", "--org", "acme")
	if code != ExitOK {
		t.Fatalf("%d %s", code, errs)
	}
	if f.paths[0] != "/mcp" || f.auths[0] != "Bearer "+fakeToken {
		t.Fatalf("%v %v", f.paths, f.auths)
	}
	if opened == "" || !strings.Contains(errs, "https://example.test/authorize?x=1") {
		t.Fatalf("url must go to stderr too: %q", errs)
	}
	if !strings.Contains(out, "acme") || !strings.Contains(out, "Acme") {
		t.Fatalf("out %q", out)
	}
}

func TestLoginExitCodes(t *testing.T) {
	_, srv := newMCP(t)
	for err, want := range map[error]int{
		auth.ErrOrgMismatch: ExitRefused,
		auth.ErrLoginFailed: ExitRefused,
		auth.ErrUnavailable: ExitUnavailable,
	} {
		svc := setup(t, srv)
		e := err
		svc.Login = func(context.Context, auth.LoginOptions) (profile.Profile, error) { return profile.Profile{}, e }
		if code, _, _ := runCmd(t, svc, "login"); code != want {
			t.Errorf("%v: %d want %d", err, code, want)
		}
	}
}

func TestLoginVerifyErrorHasNoToken(t *testing.T) {
	f, srv := newMCP(t)
	f.status = 500
	svc := setup(t, srv)
	var verr error
	svc.Login = func(ctx context.Context, o auth.LoginOptions) (profile.Profile, error) {
		_, _, verr = o.Verify(ctx, fakeToken)
		return profile.Profile{}, auth.ErrUnavailable
	}
	runCmd(t, svc, "login")
	if verr == nil || strings.Contains(verr.Error(), fakeToken) {
		t.Fatalf("%v", verr)
	}
}

func TestNoCommandPrintsToken(t *testing.T) {
	f, srv := newMCP(t)
	svc := setup(t, srv, prof("acme", orgID))
	svc.OpenBrowser = func(string) error { return nil }
	svc.Login = func(ctx context.Context, o auth.LoginOptions) (profile.Profile, error) {
		_, _, err := o.Verify(ctx, fakeToken)
		if err != nil {
			return profile.Profile{}, err
		}
		return prof("acme", orgID), nil
	}
	f.tool = func(name string, a map[string]any) string {
		return `{"organization":{"slug":"acme","id":"` + orgID + `","name":"Acme"},"spaces":[]}`
	}
	cases := [][]string{
		{"login"}, {"status"}, {"call", "start"}, {"logout"},
		{"login", "--json=false"}, {"status", "--json=false"}, {"call", "start", "--json=false"},
	}
	for _, status := range []int{0, 401, 429, 500} {
		f.mu.Lock()
		f.status = status
		f.mu.Unlock()
		for _, c := range cases {
			_, out, errs := runCmd(t, svc, c...)
			if strings.Contains(out, fakeToken) || strings.Contains(errs, fakeToken) {
				t.Errorf("%v (status %d) printed the token: %q %q", c, status, out, errs)
			}
			// headless too
		}
	}
	f.mu.Lock()
	f.status, f.rpcError = 0, true
	f.mu.Unlock()
	for _, c := range cases {
		_, out, errs := runCmd(t, svc, c...)
		if strings.Contains(out, fakeToken) || strings.Contains(errs, fakeToken) {
			t.Errorf("%v (rpc error) printed the token: %q %q", c, out, errs)
		}
	}
	f.mu.Lock()
	f.rpcError = false
	f.mu.Unlock()
	svc.Headless = func() (string, bool) { return fakeToken, true }
	for _, c := range cases {
		_, out, errs := runCmd(t, svc, c...)
		if strings.Contains(out, fakeToken) || strings.Contains(errs, fakeToken) {
			t.Errorf("headless %v printed the token", c)
		}
	}
}

func TestLoginPrintsURLOnce(t *testing.T) {
	_, srv := newMCP(t)
	svc := setup(t, srv)
	svc.OpenBrowser = func(string) error { return errors.New("no opener") }
	const u = "https://example.test/authorize?x=1"
	svc.Login = func(ctx context.Context, o auth.LoginOptions) (profile.Profile, error) {
		err := o.OpenBrowser(u)
		if err != nil && o.Notice != nil {
			io.WriteString(o.Notice, "Open this address in a browser to sign in:\n"+u+"\n")
		}
		return profile.Profile{}, auth.ErrLoginFailed
	}
	_, _, errs := runCmd(t, svc, "login")
	if n := strings.Count(errs, u); n != 1 {
		t.Fatalf("url printed %d times: %q", n, errs)
	}
}

func TestStoppedContextPrintsASentence(t *testing.T) {
	_, srv := newMCP(t)
	svc := setup(t, srv, prof("acme", orgID))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errb bytes.Buffer
	code := Run(ctx, Commands(svc), []string{"status"}, &out, &errb, false)
	if code != ExitUnavailable || strings.Contains(errb.String(), "context canceled") || !strings.Contains(errb.String(), "Stopped before Smrit answered.") {
		t.Fatalf("%d %q", code, errb.String())
	}
}
