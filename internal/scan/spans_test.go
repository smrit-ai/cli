package scan

import (
	"strings"
	"testing"
)

// LinkSpans finds exactly the links parseLinks finds, and each span covers
// the target as written, so a rewrite in place never touches anything else.
func TestLinkSpansLocateTargets(t *testing.T) {
	body := strings.Join([]string{
		"See [the guide](../guide.md#setup) and ![logo](img/logo.png \"Logo\").",
		"Inline `[not a link](code.md)` stays code.",
		"```md",
		"[fenced](fenced.md)",
		"```",
		"[spaced](<my file.md>) [esc](a\\_b.md) [nested (parens)](a_(b).md)",
		"[![badge](badge.svg)](next.md)",
	}, "\n")
	spans := LinkSpans(body, "sub/page.md")
	links := parseLinks(body, "sub/page.md")
	if len(spans) != len(links) {
		t.Fatalf("%d spans, %d links", len(spans), len(links))
	}
	want := []string{"../guide.md#setup", "img/logo.png", "my file.md", "a\\_b.md", "a_(b).md", "next.md", "badge.svg"}
	for i, s := range spans {
		if s.Link != links[i] {
			t.Errorf("span %d link %+v, parseLinks %+v", i, s.Link, links[i])
		}
		if got := body[s.Start:s.End]; got != want[i] {
			t.Errorf("span %d covers %q, want %q", i, got, want[i])
		}
	}
}
