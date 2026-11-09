package state

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNewRunIDIsUTCStampAndSixHex(t *testing.T) {
	loc := time.FixedZone("x", 5*3600+1800)
	id, err := NewRunID(time.Date(2026, 11, 9, 20, 0, 5, 0, loc), bytes.NewReader([]byte{0xa1, 0xb2, 0xc3}))
	if err != nil || id != "20261109T143005Z-a1b2c3" {
		t.Fatalf("%q %v", id, err)
	}
	if !regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{6}$`).MatchString(id) {
		t.Fatal(id)
	}
}

func TestRunSavedPrivateAndLoadedBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "smrit")
	r := &Run{ID: "20261109T143005Z-a1b2c3", Root: "/docs", PlanSHA256: "ab", Phase: PhaseDocuments,
		Entries: map[string]EntryState{"a.md": {Kind: "document", SourceSHA256: "s", SentSHA256: "t", Status: StatusCreated, ID: "D1", Phase: PhaseDocuments}}}
	if err := SaveRun(dir, r); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "runs", r.ID+".json")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "runs")); st.Mode().Perm() != 0o700 {
		t.Fatalf("runs mode %o", st.Mode().Perm())
	}
	got, err := LoadRun(dir, r.ID)
	if err != nil || got.Entries["a.md"].ID != "D1" || got.Phase != PhaseDocuments || got.PlanSHA256 != "ab" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestLoadRunRefusesAnyOtherID(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"", "../x", "20261109T143005Z-a1b2c3/../../etc", "20261109T143005Z-A1B2C3", "latest"} {
		if _, err := LoadRun(dir, id); err == nil || !strings.Contains(err.Error(), "No import run") {
			t.Errorf("%q: %v", id, err)
		}
	}
	if _, err := LoadRun(dir, "20261109T143005Z-a1b2c3"); err == nil || !strings.Contains(err.Error(), "No import run") {
		t.Errorf("missing: %v", err)
	}
}
