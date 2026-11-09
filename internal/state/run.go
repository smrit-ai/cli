package state

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/smrit-ai/cli/internal/plan"
)

// Phases of an import run, in order. A run's Phase is the one it is in, or
// last reached; an entry's is the phase that last wrote it.
const (
	PhaseObjects   = "objects"
	PhaseDocuments = "documents"
	PhaseWorkbooks = "workbooks"
	PhaseLinks     = "links"
	PhaseMap       = "map"
	PhaseFinished  = "finished"
)

// Statuses of an entry. Created, updated and unchanged are done: Smrit holds
// the file's bytes.
const (
	StatusCreated   = "created"
	StatusUpdated   = "updated"
	StatusUnchanged = "unchanged"
	StatusFailed    = "failed"
	StatusRefused   = "refused"
	StatusSkipped   = "skipped"
	// StatusExists is a workbook whose describing document Smrit already
	// has under the same name, from an earlier run whose state is gone.
	// Nothing was written for it.
	StatusExists = "exists"
)

// Done reports whether status means Smrit holds the entry.
func Done(status string) bool {
	return status == StatusCreated || status == StatusUpdated || status == StatusUnchanged
}

// Org is the organization a run writes into.
type Org struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// EntryState is what a run did with one plan entry.
type EntryState struct {
	Kind string `json:"kind"`
	// SourceSHA256 is the hash of the bytes read from disk; SentSHA256 of
	// the bytes sent, after links were rewritten.
	SourceSHA256 string `json:"source_sha256"`
	SentSHA256   string `json:"sent_sha256,omitempty"`
	Status       string `json:"status"`
	// ID is the document's or object's id in Smrit, once known; for a
	// workbook, its describing document's.
	ID string `json:"id,omitempty"`
	// Workbook is a workbook entry's workbook id, once made, and Rows how
	// many of its CSV rows Smrit was last known to hold, so a resume
	// carries on without writing a row twice.
	Workbook string `json:"workbook,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	Error    string `json:"error,omitempty"`
	Phase    string `json:"phase,omitempty"`
}

// Run is one import run, kept so it can be resumed and later retired.
type Run struct {
	ID string `json:"id"`
	// Root is the plan's root folder, absolute.
	Root string `json:"root"`
	Org  Org    `json:"org"`
	// PlanSHA256 is the hash of the plan file's bytes; Plan is the plan.
	PlanSHA256 string                `json:"plan_sha256"`
	Plan       plan.Plan             `json:"plan"`
	Entries    map[string]EntryState `json:"entries"`
	Phase      string                `json:"phase"`
	// Map is the import map document's id, once made.
	Map string `json:"map,omitempty"`
}

var runIDRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}$`)

// NewRunID is now in UTC, to the second, and six random hex characters:
// 20261109T143005Z-a1b2c3.
func NewRunID(now time.Time, random io.Reader) (string, error) {
	b := make([]byte, 3)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", errors.New("Could not make a run id.")
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b), nil
}

// RunsDir is where runs are kept under the state directory dir.
func RunsDir(dir string) string { return filepath.Join(dir, "runs") }

// RunPath is the file of run id under dir, or "" when id is not a run id.
func RunPath(dir, id string) string {
	if !runIDRE.MatchString(id) {
		return ""
	}
	return filepath.Join(RunsDir(dir), id+".json")
}

// SaveRun writes r atomically, mode 0600, under dir's runs folder.
func SaveRun(dir string, r *Run) error {
	p := RunPath(dir, r.ID)
	if p == "" {
		return errors.New("Could not save the import run.")
	}
	if err := EnsureDir(RunsDir(dir)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return errors.New("Could not save the import run.")
	}
	if err := WriteFile(p, append(data, '\n')); err != nil {
		return fmt.Errorf("Could not save the import run: %v", err)
	}
	return nil
}

// RunIDs lists the ids of the runs kept under dir, oldest first. A missing
// runs folder holds none.
func RunIDs(dir string) ([]string, error) {
	des, err := os.ReadDir(RunsDir(dir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, de := range des {
		id, ok := strings.CutSuffix(de.Name(), ".json")
		if ok && de.Type().IsRegular() && runIDRE.MatchString(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// LoadRun reads run id from dir.
func LoadRun(dir, id string) (*Run, error) {
	missing := errors.New("No import run has that id. The id is printed when a run begins.")
	p := RunPath(dir, id)
	if p == "" {
		return nil, missing
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, missing
	}
	defer f.Close()
	var r Run
	dec := json.NewDecoder(io.LimitReader(f, 64<<20))
	if err := dec.Decode(&r); err != nil || r.ID != id || r.Entries == nil {
		return nil, errors.New("The saved import run is damaged, so it cannot be resumed.")
	}
	return &r, nil
}
