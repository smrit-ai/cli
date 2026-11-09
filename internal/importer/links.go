package importer

import (
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/smrit-ai/cli/internal/scan"
)

// LinkFinding is a link a person should look at: one left as written, or
// one rewritten with part of it dropped.
type LinkFinding struct {
	Path   string `json:"path"`
	Target string `json:"target"`
	Reason string `json:"reason"`
}

// linkIndex is what a rewrite knows about the plan and the run.
type linkIndex struct {
	// kind is each imported plan entry's kind, by root relative path.
	kind map[string]string
	// ids are the ids in Smrit known so far, by path.
	ids map[string]string
	// pending are documents that will get an id later in this run.
	pending map[string]bool
	// exists are the files the rescan found.
	exists map[string]bool
}

// rewriteLinks replaces the target of every inline link and image in body
// that points at a plan entry with a known id: smrit:d:<id> for a document
// or a workbook, smrit:object/<id> for an object. The link text, any title
// and every other byte stay as they are. It works from the scan's own
// parser, by the byte range of each parsed target, so a fenced block or a
// code span is never touched and no text is replaced by matching.
// waiting reports a link left for later because its target is pending.
func rewriteLinks(body []byte, rel string, ix *linkIndex) (out []byte, findings []LinkFinding, waiting bool) {
	spans := scan.LinkSpans(string(body), rel)
	// A link inside an image's text is listed after the link holding it.
	slices.SortFunc(spans, func(a, b scan.LinkSpan) int { return a.Start - b.Start })
	out = make([]byte, 0, len(body)+64)
	from := 0
	find := func(l scan.LinkSpan, format string, a ...any) {
		findings = append(findings, LinkFinding{Path: rel, Target: capRunes(l.Target), Reason: fmt.Sprintf(format, a...)})
	}
	for _, l := range spans {
		if l.External {
			continue
		}
		if l.Resolved == "" {
			if l.Target != "" && l.Target[0] != '#' {
				find(l, "This link points outside the folder, so it is left as written.")
			}
			continue
		}
		kind, inPlan := ix.kind[l.Resolved]
		id := ix.ids[l.Resolved]
		switch {
		case !inPlan && ix.exists[l.Resolved]:
			find(l, "This link points at a file the plan does not import, so it is left as written.")
			continue
		case !inPlan:
			find(l, "This link points at nothing in the folder, so it is left as written.")
			continue
		case id == "" && ix.pending[l.Resolved]:
			waiting = true
			continue
		case id == "":
			find(l, "This link points at a file that was not imported, so it is left as written.")
			continue
		}
		ref, whole := "smrit:d:"+id, "document"
		if kind == scan.KindObject {
			ref, whole = "smrit:object/"+id, "file"
		}
		if l.Anchor != "" {
			find(l, "The link now points at the whole %s in Smrit, so %s was dropped.", whole, capRunes(l.Anchor))
		}
		out = append(out, body[from:l.Start]...)
		out = append(out, ref...)
		from = l.End
	}
	out = append(out, body[from:]...)
	return out, findings, waiting
}

// capRunes keeps untrusted text from a file to 200 characters.
func capRunes(s string) string {
	if utf8.RuneCountInString(s) <= 200 {
		return s
	}
	n := 0
	for i := range s {
		if n == 199 {
			return s[:i] + "…"
		}
		n++
	}
	return s
}
