package importer

import (
	"fmt"
	"slices"
	"strings"
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
	// links are each scanned file's links, by path.
	links map[string][]scan.Link
}

// newLinkIndex knows the plan's validated entries and the rescan's files.
func newLinkIndex(pr prepared) *linkIndex {
	ix := &linkIndex{kind: map[string]string{}, ids: map[string]string{}, pending: map[string]bool{},
		exists: map[string]bool{}, links: map[string][]scan.Link{}}
	for _, f := range pr.res.Files {
		ix.exists[f.Path] = true
		ix.links[f.Path] = f.Links
	}
	for _, e := range pr.entries {
		ix.kind[e.Path] = e.Kind
	}
	return ix
}

// linksIntoPlan reports whether the file at path links to any plan entry,
// so a rewrite may change its bytes.
func (ix *linkIndex) linksIntoPlan(path string) bool {
	for _, l := range ix.links[path] {
		if _, ok := ix.kind[l.Resolved]; ok && !l.External && l.Resolved != "" {
			return true
		}
	}
	return false
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
		if dropped := droppedParts(l); len(dropped) == 1 {
			find(l, "The link now points at the whole %s in Smrit, so %s was dropped.", whole, dropped[0])
		} else if len(dropped) == 2 {
			find(l, "The link now points at the whole %s in Smrit, so %s and %s were dropped.", whole, dropped[0], dropped[1])
		}
		out = append(out, body[from:l.Start]...)
		out = append(out, ref...)
		from = l.End
	}
	out = append(out, body[from:]...)
	return out, findings, waiting
}

// droppedParts are the ?query and the #anchor of a link's target, which a
// link to a whole document or file in Smrit cannot keep.
func droppedParts(l scan.LinkSpan) []string {
	var out []string
	p := l.Target
	if i := strings.IndexByte(p, '#'); i >= 0 {
		p = p[:i]
	}
	if i := strings.IndexByte(p, '?'); i >= 0 && i+1 < len(p) {
		out = append(out, capRunes(p[i:]))
	}
	if len(l.Anchor) > 1 {
		out = append(out, capRunes(l.Anchor))
	}
	return out
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
