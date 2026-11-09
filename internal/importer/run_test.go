package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smrit-ai/cli/internal/mcp"
	"github.com/smrit-ai/cli/internal/plan"
	"github.com/smrit-ai/cli/internal/state"
)

// storeFake is a Smrit that keeps documents by slug, as the import route
// does: the same bytes for a slug answer unchanged and write nothing.
type storeFake struct {
	mu       sync.Mutex
	docs     map[string]*fakeDoc // by slug
	nextID   int
	previews []received
	commits  []received
	calls    []fakeCall
	// tamper names paths whose received_sha256 comes back wrong.
	tamper map[string]bool
	// failOnce names paths whose first commit fails with that sentence.
	failOnce map[string]string
	// onCommit runs at the start of every commit request.
	onCommit func()
	srv      *httptest.Server
}

type fakeDoc struct {
	ID, Slug, Title, Body string
	Revisions             int
}

type fakeCall struct {
	Tool string
	Args map[string]any
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func newStore(t *testing.T) *storeFake {
	f := &storeFake{docs: map[string]*fakeDoc{}, tamper: map[string]bool{}, failOnce: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == mcpURL {
			f.mcp(w, r)
			return
		}
		f.route(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// seed stores a document as if an earlier import had made it.
func (f *storeFake) seed(slug, body string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.add(slug, slug, body).ID
}

func (f *storeFake) add(slug, title, body string) *fakeDoc {
	f.nextID++
	d := &fakeDoc{ID: fmt.Sprintf("Doc%018d", f.nextID), Slug: slug, Title: title, Body: body, Revisions: 1}
	f.docs[slug] = d
	return d
}

func (f *storeFake) byID(id string) *fakeDoc {
	for _, d := range f.docs {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (f *storeFake) mcp(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m struct {
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	json.Unmarshal(b, &m)
	w.Header().Set("Content-Type", "application/json")
	if m.Method == "tools/list" {
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"spaces"},{"name":"create_document"},{"name":"update_document"},{"name":"get_document_metadata"}]}}`)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{m.Params.Name, m.Params.Arguments})
	answer := func(v any) {
		txt, _ := json.Marshal(v)
		s, _ := json.Marshal(string(txt))
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":`+string(s)+`}]}}`)
	}
	refuse := func(msg string) {
		s, _ := json.Marshal(msg)
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":`+string(s)+`}]}}`)
	}
	a := m.Params.Arguments
	switch m.Params.Name {
	case "spaces":
		answer(map[string]any{
			"org": map[string]string{"id": "AbCdEfGhIjKlMnOpQrStU", "name": "Acme", "slug": "acme"},
			"spaces": []map[string]string{
				{"id": "SpaceIdEngineering0001", "slug": "engineering", "name": "Engineering", "visibility": "private"},
				{"id": "SpaceIdHandbook0000003", "slug": "handbook", "name": "Handbook", "visibility": "private"},
			},
			"types": []string{"decision", "reference"},
		})
	case "create_document":
		title, _ := a["title"].(string)
		slug := strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(title), "-"), "-")
		if d, ok := f.docs[slug]; ok && a["allow_duplicate"] != true {
			refuse(fmt.Sprintf("this space already has a document named '%s': %s. Read it first and update it rather than creating another; pass allow_duplicate=true if you genuinely need a second one.", slug, d.ID))
			return
		}
		body, _ := a["body"].(string)
		d := f.add(slug, title, body)
		answer(map[string]any{"document_id": d.ID, "revision": d.Revisions})
	case "get_document_metadata":
		id, _ := a["document_id"].(string)
		d := f.byID(id)
		if d == nil {
			refuse("not found")
			return
		}
		answer(map[string]any{"document_id": d.ID, "title": d.Title, "revision": d.Revisions})
	case "update_document":
		id, _ := a["document_id"].(string)
		d := f.byID(id)
		if d == nil {
			refuse("not found")
			return
		}
		if base, _ := a["base_revision"].(float64); int(base) != d.Revisions {
			refuse("base_revision is stale")
			return
		}
		d.Body, _ = a["body"].(string)
		d.Revisions++
		answer(map[string]any{"document_id": d.ID, "revision": d.Revisions})
	default:
		refuse("unknown tool")
	}
}

func (f *storeFake) route(w http.ResponseWriter, r *http.Request) {
	commit := strings.HasSuffix(r.URL.Path, "/import")
	if commit && f.onCommit != nil {
		f.onCommit()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := received{Space: strings.TrimPrefix(r.URL.Path, "/cli/v1/spaces/"), Files: map[string]string{}}
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
	var items []map[string]any
	for _, pe := range rec.Plan.Files {
		path, slug, body := pe["relative_path"], pe["slug"], rec.Files[pe["relative_path"]]
		got := sum(body)
		if f.tamper[path] {
			got = sum(body + "x")
		}
		d := f.docs[slug]
		if !commit {
			it := map[string]any{"relative_path": path, "action": "create", "slug": slug, "received_sha256": got}
			if d != nil {
				it["document_id"] = d.ID
				it["action"] = "update"
				if d.Body == body {
					it["action"] = "unchanged"
				}
			}
			items = append(items, it)
			continue
		}
		if msg, ok := f.failOnce[path]; ok {
			delete(f.failOnce, path)
			items = append(items, map[string]any{"relative_path": path, "status": "failed", "error": msg, "received_sha256": got})
			continue
		}
		it := map[string]any{"relative_path": path, "received_sha256": got}
		switch {
		case d == nil:
			d = f.add(slug, pe["title"], body)
			it["status"] = "created"
		case d.Body == body:
			it["status"] = "unchanged"
		default:
			d.Body = body
			d.Revisions++
			it["status"] = "updated"
		}
		it["document_id"] = d.ID
		items = append(items, it)
	}
	if commit {
		f.commits = append(f.commits, rec)
	} else {
		f.previews = append(f.previews, rec)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"items": items})
}

func (f *storeFake) client() *mcp.Client {
	return &mcp.Client{
		Origin: f.srv.URL, Path: mcpURL, Version: "9.9",
		Token: func(context.Context) (string, error) { return token, nil },
		Now:   func() time.Time { return time.Unix(1000, 0) },
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
}

// opts is a run against f keeping its state in dir.
func (f *storeFake) opts(dir string) RunOptions {
	c := f.client()
	return RunOptions{
		MCP: c, Route: &Route{MCP: c}, StateDir: dir, PlanSHA256: "planhash",
		Now:    func() time.Time { return time.Date(2026, 11, 9, 14, 30, 5, 0, time.UTC) },
		Random: bytes.NewReader(bytes.Repeat([]byte{0xa1, 0xb2, 0xc3}, 50)),
	}
}

// committed counts the commits that carried path.
func (f *storeFake) committed(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.commits {
		if _, ok := c.Files[path]; ok {
			n++
		}
	}
	return n
}

// firstSent is the body path carried in its first commit.
func (f *storeFake) firstSent(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commits {
		if b, ok := c.Files[path]; ok {
			return b
		}
	}
	return ""
}

func (f *storeFake) revisions() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for s, d := range f.docs {
		out[s] = d.Revisions
	}
	return out
}

func (f *storeFake) toolCalls(name string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeCall
	for _, c := range f.calls {
		if c.Tool == name {
			out = append(out, c)
		}
	}
	return out
}

func docsPlan(root string, paths ...string) plan.Plan {
	p := plan.Plan{Version: 1, Root: root}
	for _, x := range paths {
		p.Entries = append(p.Entries, docEntry(x))
	}
	return p
}

func entryOf(t *testing.T, rep RunReport, path string) RunEntry {
	t.Helper()
	for _, e := range rep.Entries {
		if e.Path == path {
			return e
		}
	}
	t.Fatalf("no entry %s in %+v", path, rep.Entries)
	return RunEntry{}
}

func mustRun(t *testing.T, p plan.Plan, o RunOptions) RunReport {
	t.Helper()
	rep, err := Run(context.Background(), p, o)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return rep
}

// The most important property of an import: run again with no state, it
// finds every target already there, rewrites every link before sending, and
// Smrit answers unchanged for every file, writing no new revision.
func TestRerunIsUnchanged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\nSee [B](sub/b.md#part) and `[B](b.md)`.\n")
	write(t, root, "sub/b.md", "# B\nBack to [A](../a.md), and [C](/c.md).\n")
	write(t, root, "c.md", "# C\nNo links here, only [the web](https://example.com/b.md).\n")
	f := newStore(t)
	p := docsPlan(root, "a.md", "sub/b.md", "c.md")
	p.Entries[1].Slug = "b"

	first := mustRun(t, p, f.opts(t.TempDir()))
	for _, e := range first.Entries {
		if e.Status != state.StatusCreated || e.ID == "" {
			t.Fatalf("first run %+v", e)
		}
	}
	ids := map[string]string{}
	for _, e := range first.Entries {
		ids[e.Path] = e.ID
	}
	f.mu.Lock()
	aBody, bBody := f.docs["a"].Body, f.docs["b"].Body
	f.mu.Unlock()
	if want := "# A\nSee [B](smrit:d:" + ids["sub/b.md"] + ") and `[B](b.md)`.\n"; aBody != want {
		t.Fatalf("a stored %q, want %q", aBody, want)
	}
	if want := "# B\nBack to [A](smrit:d:" + ids["a.md"] + "), and [C](smrit:d:" + ids["c.md"] + ").\n"; bBody != want {
		t.Fatalf("b stored %q, want %q", bBody, want)
	}

	before := f.revisions()
	commitsBefore := len(f.commits)
	second := mustRun(t, p, f.opts(t.TempDir()))
	for _, e := range second.Entries {
		if e.Status != state.StatusUnchanged || e.ID != ids[e.Path] {
			t.Errorf("rerun %+v", e)
		}
	}
	if second.Unchanged != 3 || second.Created+second.Updated+second.Failed != 0 {
		t.Fatalf("rerun counts %+v", second)
	}
	after := f.revisions()
	for slug, n := range before {
		if after[slug] != n {
			t.Errorf("%s went from revision %d to %d", slug, n, after[slug])
		}
	}
	// One commit for the batch, and no link pass: nothing was created.
	if len(f.commits)-commitsBefore != 1 {
		t.Fatalf("rerun made %d commits", len(f.commits)-commitsBefore)
	}
}

func TestLinksRewrittenBeforeUploadWhenKnown(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\nRead [the plan](b.md \"Plan\") first.\n")
	write(t, root, "b.md", "# B\n")
	f := newStore(t)
	bid := f.seed("b", "# B\n")
	rep := mustRun(t, docsPlan(root, "a.md", "b.md"), f.opts(t.TempDir()))
	if got, want := f.firstSent("a.md"), "# A\nRead [the plan](smrit:d:"+bid+" \"Plan\") first.\n"; got != want {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if f.committed("a.md") != 1 {
		t.Fatalf("a committed %d times", f.committed("a.md"))
	}
	if e := entryOf(t, rep, "b.md"); e.Status != state.StatusUnchanged || e.ID != bid {
		t.Fatalf("%+v", e)
	}
	if e := entryOf(t, rep, "a.md"); e.Status != state.StatusCreated {
		t.Fatalf("%+v", e)
	}
}

func TestSecondPassOnlyForNewTargets(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\nSee [B](b.md).\n")
	write(t, root, "b.md", "# B\n")
	write(t, root, "c.md", "# C\nSee [D](d.md).\n")
	write(t, root, "d.md", "# D\n")
	write(t, root, "e.md", "# E\nSee [the web](https://example.com).\n")
	// g is in another space, so its batch is committed after b has its id:
	// it links to a document this run created, yet its bytes were right the
	// first time and the link pass does not send it again.
	write(t, root, "g.md", "# G\nSee [B](b.md).\n")
	f := newStore(t)
	did := f.seed("d", "# D\n")
	p := docsPlan(root, "a.md", "b.md", "c.md", "d.md", "e.md", "g.md")
	p.Entries[5].Space = "handbook"
	rep := mustRun(t, p, f.opts(t.TempDir()))
	want := map[string]int{"a.md": 2, "b.md": 1, "c.md": 1, "d.md": 1, "e.md": 1, "g.md": 1}
	for p, n := range want {
		if got := f.committed(p); got != n {
			t.Errorf("%s committed %d times, want %d", p, got, n)
		}
	}
	b := entryOf(t, rep, "b.md").ID
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.docs["a"].Body != "# A\nSee [B](smrit:d:"+b+").\n" || f.docs["c"].Body != "# C\nSee [D](smrit:d:"+did+").\n" ||
		f.docs["g"].Body != "# G\nSee [B](smrit:d:"+b+").\n" {
		t.Fatalf("a %q c %q", f.docs["a"].Body, f.docs["c"].Body)
	}
	if e := rep.Entries[0]; e.Status != state.StatusCreated {
		t.Fatalf("a after its second pass: %+v", e)
	}
}

func TestCodeSpansUntouched(t *testing.T) {
	root := t.TempDir()
	body := strings.Join([]string{
		"# A",
		"Inline `[b](b.md)` and ``[b `x`](b.md)`` stay, [this](b.md) moves.",
		"```md",
		"[b](b.md)",
		"```",
		"~~~",
		"![b](b.md)",
		"~~~",
		"    ```",
		"    [b](b.md)",
		"    ```",
		"and [again](./b.md#top).",
		"",
	}, "\n")
	write(t, root, "a.md", body)
	write(t, root, "b.md", "# B\n")
	f := newStore(t)
	bid := f.seed("b", "# B\n")
	rep := mustRun(t, docsPlan(root, "a.md", "b.md"), f.opts(t.TempDir()))
	want := strings.Replace(body, "[this](b.md)", "[this](smrit:d:"+bid+")", 1)
	want = strings.Replace(want, "[again](./b.md#top)", "[again](smrit:d:"+bid+")", 1)
	if got := f.firstSent("a.md"); got != want {
		t.Fatalf("sent\n%s\nwant\n%s", got, want)
	}
	// The dropped anchor is reported, once, naming the link as written.
	var anchors []LinkFinding
	for _, l := range rep.Links {
		if l.Target == "./b.md#top" {
			anchors = append(anchors, l)
		}
	}
	if len(anchors) != 1 || anchors[0].Path != "a.md" || !strings.Contains(anchors[0].Reason, "#top") {
		t.Fatalf("findings %+v", rep.Links)
	}
}

func TestReceivedHashMismatchFails(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\n")
	write(t, root, "b.md", "# B\n")
	f := newStore(t)
	f.tamper["a.md"] = true
	dir := t.TempDir()
	rep := mustRun(t, docsPlan(root, "a.md", "b.md"), f.opts(dir))
	a := entryOf(t, rep, "a.md")
	if a.Status != state.StatusFailed || a.Error != "The bytes Smrit received differ from the bytes sent." {
		t.Fatalf("%+v", a)
	}
	if b := entryOf(t, rep, "b.md"); b.Status != state.StatusCreated {
		t.Fatalf("%+v", b)
	}
	if rep.Failed != 1 || rep.Created != 1 {
		t.Fatalf("%+v", rep)
	}
	run, err := state.LoadRun(dir, rep.Run)
	if err != nil || run.Entries["a.md"].Status != state.StatusFailed {
		t.Fatalf("%+v %v", run, err)
	}
}

func TestResumeSkipsDone(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\n")
	write(t, root, "b.md", "# B\n")
	write(t, root, "c.md", "# C\n")
	f := newStore(t)
	f.failOnce["b.md"] = "Smrit could not save this file."
	dir := t.TempDir()
	p := docsPlan(root, "a.md", "b.md", "c.md")
	first := mustRun(t, p, f.opts(dir))
	if e := entryOf(t, first, "b.md"); e.Status != state.StatusFailed || e.Error != "Smrit could not save this file." {
		t.Fatalf("%+v", e)
	}
	// c changes on disk after its import: a resume imports it again.
	write(t, root, "c.md", "# C, edited\n")
	f.mu.Lock()
	f.commits, f.previews = nil, nil
	f.mu.Unlock()

	o := f.opts(dir)
	o.Resume = first.Run
	rep := mustRun(t, p, o)
	if rep.Run != first.Run {
		t.Fatalf("resumed as %s, not %s", rep.Run, first.Run)
	}
	if f.committed("a.md") != 0 || f.committed("b.md") != 1 || f.committed("c.md") != 1 {
		t.Fatalf("commits a %d b %d c %d", f.committed("a.md"), f.committed("b.md"), f.committed("c.md"))
	}
	for _, pv := range f.previews {
		if _, ok := pv.Files["a.md"]; ok {
			t.Fatal("a done entry was previewed again")
		}
	}
	got := map[string]string{}
	for _, e := range rep.Entries {
		got[e.Path] = e.Status
	}
	if got["a.md"] != state.StatusCreated || got["b.md"] != state.StatusCreated || got["c.md"] != state.StatusUpdated {
		t.Fatalf("%v", got)
	}

	// A plan whose file changed is a different plan: refused, nothing sent.
	f.mu.Lock()
	f.commits, f.previews = nil, nil
	f.mu.Unlock()
	o = f.opts(dir)
	o.Resume, o.PlanSHA256 = first.Run, "otherhash"
	if _, err := Run(context.Background(), p, o); err == nil || !strings.Contains(err.Error(), "plan changed") {
		t.Fatalf("changed plan: %v", err)
	}
	if len(f.commits)+len(f.previews) != 0 {
		t.Fatal("a refused resume sent files")
	}
	o.Resume, o.PlanSHA256 = "../../etc/passwd", "planhash"
	if _, err := Run(context.Background(), p, o); err == nil || !strings.Contains(err.Error(), "No import run") {
		t.Fatalf("bad id: %v", err)
	}
}

func TestStateIs0600AndAtomic(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\n[B](b.md)\n")
	write(t, root, "b.md", "# B\n")
	f := newStore(t)
	dir := t.TempDir()
	var seen []*state.Run
	// At every commit, the state on disk is whole JSON holding every result
	// so far: the link pass's commit finds both documents already recorded.
	f.onCommit = func() {
		matches, _ := filepath.Glob(filepath.Join(dir, "runs", "*.json"))
		if len(matches) != 1 {
			t.Errorf("state files %v", matches)
			return
		}
		id := strings.TrimSuffix(filepath.Base(matches[0]), ".json")
		r, err := state.LoadRun(dir, id)
		if err != nil {
			t.Errorf("state mid run: %v", err)
			return
		}
		seen = append(seen, r)
	}
	var began []string
	o := f.opts(dir)
	o.Began = func(org Org, run string) { began = append(began, org.Name+" "+run) }
	rep := mustRun(t, docsPlan(root, "a.md", "b.md"), o)
	if rep.Run != "20261109T143005Z-a1b2c3" || len(began) != 1 || began[0] != "Acme "+rep.Run {
		t.Fatalf("run %q began %v", rep.Run, began)
	}
	if len(seen) != 2 || seen[0].Phase != state.PhaseDocuments || seen[1].Phase != state.PhaseLinks ||
		seen[1].Entries["a.md"].Status != state.StatusCreated || seen[1].Entries["b.md"].Status != state.StatusCreated {
		t.Fatalf("mid run states %+v", seen)
	}
	p := filepath.Join(dir, "runs", rep.Run+".json")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "runs")); st.Mode().Perm() != 0o700 {
		t.Fatalf("runs folder %o", st.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "runs", ".*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
	run, err := state.LoadRun(dir, rep.Run)
	if err != nil {
		t.Fatal(err)
	}
	a := run.Entries["a.md"]
	if run.Phase != state.PhaseFinished || run.Root != root || run.Org.Slug != "acme" || run.PlanSHA256 != "planhash" ||
		len(run.Plan.Entries) != 2 || run.Map == "" || run.Map != rep.Map {
		t.Fatalf("%+v", run)
	}
	raw, _ := os.ReadFile(filepath.Join(root, "a.md"))
	if a.SourceSHA256 != sum(string(raw)) || a.SentSHA256 != sum(f.docs["a"].Body) || a.SentSHA256 == a.SourceSHA256 || a.ID == "" {
		t.Fatalf("a %+v", a)
	}
	data, _ := os.ReadFile(p)
	if bytes.Contains(data, []byte(token)) {
		t.Fatal("token in state")
	}
}

func TestMapUpdatedNotDuplicated(t *testing.T) {
	root := filepath.Join(t.TempDir(), "team-docs")
	write(t, root, "a.md", "# A\n")
	write(t, root, "x|y.md", "# Y\n")
	f := newStore(t)
	dir := t.TempDir()
	p := docsPlan(root, "a.md", "x|y.md")
	p.Entries = append(p.Entries, plan.PlanEntry{Path: "a.md.bak", Kind: "document", Space: "nowhere", Type: "note"})
	write(t, root, "a.md.bak", "# old\n")
	first := mustRun(t, p, f.opts(dir))
	creates := f.toolCalls("create_document")
	if first.Map == "" || len(creates) != 1 {
		t.Fatalf("map %q, %d creates", first.Map, len(creates))
	}
	c := creates[0].Args
	a, y := entryOf(t, first, "a.md").ID, entryOf(t, first, "x|y.md").ID
	wantBody := "| Path | Document |\n|---|---|\n| a.md | <smrit:d:" + a + "> |\n| x\\|y.md | <smrit:d:" + y + "> |\n"
	if c["title"] != "Import map: team-docs 2026-11-09" || c["space_id"] != "engineering" || c["type"] != "reference" || c["body"] != wantBody {
		t.Fatalf("create %+v", c)
	}
	if _, ok := c["allow_duplicate"]; ok {
		t.Fatal("asked for a duplicate")
	}

	// Resumed: the same map is updated against its current revision.
	o := f.opts(dir)
	o.Resume = first.Run
	again := mustRun(t, p, o)
	ups := f.toolCalls("update_document")
	if again.Map != first.Map || len(f.toolCalls("create_document")) != 1 || len(ups) != 1 ||
		ups[0].Args["document_id"] != first.Map || ups[0].Args["base_revision"] != float64(1) {
		t.Fatalf("map %q, updates %+v", again.Map, ups)
	}

	// A fresh run with no state finds the name taken: reported, no retry.
	fresh := mustRun(t, p, f.opts(t.TempDir()))
	creates = f.toolCalls("create_document")
	if fresh.Map != "" || !strings.Contains(fresh.MapError, "already") || len(creates) != 2 {
		t.Fatalf("fresh %q %q %d", fresh.Map, fresh.MapError, len(creates))
	}
	for _, c := range creates {
		if _, ok := c.Args["allow_duplicate"]; ok {
			t.Fatal("retried with allow_duplicate")
		}
	}
}

func TestObjectsAndWorkbooksSkippedForNow(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\n")
	write(t, root, "t.csv", "a,b\n1,2\n")
	f := newStore(t)
	p := docsPlan(root, "a.md")
	p.Entries = append(p.Entries, plan.PlanEntry{Path: "t.csv", Kind: "workbook", Space: "engineering", Title: "T"})
	rep := mustRun(t, p, f.opts(t.TempDir()))
	w := entryOf(t, rep, "t.csv")
	if w.Status != state.StatusSkipped || w.Error != "Importing files and workbooks comes in a later version." || rep.Skipped != 1 {
		t.Fatalf("%+v", w)
	}
}

func TestRefusalsAreReportedNotSent(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\n")
	f := newStore(t)
	p := docsPlan(root, "a.md", "missing.md")
	rep := mustRun(t, p, f.opts(t.TempDir()))
	if e := entryOf(t, rep, "missing.md"); e.Status != state.StatusRefused || e.Error == "" {
		t.Fatalf("%+v", e)
	}
	if rep.Refused != 1 || rep.Created != 1 {
		t.Fatalf("%+v", rep)
	}
}

func TestRunStopsOnUpgrade(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.md", "# A\n")
	f := newImportOnly426(t)
	c := &mcp.Client{Origin: f.URL, Path: mcpURL, Version: "9.9",
		Token: func(context.Context) (string, error) { return token, nil },
		Sleep: func(context.Context, time.Duration) error { return nil }}
	o := RunOptions{MCP: c, Route: &Route{MCP: c}, StateDir: t.TempDir(), PlanSHA256: "h", Random: bytes.NewReader(make([]byte, 3))}
	rep, err := Run(context.Background(), docsPlan(root, "a.md"), o)
	if !errors.Is(err, mcp.ErrUpgrade) || rep.Run == "" {
		t.Fatalf("%v %+v", err, rep)
	}
}

// newImportOnly426 answers MCP as the store does and 426 on the import
// route.
func newImportOnly426(t *testing.T) *httptest.Server {
	s := newStore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == mcpURL {
			s.mcp(w, r)
			return
		}
		w.WriteHeader(http.StatusUpgradeRequired)
	}))
	t.Cleanup(srv.Close)
	return srv
}
