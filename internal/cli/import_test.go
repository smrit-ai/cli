package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/smrit-ai/cli/internal/importer"
	"github.com/smrit-ai/cli/internal/state"
)

type importFake struct {
	mu      sync.Mutex
	paths   []string
	preview int // status for the preview route; 0 is 200
	// badHash makes a commit answer a received hash that is not the sent one.
	badHash bool
	// duplicate refuses every create_document as a name already taken.
	duplicate bool
	tools     []string
}

func newImportFake(t *testing.T) (*importFake, *httptest.Server) {
	f := &importFake{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		status := f.preview
		f.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/cli/v1/") {
			if status != 0 {
				w.WriteHeader(status)
				io.WriteString(w, `{"error":"upgrade","detail":"too old"}`)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/import") {
				h := sha256.Sum256([]byte("# A\n"))
				got := hex.EncodeToString(h[:])
				if f.badHash {
					got = strings.Repeat("0", 64)
				}
				io.WriteString(w, `{"items":[{"relative_path":"a.md","status":"created","document_id":"DocAAAAAAAAAAAAAAAAAA","received_sha256":"`+got+`"}]}`)
				return
			}
			io.WriteString(w, `{"items":[{"relative_path":"a.md","action":"create","slug":"a","type":"decision","title":"A"},{"relative_path":"b.md","action":"rejected","reason":"The file is empty."}]}`)
			return
		}
		var m struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.Unmarshal(b, &m)
		w.Header().Set("Content-Type", "application/json")
		if m.Method == "tools/list" {
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"spaces"},{"name":"create_object"}]}}`)
			return
		}
		f.mu.Lock()
		f.tools = append(f.tools, m.Params.Name)
		duplicate := f.duplicate
		f.mu.Unlock()
		if m.Params.Name == "create_document" && duplicate {
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"this space already has a document named 'tasks': DocTTTTTTTTTTTTTTTTTT."}]}}`)
			return
		}
		if m.Params.Name == "create_document" {
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"document_id\":\"MapIdMMMMMMMMMMMMMMMM\",\"revision\":1}"}]}}`)
			return
		}
		out, _ := json.Marshal(`{"org":{"id":"` + orgID + `","name":"Acme","slug":"acme"},"spaces":[{"id":"S1","slug":"engineering","name":"E","visibility":"private"}],"types":["decision"]}`)
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":`+string(out)+`}]}}`)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func writePlan(t *testing.T, entries string, files map[string]string) string {
	dir := t.TempDir()
	root := filepath.Join(dir, "docs")
	for n, body := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(root, n)), 0o755)
		os.WriteFile(filepath.Join(root, n), []byte(body), 0o644)
	}
	p := filepath.Join(dir, "plan.json")
	os.WriteFile(p, []byte(`{"version":1,"root":"docs","entries":`+entries+`}`), 0o644)
	return p
}

const twoDocs = `[{"path":"a.md","kind":"document","space":"engineering","type":"decision"},{"path":"b.md","kind":"document","space":"engineering","type":"decision"}]`

func TestImportDryRunTextNamesOrgFirstAndExits1OnRefusal(t *testing.T) {
	f, srv := newImportFake(t)
	svc := setup(t, srv, prof("acme", orgID))
	plan := writePlan(t, twoDocs, map[string]string{"a.md": "# A\n", "b.md": "# B\n"})
	code, out, errs := runCmd(t, svc, "--json=false", "import", plan, "--dry-run")
	if code != ExitPartial {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	if !strings.HasPrefix(out, "Organization: Acme (acme)\n") || !strings.Contains(out, "b.md  document  refused") || !strings.Contains(out, "The file is empty.") {
		t.Fatalf("%s", out)
	}
	if strings.Contains(out+errs, fakeToken) {
		t.Fatal("token in output")
	}
	for _, p := range f.paths {
		if strings.HasSuffix(p, "/import") {
			t.Fatalf("committed: %v", f.paths)
		}
	}
}

func TestImportDryRunJSONExit0(t *testing.T) {
	_, srv := newImportFake(t)
	svc := setup(t, srv, prof("acme", orgID))
	plan := writePlan(t, `[{"path":"a.md","kind":"document","space":"engineering","type":"decision"}]`, map[string]string{"a.md": "# A\n"})
	code, out, errs := runCmd(t, svc, "import", plan, "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	var rep struct {
		Organization struct{ Name string } `json:"organization"`
		Entries      []map[string]any      `json:"entries"`
		WouldCreate  int                   `json:"would_create"`
		Refused      int                   `json:"refused"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Organization.Name != "Acme" || rep.WouldCreate != 1 || rep.Refused != 0 {
		t.Fatalf("%v %s", err, out)
	}
	for _, k := range []string{"path", "kind", "space", "action", "reason", "similar", "type_from", "slug", "title"} {
		if _, ok := rep.Entries[0][k]; !ok {
			t.Errorf("entry lacks %s", k)
		}
	}
}

func TestUpgradeRequiredExits6(t *testing.T) {
	f, srv := newImportFake(t)
	f.preview = 426
	svc := setup(t, srv, prof("acme", orgID))
	plan := writePlan(t, `[{"path":"a.md","kind":"document","space":"engineering","type":"decision"}]`, map[string]string{"a.md": "# A\n"})
	code, _, errs := runCmd(t, svc, "import", plan, "--dry-run")
	if code != ExitUpgrade || !strings.Contains(errs, "Upgrade smrit") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}

func TestImportRouteRefusalExits1WithItsSentence(t *testing.T) {
	f, srv := newImportFake(t)
	f.preview = 403
	svc := setup(t, srv, prof("acme", orgID))
	plan := writePlan(t, `[{"path":"a.md","kind":"document","space":"engineering","type":"decision"}]`, map[string]string{"a.md": "# A\n"})
	code, out, _ := runCmd(t, svc, "import", plan, "--dry-run")
	if code != ExitRefused || !strings.Contains(out, `"refused":1`) {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestImportRunsTextNamesOrgFirstAndExits1OnRefusal(t *testing.T) {
	f, srv := newImportFake(t)
	svc := setup(t, srv, prof("acme", orgID))
	plan := writePlan(t, twoDocs, map[string]string{"a.md": "# A\n", "b.md": "# B\n"})
	code, out, errs := runCmd(t, svc, "--json=false", "import", plan)
	if code != ExitPartial {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	run := regexp.MustCompile(`[0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}`).FindString(errs)
	if !strings.HasPrefix(errs, "Importing into Acme (acme) as run "+run) || run == "" {
		t.Fatalf("stderr %q", errs)
	}
	for _, want := range []string{"Run: " + run, "a.md  document  created  DocAAAAAAAAAAAAAAAAAA", "b.md  document  refused: The file is empty.",
		"Import map: MapIdMMMMMMMMMMMMMMMM", "1 created, 0 updated, 0 unchanged, 0 failed, 1 refused, 0 skipped."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !strings.HasPrefix(out, "Organization: Acme (acme)\n") {
		t.Fatalf("%s", out)
	}
	if strings.Contains(out+errs, fakeToken) {
		t.Fatal("token in output")
	}
	if !slices.Contains(f.paths, "/cli/v1/spaces/engineering/import") || !slices.Contains(f.tools, "create_document") {
		t.Fatalf("%v %v", f.paths, f.tools)
	}
}

func TestImportRunJSONReport(t *testing.T) {
	_, srv := newImportFake(t)
	svc := setup(t, srv, prof("acme", orgID))
	plan := writePlan(t, `[{"path":"a.md","kind":"document","space":"engineering","type":"decision"}]`, map[string]string{"a.md": "# A\n"})
	code, out, errs := runCmd(t, svc, "import", plan)
	if code != ExitOK {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, `{"organization":`) || rep["map"] != "MapIdMMMMMMMMMMMMMMMM" || rep["created"] != float64(1) {
		t.Fatalf("%s", out)
	}
	for _, k := range []string{"run", "map", "map_error", "links", "entries", "failed", "refused", "skipped"} {
		if _, ok := rep[k]; !ok {
			t.Errorf("report lacks %s", k)
		}
	}
	e := rep["entries"].([]any)[0].(map[string]any)
	for _, k := range []string{"path", "status", "id", "error"} {
		if _, ok := e[k]; !ok {
			t.Errorf("entry lacks %s", k)
		}
	}
	if e["status"] != "created" || e["id"] != "DocAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("%v", e)
	}
	dir, _ := state.Dir()
	if _, err := os.Stat(filepath.Join(dir, "runs", rep["run"].(string)+".json")); err != nil {
		t.Fatalf("no run state: %v", err)
	}
}

func TestImportRunFailedEntryExits1(t *testing.T) {
	f, srv := newImportFake(t)
	f.badHash = true
	svc := setup(t, srv, prof("acme", orgID))
	plan := writePlan(t, `[{"path":"a.md","kind":"document","space":"engineering","type":"decision"}]`, map[string]string{"a.md": "# A\n"})
	code, out, _ := runCmd(t, svc, "import", plan)
	if code != ExitPartial || !strings.Contains(out, "The bytes Smrit received differ from the bytes sent.") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestImportRunEveryEntryRefusedExits4(t *testing.T) {
	f, srv := newImportFake(t)
	svc := setup(t, srv, prof("acme", orgID))
	plan := writePlan(t, `[{"path":"a.md","kind":"document","space":"nowhere","type":"decision"}]`, map[string]string{"a.md": "# A\n"})
	code, out, _ := runCmd(t, svc, "import", plan)
	if code != ExitRefused || !strings.Contains(out, `"refused":1`) {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, p := range f.paths {
		if strings.HasPrefix(p, "/cli/v1/") {
			t.Fatalf("sent %v", f.paths)
		}
	}
}

func TestImportRunUpgradeExits6AndNamesTheRun(t *testing.T) {
	f, srv := newImportFake(t)
	f.preview = 426
	svc := setup(t, srv, prof("acme", orgID))
	plan := writePlan(t, `[{"path":"a.md","kind":"document","space":"engineering","type":"decision"}]`, map[string]string{"a.md": "# A\n"})
	code, out, errs := runCmd(t, svc, "import", plan)
	if code != ExitUpgrade || !strings.Contains(errs, "Upgrade smrit") || !strings.Contains(errs, "--resume") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	// What was done before the stop is reported, with the run's id.
	var rep struct {
		Run     string           `json:"run"`
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Run == "" || len(rep.Entries) != 1 || !strings.Contains(errs, rep.Run) {
		t.Fatalf("%v %q %q", err, out, errs)
	}
}

func TestImportResumeUsage(t *testing.T) {
	f, srv := newImportFake(t)
	svc := setup(t, srv, prof("acme", orgID))
	plan := writePlan(t, twoDocs, map[string]string{"a.md": "# A\n", "b.md": "# B\n"})
	if code, _, errs := runCmd(t, svc, "import", plan, "--dry-run", "--resume", "20261109T143005Z-a1b2c3"); code != ExitUsage || !strings.Contains(errs, "--dry-run") {
		t.Fatalf("both: %d %s", code, errs)
	}
	code, _, errs := runCmd(t, svc, "import", plan, "--resume", "../../x")
	if code != ExitUsage || !strings.Contains(errs, "No import run") {
		t.Fatalf("bad id: %d %s", code, errs)
	}
	for _, p := range f.paths {
		if strings.HasPrefix(p, "/cli/v1/") {
			t.Fatalf("sent %v", f.paths)
		}
	}
}

func TestImportUsageErrors(t *testing.T) {
	_, srv := newImportFake(t)
	svc := setup(t, srv, prof("acme", orgID))
	if code, _, _ := runCmd(t, svc, "import", "--dry-run"); code != ExitUsage {
		t.Fatalf("no plan: %d", code)
	}
	if code, _, _ := runCmd(t, svc, "import", filepath.Join(t.TempDir(), "none.json"), "--dry-run"); code != ExitUsage {
		t.Fatalf("missing plan: %d", code)
	}
}

func TestImportEveryEntryRefusedExits4(t *testing.T) {
	_, srv := newImportFake(t)
	svc := setup(t, srv, prof("acme", orgID))
	plan := writePlan(t, `[{"path":"a.md","kind":"document","space":"nowhere","type":"decision"}]`, map[string]string{"a.md": "# A\n"})
	code, out, _ := runCmd(t, svc, "import", plan, "--dry-run")
	if code != ExitRefused || !strings.Contains(out, `"refused":1`) {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestImportRefusedRootExits4BeforeAnyRequest(t *testing.T) {
	f, srv := newImportFake(t)
	svc := setup(t, srv, prof("acme", orgID))
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.json")
	os.WriteFile(plan, []byte(`{"version":1,"root":"/","entries":[{"path":"a.md","kind":"document","space":"engineering","type":"decision"}]}`), 0o644)
	code, _, errs := runCmd(t, svc, "import", plan, "--dry-run")
	if code != ExitRefused || !strings.Contains(errs, "smrit import will not") || len(f.paths) != 0 {
		t.Fatalf("exit %d %q %v", code, errs, f.paths)
	}
}

// A workbook already in Smrit is listed with its sentence and counts as
// done for the exit code (ruling, Task 13 review).
func TestImportWorkbookAlreadyInSmritExits0AndSaysSo(t *testing.T) {
	f, srv := newImportFake(t)
	f.duplicate = true
	svc := setup(t, srv, prof("acme", orgID))
	plan := writePlan(t, `[{"path":"tasks.csv","kind":"workbook","space":"engineering","title":"Tasks","columns":[{"name":"a","type":"text"}]}]`,
		map[string]string{"tasks.csv": "a\nx\n"})
	code, out, errs := runCmd(t, svc, "--json=false", "import", plan)
	if code != ExitOK {
		t.Fatalf("exit %d: %s %s", code, out, errs)
	}
	for _, want := range []string{"tasks.csv  workbook  exists: This space already has a document under this workbook's title",
		"0 created, 0 updated, 0 unchanged, 1 already in Smrit, 0 failed, 0 refused, 0 skipped."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if slices.Contains(f.tools, "write_workbook") || slices.Contains(f.tools, "write_records") {
		t.Fatalf("wrote after the duplicate: %v", f.tools)
	}
}

func TestImportTextCountsBlankRowsSkipped(t *testing.T) {
	run := runOutput{importer.RunReport{Entries: []importer.RunEntry{
		{Path: "t.csv", Kind: "workbook", Status: "created", ID: "DocAAAAAAAAAAAAAAAAAA", BlankRowsSkipped: 2},
		{Path: "u.csv", Kind: "workbook", Status: "created", ID: "DocBBBBBBBBBBBBBBBBBB"},
	}}}.String()
	if !strings.Contains(run, "t.csv  workbook  created  DocAAAAAAAAAAAAAAAAAA  (2 blank rows skipped)\n") || strings.Contains(run, "u.csv  workbook  created  DocBBBBBBBBBBBBBBBBBB  (") {
		t.Fatalf("%s", run)
	}
	dry := importOutput{importer.Report{Entries: []importer.ReportEntry{
		{Path: "t.csv", Kind: "workbook", Action: "create", BlankRowsSkipped: 1},
	}}}.String()
	if !strings.Contains(dry, "t.csv  workbook  create  (1 blank row skipped)") {
		t.Fatalf("%s", dry)
	}
}
