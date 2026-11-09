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
	"syscall"

	"github.com/smrit-ai/cli/internal/plan"
	"github.com/smrit-ai/cli/internal/scan"
)

// Actions in a report.
const (
	ActionCreate    = "create"
	ActionUpdate    = "update"
	ActionUnchanged = "unchanged"
	ActionRefused   = "refused"
)

// Org is the organization the report is about.
type Org struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

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

// DryRun previews a plan and writes nothing. It scans the plan's root again
// now and trusts no earlier scan. Documents are sent to the preview route;
// workbooks and objects are checked here and reported as create.
func DryRun(ctx context.Context, in Input) (Report, error) {
	var rep Report
	raw, err := in.MCP.Call(ctx, "spaces", nil)
	if err != nil {
		return rep, err
	}
	var sp struct {
		Org    Org          `json:"org"`
		Spaces []plan.Space `json:"spaces"`
	}
	if json.Unmarshal(raw, &sp) != nil {
		return rep, errors.New("Smrit sent an answer this CLI cannot read.")
	}
	rep.Organization = sp.Org
	tools, err := in.MCP.ListTools(ctx)
	if err != nil {
		return rep, err
	}
	scanFn := in.Scan
	if scanFn == nil {
		scanFn = func(ctx context.Context, root string) (scan.Result, error) {
			return scan.Run(ctx, []string{root}, scan.Options{})
		}
	}
	res, err := scanFn(ctx, in.Plan.Root)
	if err != nil {
		var re *scan.RefusedError
		var ue *scan.UsageError
		if errors.As(err, &re) || errors.As(err, &ue) {
			return rep, err
		}
		if ctx.Err() != nil {
			return rep, err
		}
		return rep, fmt.Errorf("The plan's folder could not be scanned: %v", err)
	}
	entries, refusals := plan.Validate(ctx, in.Plan, res, sp.Spaces, slices.Contains(tools, "create_object"))

	out := make([]ReportEntry, len(in.Plan.Entries))
	for _, r := range refusals {
		out[r.Index] = ReportEntry{Path: r.Path, Kind: r.Kind, Space: r.Space, Action: ActionRefused, Reason: r.Reason, Similar: []Similar{}}
	}
	var docs []plan.Entry
	for _, e := range entries {
		out[e.Index] = ReportEntry{Path: e.Path, Kind: e.Kind, Space: e.Space, Action: ActionCreate, Similar: []Similar{},
			Type: e.Type, Slug: e.Slug, Title: e.Title}
		if e.Kind == scan.KindDocument {
			docs = append(docs, e)
		} else if e.Kind == scan.KindWorkbook {
			out[e.Index].Title = e.Title
		}
	}
	for _, batch := range Batches(docs) {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		space := batch[0].Space
		var ups []Upload
		var sent []plan.Entry
		for _, e := range batch {
			data, err := readExact(e)
			if err != nil {
				out[e.Index].Action, out[e.Index].Reason = ActionRefused, err.Error()
				continue
			}
			ups = append(ups, Upload{Path: e.Path, Data: data, Type: e.Type, Title: e.Title, Slug: e.Slug})
			sent = append(sent, e)
		}
		if len(ups) == 0 {
			continue
		}
		items, err := in.Route.Preview(ctx, space, ups)
		if err != nil {
			var re *RouteError
			if !errors.As(err, &re) {
				return rep, err
			}
			for _, e := range sent {
				out[e.Index].Action, out[e.Index].Reason = ActionRefused, re.Msg
			}
			continue
		}
		byPath := map[string]PreviewItem{}
		for _, it := range items {
			byPath[it.RelativePath] = it
		}
		for _, e := range sent {
			r := &out[e.Index]
			it, ok := byPath[e.Path]
			if !ok {
				r.Action, r.Reason = ActionRefused, "Smrit gave no answer for this file."
				continue
			}
			switch it.Action {
			case "create":
				r.Action = ActionCreate
			case "update":
				r.Action = ActionUpdate
			case "unchanged":
				r.Action = ActionUnchanged
			default:
				r.Action = ActionRefused
				if it.Reason == "" {
					it.Reason = "Smrit would not import this file."
				}
			}
			r.Reason = plainSentence(it.Reason)
			if it.Slug != "" {
				r.Slug = it.Slug
			}
			if it.Type != "" {
				r.Type = it.Type
			}
			if it.Title != "" {
				r.Title = it.Title
			}
			r.TypeFrom = it.TypeFrom
			if it.Similar != nil {
				r.Similar = it.Similar
			}
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
