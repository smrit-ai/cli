package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"syscall"

	"github.com/smrit-ai/cli/internal/plan"
	"github.com/smrit-ai/cli/internal/scan"
	"github.com/smrit-ai/cli/internal/state"
)

// Actions in a report.
const (
	ActionCreate    = "create"
	ActionUpdate    = "update"
	ActionUnchanged = "unchanged"
	ActionRefused   = "refused"
)

// Org is the organization the report is about.
type Org = state.Org

// ReportEntry is one planned file and what an import would do with it.
type ReportEntry struct {
	Path     string    `json:"path"`
	Kind     string    `json:"kind"`
	Space    string    `json:"space"`
	Action   string    `json:"action"`
	Reason   string    `json:"reason"`
	Similar  []Similar `json:"similar"`
	TypeFrom string    `json:"type_from"`
	Type     string    `json:"type"`
	Slug     string    `json:"slug"`
	Title    string    `json:"title"`
}

// Report is the whole dry run.
type Report struct {
	Organization Org           `json:"organization"`
	Entries      []ReportEntry `json:"entries"`
	Refused      int           `json:"refused"`
	WouldCreate  int           `json:"would_create"`
	WouldUpdate  int           `json:"would_update"`
	Unchanged    int           `json:"unchanged"`
}

// Tools is the MCP side the dry run needs.
type Tools interface {
	Call(ctx context.Context, tool string, args map[string]any) (json.RawMessage, error)
	ListTools(ctx context.Context) ([]string, error)
}

// Previewer is the route side the dry run needs. It has no commit.
type Previewer interface {
	Preview(ctx context.Context, space string, batch []Upload) ([]PreviewItem, error)
}

// Input is what DryRun works from.
type Input struct {
	Plan  plan.Plan
	MCP   Tools
	Route Previewer
	// Scan runs the rescan of the plan's root; nil means scan.Run.
	Scan func(ctx context.Context, root string) (scan.Result, error)
}

// prepared is a plan checked against a scan made now and the spaces the
// caller can see: what the dry run and the real run both start from.
type prepared struct {
	org      Org
	res      scan.Result
	entries  []plan.Entry
	refusals []plan.Refusal
}

// prepare checks the plan's root, learns the organization, its spaces and
// tools, rescans the root and validates every entry. It writes nothing. A
// root the scan will not read is refused before any request is made.
func prepare(ctx context.Context, p plan.Plan, tools Tools, scanFn func(ctx context.Context, root string) (scan.Result, error)) (prepared, error) {
	var pr prepared
	if err := scan.CheckRoots([]string{p.Root}, scan.Options{}); err != nil {
		return pr, forImport(err)
	}
	raw, err := tools.Call(ctx, "spaces", nil)
	if err != nil {
		return pr, err
	}
	var sp struct {
		Org    Org          `json:"org"`
		Spaces []plan.Space `json:"spaces"`
	}
	if json.Unmarshal(raw, &sp) != nil {
		return pr, errors.New("Smrit sent an answer this CLI cannot read.")
	}
	pr.org = sp.Org
	names, err := tools.ListTools(ctx)
	if err != nil {
		return pr, err
	}
	if scanFn == nil {
		scanFn = func(ctx context.Context, root string) (scan.Result, error) {
			return scan.Run(ctx, []string{root}, scan.Options{})
		}
	}
	res, err := scanFn(ctx, p.Root)
	if err != nil {
		var re *scan.RefusedError
		var ue *scan.UsageError
		if errors.As(err, &re) || errors.As(err, &ue) {
			return pr, forImport(err)
		}
		if ctx.Err() != nil {
			return pr, err
		}
		return pr, fmt.Errorf("The plan's folder could not be scanned: %v", err)
	}
	pr.res = res
	pr.entries, pr.refusals = plan.Validate(ctx, p, res, sp.Spaces, slices.Contains(names, "create_object"))
	return pr, nil
}

// previewed is one file of a previewed batch: the preview's answer, or the
// sentence saying why the file has none.
type previewed struct {
	Entry   plan.Entry
	Item    PreviewItem
	Refused string
}

// previewBatch reads each file of batch, checking its bytes against the
// rescan, and previews those it could read. A refusal of the whole batch by
// the route refuses each file; any other failure is returned.
func previewBatch(ctx context.Context, route Previewer, batch []plan.Entry) ([]previewed, error) {
	out := make([]previewed, len(batch))
	var ups []Upload
	var sent []int
	for i, e := range batch {
		out[i].Entry = e
		data, err := readExact(e)
		if err != nil {
			out[i].Refused = err.Error()
			continue
		}
		ups = append(ups, Upload{Path: e.Path, Data: data, Type: e.Type, Title: e.Title, Slug: e.Slug})
		sent = append(sent, i)
	}
	if len(ups) == 0 {
		return out, nil
	}
	items, err := route.Preview(ctx, batch[0].Space, ups)
	if err != nil {
		var re *RouteError
		if !errors.As(err, &re) {
			return nil, err
		}
		for _, i := range sent {
			out[i].Refused = re.Msg
		}
		return out, nil
	}
	byPath := map[string]PreviewItem{}
	for _, it := range items {
		byPath[it.RelativePath] = it
	}
	for _, i := range sent {
		it, ok := byPath[out[i].Entry.Path]
		if !ok {
			out[i].Refused = "Smrit gave no answer for this file."
			continue
		}
		out[i].Item = it
	}
	return out, nil
}

// previewAction is what a preview item says will happen, with its reason as
// one plain sentence.
func previewAction(it PreviewItem) (action, reason string) {
	switch it.Action {
	case "create":
		action = ActionCreate
	case "update":
		action = ActionUpdate
	case "unchanged":
		action = ActionUnchanged
	default:
		action = ActionRefused
		if it.Reason == "" {
			it.Reason = "Smrit would not import this file."
		}
	}
	return action, plainSentence(it.Reason)
}

// DryRun previews a plan and writes nothing. It scans the plan's root again
// now and trusts no earlier scan. Documents are sent to the preview route;
// workbooks and objects are checked here and reported as create.
func DryRun(ctx context.Context, in Input) (Report, error) {
	var rep Report
	pr, err := prepare(ctx, in.Plan, in.MCP, in.Scan)
	rep.Organization = pr.org
	if err != nil {
		return rep, err
	}
	out := make([]ReportEntry, len(in.Plan.Entries))
	for _, r := range pr.refusals {
		out[r.Index] = ReportEntry{Path: r.Path, Kind: r.Kind, Space: r.Space, Action: ActionRefused, Reason: r.Reason, Similar: []Similar{}}
	}
	var docs []plan.Entry
	for _, e := range pr.entries {
		out[e.Index] = ReportEntry{Path: e.Path, Kind: e.Kind, Space: e.Space, Action: ActionCreate, Similar: []Similar{},
			Type: e.Type, Slug: e.Slug, Title: e.Title}
		if e.Kind == scan.KindDocument {
			docs = append(docs, e)
		}
	}
	for _, batch := range Batches(docs) {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		res, err := previewBatch(ctx, in.Route, batch)
		if err != nil {
			return rep, err
		}
		for _, x := range res {
			r := &out[x.Entry.Index]
			if x.Refused != "" {
				r.Action, r.Reason = ActionRefused, x.Refused
				continue
			}
			it := x.Item
			r.Action, r.Reason = previewAction(it)
			fillFromPreview(&r.Type, &r.Slug, &r.Title, &r.TypeFrom, &r.Similar, it)
		}
	}
	for _, r := range out {
		switch r.Action {
		case ActionRefused:
			rep.Refused++
		case ActionCreate:
			rep.WouldCreate++
		case ActionUpdate:
			rep.WouldUpdate++
		case ActionUnchanged:
			rep.Unchanged++
		}
	}
	rep.Entries = out
	return rep, nil
}

// fillFromPreview takes what the preview resolved over what the plan said.
func fillFromPreview(typ, slug, title, typeFrom *string, similar *[]Similar, it PreviewItem) {
	if it.Slug != "" {
		*slug = it.Slug
	}
	if it.Type != "" {
		*typ = it.Type
	}
	if it.Title != "" {
		*title = it.Title
	}
	*typeFrom = it.TypeFrom
	if it.Similar != nil {
		*similar = it.Similar
	}
}

// Batches groups document entries by space, then cuts each space's run into
// batches of at most MaxBatchFiles files and MaxBatchBytes bytes, in plan
// order. A single file larger than the byte limit is a batch of its own.
func Batches(entries []plan.Entry) [][]plan.Entry {
	var order []string
	by := map[string][]plan.Entry{}
	for _, e := range entries {
		if _, ok := by[e.Space]; !ok {
			order = append(order, e.Space)
		}
		by[e.Space] = append(by[e.Space], e)
	}
	var out [][]plan.Entry
	for _, sp := range order {
		var cur []plan.Entry
		var size int64
		for _, e := range by[sp] {
			if len(cur) > 0 && (len(cur) >= MaxBatchFiles || size+e.Size > MaxBatchBytes) {
				out = append(out, cur)
				cur, size = nil, 0
			}
			cur = append(cur, e)
			size += e.Size
		}
		if len(cur) > 0 {
			out = append(out, cur)
		}
	}
	return out
}

// readExact reads the file the rescan listed, refusing a link and any file
// whose bytes are not the ones the rescan hashed.
func readExact(e plan.Entry) ([]byte, error) {
	changed := errors.New("This file changed while it was being read.")
	f, err := os.OpenFile(e.Abs, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("This file could not be read.")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("This file could not be read.")
	}
	data, err := io.ReadAll(io.LimitReader(f, scan.MaxDocumentBytes+1))
	if err != nil {
		return nil, errors.New("This file could not be read.")
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != e.Size || hex.EncodeToString(sum[:]) != e.SHA256 {
		return nil, changed
	}
	return data, nil
}

// forImport words a scan refusal for smrit import, which shares the scan's
// rules but is not smrit scan.
func forImport(err error) error {
	var re *scan.RefusedError
	var ue *scan.UsageError
	switch {
	case errors.As(err, &re):
		return &scan.RefusedError{Msg: importWords(re.Msg)}
	case errors.As(err, &ue):
		return &scan.UsageError{Msg: importWords(ue.Msg)}
	}
	return err
}

func importWords(s string) string {
	s = strings.ReplaceAll(s, "smrit scan will not scan", "smrit import will not read")
	s = strings.ReplaceAll(s, "smrit scan does not follow", "smrit import does not follow")
	return strings.ReplaceAll(s, "smrit scan", "smrit import")
}
