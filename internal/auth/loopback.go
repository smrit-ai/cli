package auth

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// callback is what the browser brought back to the loopback redirect.
type callback struct {
	code  string
	error string
}

// listen opens the loopback listener: on the port of a redirect URI the
// client was registered with, so that registration can be used again, or on
// a random port of 127.0.0.1 when reuse is empty.
func listen(reuse string) (net.Listener, string, error) {
	if reuse != "" {
		u, err := url.Parse(reuse)
		if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.Port() == "" || u.Path != "/callback" {
			return nil, "", fmt.Errorf("not a loopback redirect")
		}
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", u.Port()))
		if err != nil {
			return nil, "", err
		}
		return ln, reuse, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	return ln, fmt.Sprintf("http://127.0.0.1:%d/callback", port), nil
}

const (
	pageSignedIn = "Smrit CLI is signed in. You can close this tab and return to the terminal."
	pageFailed   = "Smrit CLI could not sign in. You can close this tab; the terminal says why."
)

// serveCallback answers the loopback until a callback carrying the expected
// state arrives, and sends it on the returned channel once. Anything else (a
// favicon, a stray probe, a request with the wrong state) is answered and
// ignored, so another page cannot end the sign in early. It is started before
// the browser opens, so an early redirect is never left waiting.
func serveCallback(ln net.Listener, state string) (<-chan callback, func()) {
	got := make(chan callback, 1)
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			http.Error(w, "This is not the sign in this terminal is waiting for.", http.StatusBadRequest)
			return
		}
		cb := callback{code: q.Get("code"), error: q.Get("error")}
		if cb.code == "" && cb.error == "" {
			cb.error = "no_code"
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if cb.code != "" {
			fmt.Fprintln(w, pageSignedIn)
		} else {
			fmt.Fprintln(w, pageFailed)
		}
		once.Do(func() { got <- cb })
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	return got, func() { srv.Close() }
}
