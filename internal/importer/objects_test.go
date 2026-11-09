package importer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/smrit-ai/cli/internal/plan"
	"github.com/smrit-ai/cli/internal/state"
)

// pngBytes is enough of a PNG for the scan to call it an object.
var pngBytes = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x01\x02\x03"

func objectEntry(p string) plan.PlanEntry {
	return plan.PlanEntry{Path: p, Kind: "object", Scope: "organization"}
}

// recorder is a server that must never be reached.
func recorder(t *testing.T, tls bool) (*httptest.Server, func() int) {
	var mu sync.Mutex
	n := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	var s *httptest.Server
	if tls {
		s = httptest.NewTLSServer(h)
	} else {
		s = httptest.NewServer(h)
	}
	t.Cleanup(s.Close)
	return s, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

// noCredential fails if anything a run wrote or reported carries the
// bearer token or an upload token.
func noCredential(t *testing.T, stateDir string, rep RunReport) {
	t.Helper()
	out, _ := json.Marshal(rep)
	var saved []byte
	filepath.WalkDir(stateDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			saved = append(saved, b...)
		}
		return nil
	})
	for _, where := range []string{string(out), string(saved)} {
		if strings.Contains(where, token) || strings.Contains(where, "uploadtoken") || strings.Contains(where, "/u/") {
			t.Fatalf("a credential was written or reported:\n%s", where)
		}
	}
}

func TestObjectUploadNeverCarriesToken(t *testing.T) {
	root := t.TempDir()
	write(t, root, "img/logo.png", pngBytes)
	f := newStore(t)
	p := plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{objectEntry("img/logo.png")}}
	dir := t.TempDir()
	rep := mustRun(t, p, f.opts(dir))

	e := entryOf(t, rep, "img/logo.png")
	if e.Status != state.StatusCreated || e.ID == "" || e.Error != "" || rep.Created != 1 {
		t.Fatalf("%+v", e)
	}
	calls := f.toolCalls("create_object")
	if len(calls) != 1 || calls[0].Args["name"] != "logo.png" || calls[0].Args["mime_type"] != "image/png" || calls[0].Args["scope"] != "organization" {
		t.Fatalf("create_object %+v", calls)
	}
	f.mu.Lock()
	puts := f.ext.puts
	obj := f.ext.objects[e.ID]
	f.mu.Unlock()
	if len(puts) != 1 {
		t.Fatalf("%d PUTs", len(puts))
	}
	put := puts[0]
	if put.Method != http.MethodPut || string(put.Body) != pngBytes || !obj.Arrived {
		t.Fatalf("%s %q", put.Method, put.Body)
	}
	if put.Header.Get("Authorization") != "" || put.Header.Get("Cookie") != "" {
		t.Fatalf("the upload carried a credential: %v", put.Header)
	}
	for k, vs := range put.Header {
		for _, v := range vs {
			if strings.Contains(v, token) {
				t.Fatalf("header %s carries the token", k)
			}
		}
	}
	if put.Header.Get("Content-Type") != "image/png" || put.Header.Get("Content-Length") != fmt.Sprint(len(pngBytes)) {
		t.Fatalf("headers %v", put.Header)
	}
	noCredential(t, dir, rep)
	st, err := state.LoadRun(dir, rep.Run)
	if err != nil || st.Entries["img/logo.png"].ID != e.ID || st.Entries["img/logo.png"].Status != state.StatusCreated {
		t.Fatalf("%v %+v", err, st)
	}
}

func TestObjectUploadRefusesOtherHosts(t *testing.T) {
	plainTrove, plainHits := recorder(t, false)
	other, otherHits := recorder(t, true)
	cases := []struct {
		name string
		set  func(f *storeFake, o *RunOptions)
		// puts is how many PUTs the store's own trove should see.
		puts int
	}{
		{"http on the trove host", func(f *storeFake, o *RunOptions) {
			// The compiled origin is https on the host the plain server
			// listens on, so a send over http would reach it.
			o.trove = "https://" + plainTrove.Listener.Addr().String()
			f.ext.uploadURL = func(tok string) string { return plainTrove.URL + "/u/" + tok }
		}, 0},
		{"another host", func(f *storeFake, o *RunOptions) {
			f.ext.uploadURL = func(tok string) string { return other.URL + "/u/" + tok }
		}, 0},
		{"credentials in the URL", func(f *storeFake, o *RunOptions) {
			f.ext.uploadURL = func(tok string) string {
				return strings.Replace(f.ext.trove.URL, "https://", "https://me:pw@", 1) + "/u/" + tok
			}
		}, 0},
		{"a redirect", func(f *storeFake, o *RunOptions) {
			f.ext.redirectTo = other.URL + "/u/elsewhere"
		}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "logo.png", pngBytes)
			f := newStore(t)
			dir := t.TempDir()
			o := f.opts(dir)
			c.set(f, &o)
			p := plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{objectEntry("logo.png")}}
			rep := mustRun(t, p, o)
			e := entryOf(t, rep, "logo.png")
			if e.Status != state.StatusFailed || e.Error == "" || e.ID == "" {
				t.Fatalf("%+v", e)
			}
			f.mu.Lock()
			puts := len(f.ext.puts)
			f.mu.Unlock()
			if puts != c.puts || plainHits() != 0 || otherHits() != 0 {
				t.Fatalf("trove %d, plain %d, other %d", puts, plainHits(), otherHits())
			}
			noCredential(t, dir, rep)
			if strings.Contains(e.Error, "127.0.0.1") || strings.Contains(e.Error, "http") {
				t.Fatalf("the error names the address: %s", e.Error)
			}
		})
	}
}

func TestObjectUploadRetriedOnceThroughGetObject(t *testing.T) {
	root := t.TempDir()
	write(t, root, "logo.png", pngBytes)
	p := plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{objectEntry("logo.png")}}

	f := newStore(t)
	f.ext.putStatus = []int{500}
	rep := mustRun(t, p, f.opts(t.TempDir()))
	if e := entryOf(t, rep, "logo.png"); e.Status != state.StatusCreated || len(f.toolCalls("get_object")) != 1 || len(f.ext.puts) != 2 {
		t.Fatalf("%+v get_object %d puts %d", e, len(f.toolCalls("get_object")), len(f.ext.puts))
	}
	if f.ext.puts[0].Path == f.ext.puts[1].Path {
		t.Fatal("the retry did not use the fresh upload URL")
	}

	// Two failures: failed, with the id kept, and no third attempt.
	f = newStore(t)
	f.ext.putStatus = []int{500, 503, 204}
	dir := t.TempDir()
	rep = mustRun(t, p, f.opts(dir))
	e := entryOf(t, rep, "logo.png")
	if e.Status != state.StatusFailed || e.ID == "" || len(f.ext.puts) != 2 || rep.Failed != 1 {
		t.Fatalf("%+v puts %d", e, len(f.ext.puts))
	}
	noCredential(t, dir, rep)

	// A resume finishes that object through get_object, never a second one.
	o := f.opts(dir)
	o.Resume = rep.Run
	again := mustRun(t, p, o)
	if a := entryOf(t, again, "logo.png"); a.Status != state.StatusCreated || a.ID != e.ID || len(f.toolCalls("create_object")) != 1 {
		t.Fatalf("%+v create_object %d", a, len(f.toolCalls("create_object")))
	}
}

func TestObjectResumeSkipsCreatedRerunMakesAnother(t *testing.T) {
	root := t.TempDir()
	write(t, root, "logo.png", pngBytes)
	f := newStore(t)
	p := plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{objectEntry("logo.png")}}
	dir := t.TempDir()
	first := mustRun(t, p, f.opts(dir))

	o := f.opts(dir)
	o.Resume = first.Run
	resumed := mustRun(t, p, o)
	if e := entryOf(t, resumed, "logo.png"); e.Status != state.StatusCreated || e.ID != entryOf(t, first, "logo.png").ID {
		t.Fatalf("%+v", e)
	}
	if len(f.toolCalls("create_object")) != 1 || len(f.ext.puts) != 1 {
		t.Fatal("a resume stored the object again")
	}

	// With no run state there is nothing to know the object by: objects
	// have no slug, so a rerun stores a second one, and says created.
	again := mustRun(t, p, f.opts(t.TempDir()))
	if e := entryOf(t, again, "logo.png"); e.Status != state.StatusCreated || e.ID == entryOf(t, first, "logo.png").ID {
		t.Fatalf("%+v", e)
	}
}

func TestObjectTypeFromPlanOrExtension(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.png", pngBytes)
	write(t, root, "b.bin", "\x00\x01\x02binary")
	write(t, root, "c.png", pngBytes)
	f := newStore(t)
	a, b, c := objectEntry("a.png"), objectEntry("b.bin"), objectEntry("c.png")
	a.Type = "image/x-custom"
	c.Type = "not a type"
	p := plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{a, b, c}}
	rep := mustRun(t, p, f.opts(t.TempDir()))
	calls := f.toolCalls("create_object")
	if len(calls) != 2 || calls[0].Args["mime_type"] != "image/x-custom" || calls[1].Args["mime_type"] != "application/octet-stream" {
		t.Fatalf("%+v", calls)
	}
	if f.ext.puts[0].Header.Get("Content-Type") != "image/x-custom" {
		t.Fatalf("%v", f.ext.puts[0].Header)
	}
	if e := entryOf(t, rep, "c.png"); e.Status != state.StatusRefused || e.Error == "" {
		t.Fatalf("%+v", e)
	}
}

// Objects are stored first, so a document in the same run shows an image
// of a plan object by its reference, in the first bytes it sends.
func TestObjectsBeforeDocuments(t *testing.T) {
	root := t.TempDir()
	write(t, root, "img/logo.png", pngBytes)
	write(t, root, "a.md", "# A\n![Logo](img/logo.png \"the logo\")\n")
	f := newStore(t)
	p := docsPlan(root, "a.md")
	p.Entries = append(p.Entries, objectEntry("img/logo.png"))
	rep := mustRun(t, p, f.opts(t.TempDir()))
	obj := entryOf(t, rep, "img/logo.png")
	if obj.Status != state.StatusCreated || obj.ID == "" {
		t.Fatalf("%+v", obj)
	}
	want := "# A\n![Logo](smrit:object/" + obj.ID + " \"the logo\")\n"
	if got := f.firstSent("a.md"); got != want {
		t.Fatalf("first sent %q, want %q", got, want)
	}
	if f.committed("a.md") != 1 {
		t.Fatalf("a.md committed %d times", f.committed("a.md"))
	}
	f.mu.Lock()
	var mapBody string
	for _, d := range f.docs {
		if strings.HasPrefix(d.Title, "Import map") {
			mapBody = d.Body
		}
	}
	f.mu.Unlock()
	if !strings.Contains(mapBody, "| img/logo.png | smrit:object/"+obj.ID+" |") {
		t.Fatalf("map %q", mapBody)
	}
}

// The upload client never follows a redirect even when handed one that
// would, and sends no cookie from any jar.
func TestUploaderNeverFollowsOrCarriesCookies(t *testing.T) {
	other, hits := recorder(t, true)
	trove := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			t.Error("cookie sent")
		}
		io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer trove.Close()
	c := trove.Client()
	c.CheckRedirect = nil
	r := &runner{o: RunOptions{trove: trove.URL, uploadHTTP: c}}
	r.ctx = t.Context()
	reason, err := r.put(trove.URL+"/u/x", []byte("abc"), "text/plain")
	if err != nil || reason == "" || hits() != 0 || bytes.Contains([]byte(reason), []byte(trove.URL)) {
		t.Fatalf("%q %v %d", reason, err, hits())
	}
}
