package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/smrit-ai/cli/internal/plan"
	"github.com/smrit-ai/cli/internal/scan"
	"github.com/smrit-ai/cli/internal/state"
)

// Importer is the route side a real run needs: preview, then commit.
type Importer interface {
	Previewer
	Commit(ctx context.Context, space string, batch []Upload) ([]CommitItem, error)
}

// RunOptions is what Run works with besides the plan.
type RunOptions struct {
	MCP   Tools
	Route Importer
	// Scan runs the rescan of the plan's root; nil means scan.Run.
	Scan func(ctx context.Context, root string) (scan.Result, error)
	// StateDir is the state directory; runs are kept in its runs folder.
	StateDir string
	// PlanSHA256 is the hash of the plan file's bytes. A resume refuses a
	// plan whose hash differs from the one the run recorded.
	PlanSHA256 string
	// Resume is the id of a run to carry on; empty starts a new one.
	Resume string
	// Now and Random make the run id; nil means the real ones.
	Now    func() time.Time
	Random io.Reader
	// Began, when set, is told the organization and the run id once both
	// are known, before anything is written to Smrit.
	Began func(org Org, run string)

	// trove and uploadHTTP stand in for the compiled file store origin and
	// the upload client in this package's tests only; nothing outside the
	// package can set them, so a real run always uses server.TroveOrigin.
	trove      string
	uploadHTTP *http.Client
}

// RunEntry is what the run did with one plan entry.
type RunEntry struct {
	Path     string    `json:"path"`
	Kind     string    `json:"kind"`
	Space    string    `json:"space"`
	Status   string    `json:"status"` // created, updated, unchanged, exists, failed, refused, skipped, pending
	ID       string    `json:"id"`
	Error    string    `json:"error"`
	Type     string    `json:"type"`
	TypeFrom string    `json:"type_from"`
	Slug     string    `json:"slug"`
	Title    string    `json:"title"`
	Similar  []Similar `json:"similar"`
	// BlankRowsSkipped is how many rows of a CSV had no value in any
	// column and were left out.
	BlankRowsSkipped int `json:"blank_rows_skipped"`
}

// RunReport is a whole run.
type RunReport struct {
	Organization Org    `json:"organization"`
	Run          string `json:"run"`
	// Map is the import map document's id; MapError says why there is
	// none, or why it was not brought up to date.
	Map       string        `json:"map"`
	MapError  string        `json:"map_error"`
	Entries   []RunEntry    `json:"entries"`
	Links     []LinkFinding `json:"links"`
	Created   int           `json:"created"`
	Updated   int           `json:"updated"`
	Unchanged int           `json:"unchanged"`
	Exists    int           `json:"exists"`
	Failed    int           `json:"failed"`
	Refused   int           `json:"refused"`
	Skipped   int           `json:"skipped"`
}

// Sentences of a run.
const (
	// statusPending is an entry the run had not reached when it stopped.
	statusPending = "pending"
	hashMismatch  = "The bytes Smrit received differ from the bytes sent."
	noAnswer      = "Smrit gave no answer for this file."
)

// runner is one run in progress.
type runner struct {
	ctx  context.Context
	o    RunOptions
	p    plan.Plan
	pr   prepared
	st   *state.Run
	out  []RunEntry
	docs []plan.Entry
	ix   *linkIndex
	// findings are each document's link findings from its last rewrite.
	findings map[string][]LinkFinding
	mapError string
	// stored is each object finished runs of this folder hold, by path,
	// source hash, scope and type; nil until the first object needs it.
	stored map[storedKey]string
}

// Run imports a plan: objects, documents, workbooks, a second pass for the
// links to documents the run created, and the import map. It validates
// exactly as DryRun does, against a scan of the root made now, and every
// entry it does not import is in the report with its reason. The run state
// is saved after every result, so a run that stops can be resumed; the
// returned report carries the run id even with an error.
func Run(ctx context.Context, p plan.Plan, o RunOptions) (RunReport, error) {
	var rep RunReport
	pr, err := prepare(ctx, p, o.MCP, o.Scan)
	rep.Organization = pr.org
	if err != nil {
		return rep, err
	}
	r := &runner{ctx: ctx, o: o, p: p, pr: pr, out: make([]RunEntry, len(p.Entries)),
		findings: map[string][]LinkFinding{}, ix: newLinkIndex(pr)}
	unlock, err := r.begin()
	if err != nil {
		return rep, err
	}
	defer unlock()
	rep.Run = r.st.ID
	if o.Began != nil {
		o.Began(pr.org, r.st.ID)
	}
	err = r.all()
	return r.report(rep), err
}

func (r *runner) all() error {
	for _, x := range r.pr.refusals {
		// A file Smrit already holds from this run stays done, with its id,
		// even when the folder no longer has it as it was: the import map
		// keeps it, and a link to it still resolves.
		if s, ok := r.st.Entries[x.Path]; ok && state.Done(s.Status) && s.ID != "" {
			msg := x.Reason + " Smrit keeps what this run imported earlier."
			if !r.ix.exists[x.Path] {
				msg = "This file is no longer in the folder, so it was not checked again. Smrit keeps what this run imported."
			}
			r.out[x.Index] = RunEntry{Path: x.Path, Kind: x.Kind, Space: x.Space, Status: s.Status, ID: s.ID, Error: msg, Similar: []Similar{}}
			r.ix.kind[x.Path], r.ix.ids[x.Path] = s.Kind, s.ID
			continue
		}
		r.out[x.Index] = RunEntry{Path: x.Path, Kind: x.Kind, Space: x.Space, Status: state.StatusRefused, Error: x.Reason, Similar: []Similar{}}
		r.st.Entries[x.Path] = state.EntryState{Kind: x.Kind, Status: state.StatusRefused, Error: x.Reason}
	}
	var objects, workbooks []plan.Entry
	for _, e := range r.pr.entries {
		r.out[e.Index] = RunEntry{Path: e.Path, Kind: e.Kind, Space: e.Space, Status: statusPending, Type: e.Type, Slug: e.Slug, Title: e.Title, Similar: []Similar{}}
		switch e.Kind {
		case scan.KindDocument:
			r.docs = append(r.docs, e)
		case scan.KindObject:
			objects = append(objects, e)
		case scan.KindWorkbook:
			workbooks = append(workbooks, e)
		}
		// An id the run already holds is a link target before anything is
		// sent; a workbook's is its describing document's.
		if s, ok := r.st.Entries[e.Path]; ok && state.Done(s.Status) && s.ID != "" {
			r.ix.ids[e.Path] = s.ID
		}
	}
	steps := []struct {
		phase string
		do    func() error
	}{
		{state.PhaseObjects, func() error { return r.objects(objects) }},
		{state.PhaseDocuments, r.documents},
		{state.PhaseWorkbooks, func() error { return r.workbooks(workbooks) }},
		{state.PhaseLinks, r.linkPass},
		{state.PhaseMap, r.importMap},
	}
	for _, s := range steps {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		r.st.Phase = s.phase
		if err := r.save(); err != nil {
			return err
		}
		if err := s.do(); err != nil {
			return err
		}
	}
	r.st.Phase = state.PhaseFinished
	return r.save()
}

// documents previews every batch first, so every target that already
// exists in Smrit has its id before anything is sent, previews again the
// files whose links that changes, as the dry run does, then commits each
// batch with its links rewritten.
func (r *runner) documents() error {
	var todo []plan.Entry
	for _, e := range r.docs {
		if r.alreadyDone(e) {
			continue
		}
		todo = append(todo, e)
	}
	res, err := previewAll(r.ctx, r.o.Route, todo, r.ix)
	if err != nil {
		return err
	}
	var alive []plan.Entry
	for _, x := range res {
		e := x.Entry
		if x.Refused != "" {
			r.refuse(e, x.Refused)
			continue
		}
		o := &r.out[e.Index]
		fillFromPreview(&o.Type, &o.Slug, &o.Title, &o.TypeFrom, &o.Similar, x.Item)
		if action, reason := previewAction(x.Item); action == ActionRefused {
			r.refuse(e, reason)
			continue
		}
		alive = append(alive, e)
	}
	if err := r.save(); err != nil {
		return err
	}
	for _, batch := range Batches(alive) {
		if err := r.commit(batch); err != nil {
			return err
		}
	}
	// What is still pending did not get an id: a link to it stays as written.
	clear(r.ix.pending)
	return nil
}

// alreadyDone carries a resumed entry Smrit already holds, unless its bytes
// on disk changed since.
func (r *runner) alreadyDone(e plan.Entry) bool {
	s, ok := r.st.Entries[e.Path]
	if !ok || !state.Done(s.Status) || s.SourceSHA256 != e.SHA256 || s.ID == "" {
		return false
	}
	o := &r.out[e.Index]
	o.Status, o.ID = s.Status, s.ID
	return true
}

// linkPass rewrites again every done document that links into the plan,
// with every id now known, and sends only those whose bytes differ from the
// ones Smrit holds. It keeps nothing in memory between runs: a target whose
// id arrived in this run, in an earlier one, or whose answer was lost and
// came back on resume as unchanged, is all the same to it.
func (r *runner) linkPass() error {
	var again []plan.Entry
	for _, e := range r.docs {
		s := r.st.Entries[e.Path]
		if state.Done(s.Status) && s.SourceSHA256 == e.SHA256 && r.ix.linksIntoPlan(e.Path) {
			again = append(again, e)
		}
	}
	for _, batch := range Batches(again) {
		if err := r.commit(batch); err != nil {
			return err
		}
	}
	return nil
}

// commit reads each file of batch, rewrites its links with the ids known
// now and commits it. In the link pass a file whose rewritten bytes are the
// ones already sent is not sent again.
func (r *runner) commit(batch []plan.Entry) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	links := r.st.Phase == state.PhaseLinks
	var ups []Upload
	var sent []plan.Entry
	hashes := map[string]string{}
	for _, e := range batch {
		data, err := readExact(e)
		if err != nil {
			if links {
				r.fail(e, err.Error())
			} else {
				r.refuse(e, err.Error())
			}
			continue
		}
		body, finds, _ := rewriteLinks(data, e.Path, r.ix)
		r.findings[e.Path] = finds
		h := sha256.Sum256(body)
		hashes[e.Path] = hex.EncodeToString(h[:])
		if links && hashes[e.Path] == r.st.Entries[e.Path].SentSHA256 {
			continue
		}
		ups = append(ups, Upload{Path: e.Path, Data: body, Type: e.Type, Title: e.Title, Slug: e.Slug})
		sent = append(sent, e)
	}
	if len(ups) == 0 {
		return r.save()
	}
	items, err := r.o.Route.Commit(r.ctx, batch[0].Space, ups)
	if err != nil {
		var re *RouteError
		if !errors.As(err, &re) {
			return err
		}
		for _, e := range sent {
			r.fail(e, re.Msg)
		}
		return r.save()
	}
	byPath := map[string]CommitItem{}
	for _, it := range items {
		byPath[it.RelativePath] = it
	}
	for _, e := range sent {
		it, ok := byPath[e.Path]
		switch {
		case !ok:
			r.fail(e, noAnswer)
		case !state.Done(it.Status):
			msg := plainSentence(it.Error)
			if msg == "" {
				msg = "Smrit could not import this file."
			}
			r.fail(e, msg)
		case it.ReceivedSHA256 != hashes[e.Path]:
			r.failWithID(e, hashMismatch, it.DocumentID)
		case it.DocumentID == "":
			r.fail(e, noAnswer)
		default:
			status := it.Status
			if prev := r.st.Entries[e.Path].Status; links && state.Done(prev) && (prev == state.StatusCreated || status == state.StatusUnchanged) {
				status = prev
			}
			r.ix.ids[e.Path] = it.DocumentID
			delete(r.ix.pending, e.Path)
			r.record(e, state.EntryState{Kind: e.Kind, SourceSHA256: e.SHA256, SentSHA256: hashes[e.Path],
				Status: status, ID: it.DocumentID, Phase: r.st.Phase})
		}
	}
	return r.save()
}

func (r *runner) refuse(e plan.Entry, reason string) {
	r.record(e, state.EntryState{Kind: e.Kind, SourceSHA256: e.SHA256, Status: state.StatusRefused, Error: reason, Phase: r.st.Phase})
}

func (r *runner) fail(e plan.Entry, reason string) { r.failWithID(e, reason, "") }

// failWithID marks e failed. The id Smrit gave, or the one the run held, is
// kept for a person to find the document by; a failed entry is never done.
// A link to it uses that id: the document is in Smrit, whatever it holds.
func (r *runner) failWithID(e plan.Entry, reason, id string) {
	if id == "" {
		id = r.st.Entries[e.Path].ID
	}
	if id != "" {
		r.ix.ids[e.Path] = id
	}
	r.record(e, state.EntryState{Kind: e.Kind, SourceSHA256: e.SHA256, Status: state.StatusFailed, ID: id, Error: reason, Phase: r.st.Phase})
}

func (r *runner) record(e plan.Entry, s state.EntryState) {
	r.st.Entries[e.Path] = s
	o := &r.out[e.Index]
	o.Status, o.ID, o.Error = s.Status, s.ID, s.Error
}

func (r *runner) report(rep RunReport) RunReport {
	rep.Map, rep.MapError = r.st.Map, r.mapError
	rep.Entries = r.out
	rep.Links = []LinkFinding{}
	for _, e := range r.out {
		switch e.Status {
		case state.StatusCreated:
			rep.Created++
		case state.StatusUpdated:
			rep.Updated++
		case state.StatusUnchanged:
			rep.Unchanged++
		case state.StatusExists:
			rep.Exists++
		case state.StatusFailed:
			rep.Failed++
		case state.StatusRefused:
			rep.Refused++
		case state.StatusSkipped:
			rep.Skipped++
		}
		if e.Kind == scan.KindDocument && e.Status != state.StatusRefused {
			rep.Links = append(rep.Links, r.findings[e.Path]...)
		}
	}
	return rep
}

var timeNow = time.Now
