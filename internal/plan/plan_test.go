package plan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smrit-ai/cli/internal/scan"
)

func TestPlanSlugDefaults(t *testing.T) {
	long := strings.Repeat("a", 300) + ".md"
	cases := map[string]string{
		"adr/0001.md":          "adr-0001",
		"Ops/Run Book (v2).md": "ops-run-book--v2-",
		"a_b-c/D.txt":          "a_b-c-d",
		"notes.tar.md":         "notes-tar",
		"héllo.md":             "h-llo",
		long:                   strings.Repeat("a", 200),
	}
	for in, want := range cases {
		if got := DefaultSlug(in); got != want {
			t.Errorf("DefaultSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadResolvesRootFromPlanFolder(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plan.json")
	os.WriteFile(file, []byte(`{"version":1,"root":"docs","entries":[{"path":"a.md","kind":"document"}]}`), 0o644)
	p, err := Load(file)
	if err != nil || p.Root != filepath.Join(dir, "docs") {
		t.Fatalf("%+v %v", p, err)
	}
	os.WriteFile(file, []byte(`{"version":1,"root":"/abs/docs","entries":[{"path":"a.md","kind":"document"}]}`), 0o644)
	if p, _ := Load(file); p.Root != "/abs/docs" {
		t.Fatalf("absolute root changed: %s", p.Root)
	}
	for _, bad := range []string{`{"version":2,"root":"d","entries":[{}]}`, `{"version":1,"root":"d","entries":[]}`, `{"version":1,"root":"d","entries":[{"path":"a","kind":"document","bogus":1}]}`, `nope`} {
		os.WriteFile(file, []byte(bad), 0o644)
		if _, err := Load(file); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("accepted a missing plan")
	}
}

var spaces = []Space{
	{ID: "SpaceIdEngineering0001", Slug: "engineering", Name: "Engineering", Visibility: "private"},
	{ID: "SpaceIdPublicDocs00002", Slug: "docs", Name: "Docs", Visibility: "public"},
}

func file(path, kind string, flags ...string) scan.File {
	return scan.File{Root: "/r", Path: path, Size: 10, SHA256: "x", Kind: kind, Flags: flags, Heading: "Heading of " + path}
}

func TestPlanRefusals(t *testing.T) {
	res := scan.Result{
		Files: []scan.File{
			file("ok.md", "document"),
			file("secret.md", "document", scan.FlagSecret),
			file(".env.md", "document", scan.FlagSensitiveName),
			file("data.csv", "workbook"),
			file("logo.png", "object"),
			file("run.py", "other"),
			file("big.md", "document", scan.FlagTooLarge),
			file("dup/a.md", "document"),
			file("dup-a.md", "document"),
			file("bin.md", "document", scan.FlagNotUTF8),
		},
		Skipped: []scan.Skipped{{Path: "link.md", Reason: scan.SkipSymlink}},
	}
	res.Files[6].Size = scan.MaxDocumentBytes + 1
	doc := func(p, space string) PlanEntry {
		return PlanEntry{Path: p, Kind: "document", Space: space, Type: "decision"}
	}
	cases := []struct {
		name    string
		entry   PlanEntry
		objects bool
		want    string // substring of the refusal; "" means accepted
	}{
		{"accepted", doc("ok.md", "engineering"), true, ""},
		{"parent path", doc("../x.md", "engineering"), true, "outside"},
		{"absolute path", doc("/etc/passwd", "engineering"), true, "outside"},
		{"missing", doc("nope.md", "engineering"), true, "not in the folder"},
		{"symlink", doc("link.md", "engineering"), true, "symlink"},
		{"secret", doc("secret.md", "engineering"), true, "secret"},
		{"sensitive name", doc(".env.md", "engineering"), true, "sensitive"},
		{"kind the file does not allow", PlanEntry{Path: "run.py", Kind: "document", Space: "engineering", Type: "x"}, true, "cannot be made"},
		{"workbook from a document", PlanEntry{Path: "ok.md", Kind: "workbook", Space: "engineering", Title: "T"}, true, "cannot be made"},
		{"unknown space", doc("ok.md", "nowhere"), true, "No space"},
		{"public space", doc("ok.md", "docs"), true, "public space"},
		{"public space by id", doc("ok.md", "SpaceIdPublicDocs00002"), true, "public space"},
		{"object without scope", PlanEntry{Path: "logo.png", Kind: "object"}, true, "scope"},
		{"object when unavailable", PlanEntry{Path: "logo.png", Kind: "object", Scope: "organization"}, false, "does not offer objects"},
		{"document over 512 KiB", doc("big.md", "engineering"), true, "512 KiB"},
		{"not utf8", doc("bin.md", "engineering"), true, "UTF-8"},
		{"no type", PlanEntry{Path: "ok.md", Kind: "document", Space: "engineering"}, true, "needs a type"},
		{"workbook without title", PlanEntry{Path: "data.csv", Kind: "workbook", Space: "engineering"}, true, "needs a title"},
		{"bad slug", PlanEntry{Path: "ok.md", Kind: "document", Space: "engineering", Type: "t", Slug: "a/b"}, true, "slug"},
		{"unknown kind", PlanEntry{Path: "ok.md", Kind: "page"}, true, "kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, bad := Validate(context.Background(), Plan{Root: "/r", Entries: []PlanEntry{c.entry}}, res, spaces, c.objects)
			if c.want == "" {
				if len(ok) != 1 || len(bad) != 0 {
					t.Fatalf("want accepted, got %+v", bad)
				}
				return
			}
			if len(ok) != 0 || len(bad) != 1 || !strings.Contains(bad[0].Reason, c.want) {
				t.Fatalf("want refusal containing %q, got ok=%d bad=%+v", c.want, len(ok), bad)
			}
		})
	}

	t.Run("duplicate slug anywhere", func(t *testing.T) {
		p := Plan{Root: "/r", Entries: []PlanEntry{doc("dup/a.md", "engineering"), doc("dup-a.md", "engineering")}}
		ok, bad := Validate(context.Background(), p, res, spaces, true)
		if len(ok) != 0 || len(bad) != 2 || !strings.Contains(bad[0].Reason, "slug dup-a") {
			t.Fatalf("%+v %+v", ok, bad)
		}
	})
	t.Run("duplicate path", func(t *testing.T) {
		p := Plan{Root: "/r", Entries: []PlanEntry{doc("ok.md", "engineering"), doc("ok.md", "engineering")}}
		if _, bad := Validate(context.Background(), p, res, spaces, true); len(bad) != 2 {
			t.Fatalf("%+v", bad)
		}
	})
	t.Run("defaults", func(t *testing.T) {
		p := Plan{Root: "/r", Entries: []PlanEntry{doc("ok.md", "engineering"), {Path: "logo.png", Kind: "object", Scope: "organization"}}}
		ok, bad := Validate(context.Background(), p, res, spaces, true)
		if len(bad) != 0 || len(ok) != 2 {
			t.Fatal(bad)
		}
		if ok[0].Title != "Heading of ok.md" || ok[0].Slug != "ok" || ok[0].Space != "engineering" {
			t.Errorf("%+v", ok[0])
		}
		if ok[1].Name != "logo.png" {
			t.Errorf("%+v", ok[1])
		}
	})
	t.Run("title falls back to the file stem", func(t *testing.T) {
		r2 := scan.Result{Files: []scan.File{{Root: "/r", Path: "x/notes.md", Kind: "document"}}}
		ok, _ := Validate(context.Background(), Plan{Root: "/r", Entries: []PlanEntry{doc("x/notes.md", "engineering")}}, r2, spaces, true)
		if len(ok) != 1 || ok[0].Title != "notes" {
			t.Fatalf("%+v", ok)
		}
	})
}
