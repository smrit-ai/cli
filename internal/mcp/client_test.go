package mcp

import (
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
)

const fakeToken = "tok_SECRET_123"

type fake struct {
	mu      sync.Mutex
	srv     *httptest.Server
	hits    int
	paths   []string
	headers []http.Header
	bodies  []map[string]any
	handler func(n int, w http.ResponseWriter, r *http.Request)
}

func newFake(t *testing.T, h func(n int, w http.ResponseWriter, r *http.Request)) *fake {
	f := &fake{handler: h}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		n := f.hits
		f.paths = append(f.paths, r.URL.Path)
		f.headers = append(f.headers, r.Header.Clone())
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		f.bodies = append(f.bodies, m)
		f.mu.Unlock()
		f.handler(n, w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func ok(w http.ResponseWriter, result string) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":`+result+`}`)
}

type clock struct {
	t      time.Time
	sleeps []time.Duration
}

func (c *clock) Now() time.Time { return c.t }
func (c *clock) Sleep(ctx context.Context, d time.Duration) error {
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	return nil
}

func client(f *fake, c *clock) *Client {
	return &Client{
		Origin: f.srv.URL, Path: "/mcp/o/acme/AbCdEfGhIjKlMnOpQrStU", Version: "9.9",
		Token: func(context.Context) (string, error) { return fakeToken, nil },
		Now:   c.Now, Sleep: c.Sleep,
	}
}

func TestCallPassesHiddenArgs(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		ok(w, `{"content":[{"type":"text","text":"{\"a\":1}"}],"isError":false}`)
	})
	c := &clock{t: time.Unix(1000, 0)}
	cl := client(f, c)
	args := map[string]any{"document_id": "x", "secret_flag": true, "n": 3}
	got, err := cl.Call(context.Background(), "get_document", args)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":1}` {
		t.Fatalf("got %s", got)
	}
	if f.paths[0] != "/mcp/o/acme/AbCdEfGhIjKlMnOpQrStU" {
		t.Fatalf("path %s", f.paths[0])
	}
	h := f.headers[0]
	if h.Get("Authorization") != "Bearer "+fakeToken || h.Get("Accept") != "application/json, text/event-stream" ||
		h.Get("Content-Type") != "application/json" || h.Get("Smrit-CLI-Version") != "9.9" {
		t.Fatalf("headers %v", h)
	}
	b := f.bodies[0]
	params := b["params"].(map[string]any)
	if b["method"] != "tools/call" || b["jsonrpc"] != "2.0" || params["name"] != "get_document" {
		t.Fatalf("body %v", b)
	}
	a := params["arguments"].(map[string]any)
	if a["secret_flag"] != true || a["document_id"] != "x" || a["n"] != float64(3) {
		t.Fatalf("args %v", a)
	}
	if f.hits != 1 {
		t.Fatalf("hits %d: no tools/list or initialize first", f.hits)
	}
}

func TestStructuredContentPreferred(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		ok(w, `{"content":[{"type":"text","text":"ignored"}],"structuredContent":{"k":"v"}}`)
	})
	got, err := client(f, &clock{}).Call(context.Background(), "t", nil)
	if err != nil || string(got) != `{"k":"v"}` {
		t.Fatalf("%s %v", got, err)
	}
}

func TestPlainTextResultIsAString(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		ok(w, `{"content":[{"type":"text","text":"hello"}]}`)
	})
	got, err := client(f, &clock{}).Call(context.Background(), "t", nil)
	if err != nil || string(got) != `"hello"` {
		t.Fatalf("%s %v", got, err)
	}
}

func TestToolErrorIsRefusal(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		ok(w, `{"content":[{"type":"text","text":"Document not found."}],"isError":true}`)
	})
	_, err := client(f, &clock{}).Call(context.Background(), "t", nil)
	var te *ToolError
	if !errors.As(err, &te) || te.Message != "Document not found." {
		t.Fatalf("%v", err)
	}
}

func TestCallHonours429(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n <= 2 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			return
		}
		ok(w, `{"content":[{"type":"text","text":"1"}]}`)
	})
	c := &clock{t: time.Unix(1000, 0)}
	got, err := client(f, c).Call(context.Background(), "t", nil)
	if err != nil || string(got) != "1" {
		t.Fatalf("%s %v", got, err)
	}
	if len(c.sleeps) != 2 || c.sleeps[0] != 7*time.Second || c.sleeps[1] != 7*time.Second {
		t.Fatalf("sleeps %v", c.sleeps)
	}
}

func TestCall429GivesUpAfterThree(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
	})
	c := &clock{t: time.Unix(1000, 0)}
	_, err := client(f, c).Call(context.Background(), "t", nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	if f.hits != 4 || len(c.sleeps) != 3 {
		t.Fatalf("hits %d sleeps %v", f.hits, c.sleeps)
	}
}

func TestPacingAt120APerMinute(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		ok(w, `{"content":[{"type":"text","text":"1"}]}`)
	})
	c := &clock{t: time.Unix(1000, 0)}
	cl := client(f, c)
	for i := 0; i < 120; i++ {
		if _, err := cl.Call(context.Background(), "t", nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.sleeps) != 0 {
		t.Fatalf("slept inside the budget: %v", c.sleeps)
	}
	if _, err := cl.Call(context.Background(), "t", nil); err != nil {
		t.Fatal(err)
	}
	if len(c.sleeps) != 1 || c.sleeps[0] <= 0 || c.sleeps[0] > time.Second {
		t.Fatalf("sleeps %v", c.sleeps)
	}
}

func TestUnauthorizedIsSignedOut(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	_, err := client(f, &clock{}).Call(context.Background(), "t", nil)
	if !errors.Is(err, ErrSignedOut) {
		t.Fatalf("%v", err)
	}
}

func TestUpgradeRequired(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) { w.WriteHeader(426) })
	_, err := client(f, &clock{}).Call(context.Background(), "t", nil)
	if !errors.Is(err, ErrUpgrade) {
		t.Fatalf("%v", err)
	}
}

func TestServerErrorIsUnavailable(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	_, err := client(f, &clock{}).Call(context.Background(), "t", nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	var other int
	o := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { other++ }))
	defer o.Close()
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, o.URL, 307)
	})
	_, err := client(f, &clock{}).Call(context.Background(), "t", nil)
	if !errors.Is(err, ErrUnavailable) || other != 0 {
		t.Fatalf("%v other=%d", err, other)
	}
}

func TestJSONRPCErrorIsRefusal(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Unknown tool: nope"}}`)
	})
	_, err := client(f, &clock{}).Call(context.Background(), "nope", nil)
	var te *ToolError
	if !errors.As(err, &te) || !strings.Contains(te.Message, "Unknown tool") {
		t.Fatalf("%v", err)
	}
}

func TestEventStreamAnswer(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"structuredContent\":{\"z\":1}}}\n\n")
	})
	got, err := client(f, &clock{}).Call(context.Background(), "t", nil)
	if err != nil || string(got) != `{"z":1}` {
		t.Fatalf("%s %v", got, err)
	}
}

func TestRefusesUnusablePath(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {})
	cl := client(f, &clock{})
	cl.Path = "//evil.example/x"
	if _, err := cl.Call(context.Background(), "t", nil); err == nil || f.hits != 0 {
		t.Fatalf("%v hits %d", err, f.hits)
	}
}

func TestTokenErrorPassesThrough(t *testing.T) {
	f := newFake(t, func(n int, w http.ResponseWriter, r *http.Request) {})
	cl := client(f, &clock{})
	boom := errors.New("boom")
	cl.Token = func(context.Context) (string, error) { return "", boom }
	if _, err := cl.Call(context.Background(), "t", nil); !errors.Is(err, boom) || f.hits != 0 {
		t.Fatalf("%v", err)
	}
}
