package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smrit-ai/cli/internal/mcp"
	"github.com/smrit-ai/cli/internal/plan"
	"github.com/smrit-ai/cli/internal/scan"
)

const (
	token  = "tok_SECRET_123"
	mcpURL = "/mcp/o/acme/AbCdEfGhIjKlMnOpQrStU"
)

// fakeSmrit stands in for the MCP endpoint and the import routes.
type fakeSmrit struct {
	mu       sync.Mutex
	paths    []string
	auths    []string
	versions []string
	previews []received
	tools    []string
	status   int // non-zero: every route answers it
	preview  func(r received) []PreviewItem
	srv      *httptest.Server
}

type received struct {
	Space string
	Paths []string
	Files map[string]string
	// Dispositions are the Content-Disposition headers of the file parts.
	Dispositions []string
	Plan         struct {
		Files []map[string]string `json:"files"`
	}
}

func newFake(t *testing.T) *fakeSmrit {
	f := &fakeSmrit{tools: []string{"spaces", "create_object"}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.versions = append(f.versions, r.Header.Get("Smrit-CLI-Version"))
		if r.URL.Path == mcpURL {
			b, _ := io.ReadAll(r.Body)
			var m struct {
				Method string `json:"method"`
			}
			json.Unmarshal(b, &m)
			w.Header().Set("Content-Type", "application/json")
			if m.Method == "tools/list" {
				var ts []string
				for _, n := range f.tools {
					ts = append(ts, `{"name":"`+n+`"}`)
				}
				io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[`+strings.Join(ts, ",")+`]}}`)
				return
			}
			out := `{"org":{"id":"AbCdEfGhIjKlMnOpQrStU","name":"Acme","slug":"acme"},"spaces":[` +
				`{"id":"SpaceIdEngineering0001","slug":"engineering","name":"Engineering","visibility":"private"},` +
				`{"id":"SpaceIdPublicDocs00002","slug":"docs","name":"Docs","visibility":"public"}],"types":["decision","note"]}`
			txt, _ := json.Marshal(out)
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":`+string(txt)+`}]}}`)
			return
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
			io.WriteString(w, `{"error":"x","detail":"A sentence from the route.","reason":"public_space"}`)
			return
		}
		space := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/cli/v1/spaces/"), "/import/preview")
		rec := received{Space: space, Files: map[string]string{}}
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		var datas []string
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			switch p.FormName() {
			case "files":
				rec.Dispositions = append(rec.Dispositions, p.Header.Get("Content-Disposition"))
				datas = append(datas, string(b))
			case "paths":
				rec.Paths = append(rec.Paths, string(b))
			case "plan":
				json.Unmarshal(b, &rec.Plan)
			}
		}
		for i, p := range rec.Paths {
			rec.Files[p] = datas[i]
		}
		f.previews = append(f.previews, rec)
		items := []PreviewItem{}
		if f.preview != nil {
			items = f.preview(rec)
		} else {
			for _, p := range rec.Paths {
				items = append(items, PreviewItem{RelativePath: p, Action: "create"})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": items})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSmrit) client() *mcp.Client {
	return &mcp.Client{
		Origin: f.srv.URL, Path: mcpURL, Version: "9.9",
		Token: func(context.Context) (string, error) { return token, nil },
		Now:   func() time.Time { return time.Unix(1000, 0) },
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
}

func (f *fakeSmrit) input(p plan.Plan) Input {
	c := f.client()
	return Input{Plan: p, MCP: c, Route: &Route{MCP: c}}
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func docEntry(p string) plan.PlanEntry {
	return plan.PlanEntry{Path: p, Kind: "document", Space: "engineering", Type: "decision"}
}

func TestBatchesAt25FilesOr8MiB(t *testing.T) {
	var es []plan.Entry
	for i := 0; i < 60; i++ {
		es = append(es, plan.Entry{Index: i, Space: "a", Size: 1})
	}
	got := Batches(es)
	if len(got) != 3 || len(got[0]) != 25 || len(got[1]) != 25 || len(got[2]) != 10 {
		t.Fatalf("count batches: %d", len(got))
	}
	// Bytes: three 3 MiB files do not fit in 8 MiB together.
	es = nil
	for i := 0; i < 3; i++ {
		es = append(es, plan.Entry{Index: i, Space: "a", Size: 3 << 20})
	}
	got = Batches(es)
	if len(got) != 2 || len(got[0]) != 2 || len(got[1]) != 1 {
		t.Fatalf("size batches: %v", got)
	}
	// Exactly 8 MiB fits; batches never mix spaces and keep plan order.
	es = []plan.Entry{{Index: 0, Space: "a", Size: 4 << 20}, {Index: 1, Space: "b", Size: 1}, {Index: 2, Space: "a", Size: 4 << 20}}
	got = Batches(es)
	if len(got) != 2 || got[0][0].Space != "a" || len(got[0]) != 2 || got[1][0].Space != "b" {
		t.Fatalf("space batches: %v", got)
	}
}

func TestDryRunNamesOrgFirstAndSendsPlanFields(t *testing.T) {
	root := t.TempDir()
	write(t, root, "adr/0001.md", "# Use Postgres\nbody\n")
	write(t, root, "tasks.csv", "a,b\n1,2\n")
	write(t, root, "logo.png", "\x89PNG\x00\x01")
	f := newFake(t)
	f.preview = func(r received) []PreviewItem {
		return []PreviewItem{{RelativePath: "adr/0001.md", Action: "update", Slug: "adr-0001", Type: "note", TypeFrom: "decison", Title: "Use Postgres",
			Similar: []Similar{{DocumentID: "D", Slug: "adr-0001-copy", Title: "Copy"}}}}
	}
	p := plan.Plan{Root: root, Entries: []plan.PlanEntry{
		docEntry("adr/0001.md"),
		{Path: "tasks.csv", Kind: "workbook", Space: "engineering", Title: "Tasks"},
		{Path: "logo.png", Kind: "object", Scope: "organization"},
	}}
	rep, err := DryRun(context.Background(), f.input(p))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Organization.Name != "Acme" || rep.Organization.Slug != "acme" {
		t.Fatalf("%+v", rep.Organization)
	}
	if rep.WouldUpdate != 1 || rep.WouldCreate != 2 || rep.Refused != 0 {
		t.Fatalf("%+v", rep)
	}
	e := rep.Entries[0]
	if e.TypeFrom != "decison" || e.Type != "note" || len(e.Similar) != 1 || e.Slug != "adr-0001" {
		t.Fatalf("%+v", e)
	}
	if rep.Entries[1].Action != "create" || rep.Entries[2].Action != "create" {
		t.Fatalf("%+v", rep.Entries)
	}
	// Only the document went to the preview route, with the plan's fields.
	if len(f.previews) != 1 || f.previews[0].Space != "engineering" || f.previews[0].Files["adr/0001.md"] != "# Use Postgres\nbody\n" {
		t.Fatalf("%+v", f.previews)
	}
	pf := f.previews[0].Plan.Files[0]
	if pf["relative_path"] != "adr/0001.md" || pf["type"] != "decision" || pf["title"] != "Use Postgres" || pf["slug"] != "adr-0001" {
		t.Fatalf("%v", pf)
	}
	for i, a := range f.auths {
		if a != "Bearer "+token || f.versions[i] != "9.9" {
			t.Fatalf("headers %q %q", a, f.versions[i])
		}
	}
	// The report is JSON with similar as an array and no token in it.
	b, _ := json.Marshal(rep)
	if strings.Contains(string(b), token) || !strings.Contains(string(b), `"similar":[]`) {
		t.Fatalf("%s", b)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\n")
	f := newFake(t)
	if _, err := DryRun(context.Background(), f.input(plan.Plan{Root: root, Entries: []plan.PlanEntry{docEntry("a.md")}})); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.paths {
		if strings.HasSuffix(p, "/import") || strings.Contains(p, "create_object") {
			t.Fatalf("the dry run reached a writing route: %v", f.paths)
		}
	}
	// Input.Route is a Previewer, whose only method is Preview: no Commit
	// can be called from the dry run's code path.
	if pt := reflect.TypeOf((*Previewer)(nil)).Elem(); pt.NumMethod() != 1 || pt.Method(0).Name != "Preview" {
		t.Fatal("Previewer must offer Preview and nothing else")
	}
}

func TestRescanAtImport(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\nclean\n")
	write(t, root, "b.md", "# B\nclean\n")
	before, err := scan.Run(context.Background(), []string{root}, scan.Options{})
	if err != nil || len(before.Files) != 2 {
		t.Fatal(err, before)
	}
	for _, fl := range before.Files {
		if len(fl.Flags) != 0 {
			t.Fatalf("clean scan flagged %v", fl)
		}
	}
	// The file gains a secret after the scan the agent looked at.
	write(t, root, "a.md", "# A\nkey AKIAABCDEFGHIJKLMNOP\n")
	f := newFake(t)
	rep, err := DryRun(context.Background(), f.input(plan.Plan{Root: root, Entries: []plan.PlanEntry{docEntry("a.md"), docEntry("b.md")}}))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Entries[0].Action != "refused" || !strings.Contains(rep.Entries[0].Reason, "secret") || rep.Entries[1].Action != "create" {
		t.Fatalf("%+v", rep.Entries)
	}
	for _, p := range f.previews {
		if _, sent := p.Files["a.md"]; sent {
			t.Fatal("the file with a secret was uploaded")
		}
	}
}

func TestDryRunRefusesFileChangedAfterRescan(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\n")
	f := newFake(t)
	in := f.input(plan.Plan{Root: root, Entries: []plan.PlanEntry{docEntry("a.md")}})
	in.Scan = func(ctx context.Context, r string) (scan.Result, error) {
		res, err := scan.Run(ctx, []string{r}, scan.Options{})
		write(t, root, "a.md", "# A\nchanged\n")
		return res, err
	}
	rep, err := DryRun(context.Background(), in)
	if err != nil || rep.Entries[0].Action != "refused" || len(f.previews) != 0 {
		t.Fatalf("%v %+v %d", err, rep.Entries, len(f.previews))
	}
}

func TestDryRunObjectsNeedCreateObject(t *testing.T) {
	root := t.TempDir()
	write(t, root, "logo.png", "\x89PNG\x00\x01")
	f := newFake(t)
	f.tools = []string{"spaces"}
	rep, err := DryRun(context.Background(), f.input(plan.Plan{Root: root, Entries: []plan.PlanEntry{{Path: "logo.png", Kind: "object", Scope: "organization"}}}))
	if err != nil || rep.Refused != 1 || !strings.Contains(rep.Entries[0].Reason, "objects") {
		t.Fatalf("%v %+v", err, rep)
	}
}

func TestDryRunPublicSpaceMakesNoRequest(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\n")
	f := newFake(t)
	e := docEntry("a.md")
	e.Space = "docs"
	rep, err := DryRun(context.Background(), f.input(plan.Plan{Root: root, Entries: []plan.PlanEntry{e}}))
	if err != nil || rep.Refused != 1 || len(f.previews) != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
	for _, p := range f.paths {
		if strings.Contains(p, "/cli/v1") {
			t.Fatalf("a request went to the route: %v", f.paths)
		}
	}
}

func TestRouteRefusalRefusesTheBatch(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\n")
	f := newFake(t)
	f.status = 403
	rep, err := DryRun(context.Background(), f.input(plan.Plan{Root: root, Entries: []plan.PlanEntry{docEntry("a.md")}}))
	if err != nil || rep.Refused != 1 || rep.Entries[0].Reason != "A sentence from the route." {
		t.Fatalf("%v %+v", err, rep)
	}
}

func TestUpgradeRequiredIsErrUpgrade(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\n")
	f := newFake(t)
	f.status = 426
	_, err := DryRun(context.Background(), f.input(plan.Plan{Root: root, Entries: []plan.PlanEntry{docEntry("a.md")}}))
	if !errors.Is(err, mcp.ErrUpgrade) {
		t.Fatalf("%v", err)
	}
}

func TestRouteUsesCompiledOriginAndPathEscape(t *testing.T) {
	var uri string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uri = r.RequestURI
		io.WriteString(w, `{"items":[]}`)
	}))
	defer srv.Close()
	c := &mcp.Client{Origin: srv.URL, Version: "1", Token: func(context.Context) (string, error) { return token, nil }}
	if _, err := (&Route{MCP: c}).Preview(context.Background(), "a b/../c", []Upload{{Path: "x.md", Data: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if uri != "/cli/v1/spaces/a%20b%2F..%2Fc/import/preview" {
		t.Fatalf("the space was not escaped into one segment: %q", uri)
	}
}

func TestBatchPairsEachFileWithItsPath(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\nalpha\n")
	write(t, root, "sub/b.md", "# B\nbravo\n")
	write(t, root, "c.md", "# C\ncharlie\n")
	f := newFake(t)
	p := plan.Plan{Root: root, Entries: []plan.PlanEntry{docEntry("a.md"), docEntry("sub/b.md"), docEntry("c.md")}}
	if _, err := DryRun(context.Background(), f.input(p)); err != nil {
		t.Fatal(err)
	}
	if len(f.previews) != 1 {
		t.Fatalf("batches: %d", len(f.previews))
	}
	r := f.previews[0]
	if strings.Join(r.Paths, ",") != "a.md,sub/b.md,c.md" {
		t.Fatalf("paths %v", r.Paths)
	}
	want := map[string]string{"a.md": "# A\nalpha\n", "sub/b.md": "# B\nbravo\n", "c.md": "# C\ncharlie\n"}
	for path, body := range want {
		if r.Files[path] != body {
			t.Errorf("%s carried %q", path, r.Files[path])
		}
	}
	for i, pf := range r.Plan.Files {
		if pf["relative_path"] != r.Paths[i] {
			t.Errorf("plan entry %d is %s, path is %s", i, pf["relative_path"], r.Paths[i])
		}
	}
}

func TestMultipartFilenameIsTheBaseName(t *testing.T) {
	f := newFake(t)
	r := &Route{MCP: f.client()}
	if _, err := r.Preview(context.Background(), "s", []Upload{{Path: "/abs/deep/dir/a.md", Data: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if got := f.previews[0].Dispositions[0]; !strings.Contains(got, `filename="a.md"`) {
		t.Fatalf("%s", got)
	}
}

func TestRefusedRootMakesNoRequest(t *testing.T) {
	f := newFake(t)
	_, err := DryRun(context.Background(), f.input(plan.Plan{Root: "/", Entries: []plan.PlanEntry{docEntry("a.md")}}))
	var re *scan.RefusedError
	if !errors.As(err, &re) || !strings.Contains(re.Msg, "smrit import will not") || strings.Contains(re.Msg, "smrit scan") {
		t.Fatalf("%v", err)
	}
	if len(f.paths) != 0 {
		t.Fatalf("requests were made: %v", f.paths)
	}
}

func TestRouteDoesNotFollowRedirect(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the redirect was followed")
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := &mcp.Client{Origin: srv.URL, Version: "1", Token: func(context.Context) (string, error) { return token, nil }}
	_, err := (&Route{MCP: c}).Preview(context.Background(), "s", []Upload{{Path: "a.md", Data: []byte("x")}})
	if !errors.Is(err, mcp.ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestRouteRefreshesOnce401(t *testing.T) {
	n := 0
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		seen = append(seen, r.Header.Get("Authorization"))
		if n == 1 {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, `{"items":[]}`)
	}))
	defer srv.Close()
	c := &mcp.Client{Origin: srv.URL, Version: "1", Token: func(context.Context) (string, error) { return "old", nil },
		Refresh: func(context.Context) (string, error) { return "new", nil }}
	if _, err := (&Route{MCP: c}).Commit(context.Background(), "s", []Upload{{Path: "a.md", Data: []byte("x")}}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seen) != "[Bearer old Bearer new]" {
		t.Fatal(seen)
	}
}
