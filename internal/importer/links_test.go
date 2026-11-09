package importer

import (
	"strings"
	"testing"
)

func TestRewriteLinksByOffset(t *testing.T) {
	ix := &linkIndex{
		kind:    map[string]string{"guide.md": "document", "img/logo.png": "object", "old.md": "document", "later.md": "document"},
		ids:     map[string]string{"guide.md": "GuideId", "img/logo.png": "ObjId"},
		pending: map[string]bool{"later.md": true},
		exists:  map[string]bool{"guide.md": true, "img/logo.png": true, "old.md": true, "later.md": true, "notes.txt": true},
	}
	body := strings.Join([]string{
		"[Guide](guide.md) [guide.md](guide.md) ![Logo](img/logo.png \"Logo\")",
		"[later](later.md) [old](old.md) [notes](notes.txt) [gone](gone.md) [up](../up.md)",
		"[web](https://example.com/guide.md) [here](#top) `[Guide](guide.md)`",
		"[![Logo](<img/logo.png>)](guide.md#intro)",
	}, "\n")
	out, findings, waiting := rewriteLinks([]byte(body), "page.md", ix)
	want := strings.Join([]string{
		"[Guide](smrit:d:GuideId) [guide.md](smrit:d:GuideId) ![Logo](smrit:object/ObjId \"Logo\")",
		"[later](later.md) [old](old.md) [notes](notes.txt) [gone](gone.md) [up](../up.md)",
		"[web](https://example.com/guide.md) [here](#top) `[Guide](guide.md)`",
		"[![Logo](<smrit:object/ObjId>)](smrit:d:GuideId)",
	}, "\n")
	if string(out) != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	if !waiting {
		t.Fatal("a link to a document still being imported should leave the body waiting")
	}
	got := map[string]string{}
	for _, f := range findings {
		if f.Path != "page.md" {
			t.Errorf("finding path %q", f.Path)
		}
		got[f.Target] = f.Reason
	}
	for target, words := range map[string]string{
		"old.md":         "not imported",
		"notes.txt":      "does not import",
		"gone.md":        "nothing",
		"../up.md":       "outside",
		"guide.md#intro": "#intro",
	} {
		if !strings.Contains(got[target], words) {
			t.Errorf("%s: %q, want it to say %q", target, got[target], words)
		}
	}
	if len(findings) != 5 {
		t.Errorf("findings %+v", findings)
	}
	for _, f := range findings {
		if strings.ContainsAny(f.Reason, "—–") || strings.Contains(f.Reason, " - ") {
			t.Errorf("dash in %q", f.Reason)
		}
	}
}
