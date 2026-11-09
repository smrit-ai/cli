package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type importFake struct {
	mu      sync.Mutex
	paths   []string
	preview int // status for the preview route; 0 is 200
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
			io.WriteString(w, `{"items":[{"relative_path":"a.md","action":"create","slug":"a","type":"decision","title":"A"},{"relative_path":"b.md","action":"rejected","reason":"The file is empty."}]}`)
			return
		}
		var m struct {
			Method string `json:"method"`
		}
		json.Unmarshal(b, &m)
		w.Header().Set("Content-Type", "application/json")
		if m.Method == "tools/list" {
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"spaces"},{"name":"create_object"}]}}`)
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

func TestImportWithoutDryRunSaysLaterAndExits2(t *testing.T) {
	f, srv := newImportFake(t)
	svc := setup(t, srv, prof("acme", orgID))
	plan := writePlan(t, twoDocs, map[string]string{"a.md": "# A\n", "b.md": "# B\n"})
	code, out, errs := runCmd(t, svc, "import", plan)
	if code != ExitUsage || out != "" || !strings.Contains(errs, "later version") || len(f.paths) != 0 {
		t.Fatalf("exit %d %q %q %v", code, out, errs, f.paths)
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
