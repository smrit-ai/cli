package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/smrit-ai/cli/internal/plan"
	"github.com/smrit-ai/cli/internal/state"
)

const retireRunID = "20261109T143005Z-a1b2c3"

// retireSetup saves a finished run over a fresh folder holding one imported
// file outside any repository, and returns the folder.
func retireSetup(t *testing.T, phase string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := "a private body\n"
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(body))
	dir, err := state.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := state.EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	r := &state.Run{ID: retireRunID, Root: root, Org: state.Org{ID: "o", Name: "O", Slug: "o"},
		Plan:    plan.Plan{Version: 1, Root: root, Entries: []plan.PlanEntry{{Path: "a.md", Kind: "document"}}},
		Entries: map[string]state.EntryState{"a.md": {Kind: "document", SourceSHA256: hex.EncodeToString(h[:]), Status: state.StatusCreated, ID: "DocAAAAAAAAAAAAAAAAAA"}},
		Phase:   phase}
	if err := state.SaveRun(dir, r); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRetireReportsThenApplies(t *testing.T) {
	root := retireSetup(t, state.PhaseFinished)
	code, out, errs := runCmd(t, nil, "retire", retireRunID)
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var rep map[string]any
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var keys []string
	for k := range rep {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"applied", "deleted", "exclude_added", "left", "recoverable_unpushed", "root", "run", "zip", "zipped"}
	if !slices.Equal(keys, want) {
		t.Errorf("keys %v, want %v", keys, want)
	}
	zp, _ := rep["zip"].(string)
	if rep["applied"] != false || !strings.HasPrefix(zp, filepath.Join(root, "smrit-import-")) || !strings.HasSuffix(zp, ".zip") {
		t.Errorf("report: %s", out)
	}
	if _, err := os.Stat(filepath.Join(root, "a.md")); err != nil {
		t.Fatal("a report without --apply removed the file")
	}
	if strings.Contains(out, "private body") {
		t.Error("file content in the output")
	}

	code, out, errs = runCmd(t, nil, "--json=false", "retire", retireRunID, "--apply")
	if code != ExitOK {
		t.Fatalf("apply exit %d: %s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(root, "a.md")); err == nil {
		t.Error("the file is still there")
	}
	if !strings.Contains(out, "1 zipped") || !strings.Contains(out, filepath.Join(root, "smrit-import-")) {
		t.Errorf("text report: %q", out)
	}
	if strings.ContainsAny(out+errs, "—–") || strings.Contains(out+errs, " - ") || strings.Contains(out, "private body") {
		t.Errorf("text: %q", out)
	}
}

func TestRetireRefusals(t *testing.T) {
	retireSetup(t, state.PhaseLinks)
	if code, _, errs := runCmd(t, nil, "retire", retireRunID, "--apply"); code != ExitRefused || !strings.Contains(errs, "has not finished") {
		t.Errorf("unfinished: %d %q", code, errs)
	}
	if code, _, errs := runCmd(t, nil, "retire", "20261109T000000Z-000000"); code != ExitRefused || !strings.Contains(errs, "No import run") {
		t.Errorf("unknown: %d %q", code, errs)
	}
	if code, _, _ := runCmd(t, nil, "retire"); code != ExitUsage {
		t.Errorf("no id: %d", code)
	}
	if code, _, _ := runCmd(t, nil, "retire", "a", "b"); code != ExitUsage {
		t.Errorf("two ids: %d", code)
	}
}

func TestRetireStoppedExits1WithTheReport(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a read only folder")
	}
	root := retireSetup(t, state.PhaseFinished)
	// The zip is written in the folder, then a.md cannot be removed from
	// it: make a.md live in a read only folder instead.
	sub := filepath.Join(root, "ro")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "a.md"), filepath.Join(sub, "a.md")); err != nil {
		t.Fatal(err)
	}
	dir, _ := state.Dir()
	r, err := state.LoadRun(dir, retireRunID)
	if err != nil {
		t.Fatal(err)
	}
	r.Plan.Entries[0].Path = "ro/a.md"
	r.Entries["ro/a.md"] = r.Entries["a.md"]
	delete(r.Entries, "a.md")
	if err := state.SaveRun(dir, r); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })

	code, out, errs := runCmd(t, nil, "retire", retireRunID, "--apply")
	if code != ExitPartial || !strings.Contains(errs, "could not be removed") {
		t.Fatalf("exit %d: %q", code, errs)
	}
	var rep struct {
		Zip  *string `json:"zip"`
		Left []struct {
			Path string `json:"path"`
		} `json:"left"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil || rep.Zip == nil || len(rep.Left) != 1 || rep.Left[0].Path != "ro/a.md" {
		t.Errorf("report: %v %s", err, out)
	}
}
