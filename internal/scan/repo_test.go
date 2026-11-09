package scan

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// A file listed as outside says why when it may in fact be in a repository,
// so retire can tell no repository from one it must not trust.
func TestOutsideSaysWhy(t *testing.T) {
	isolate(t)
	plain := t.TempDir()
	write(t, plain, "a.md", "a")
	if g := file(t, scanOK(t, Options{}, plain), "a.md").Git; g.State != StateOutside || g.Unknown != "" {
		t.Errorf("no repository: %+v", g)
	}

	planted := newRepo(t)
	write(t, planted, "a.md", "a")
	commitAll(t, planted, "a")
	gitIn(t, planted, "config", "filter.x.clean", "/bin/false")
	if g := file(t, scanOK(t, Options{}, planted), "a.md").Git; g.State != StateOutside || g.Unknown != UnknownNamesPrograms {
		t.Errorf("planted config: %+v", g)
	}

	repo := newRepo(t)
	write(t, repo, "a.md", "a")
	commitAll(t, repo, "a")
	if g := file(t, scanOK(t, Options{Git: filepath.Join(t.TempDir(), "no-git-here")}, repo), "a.md").Git; g.State != StateOutside || g.Unknown != UnknownNoGit {
		t.Errorf("git missing: %+v", g)
	}

	broken := t.TempDir()
	write(t, broken, "top.md", "t")
	write(t, broken, "wt/.git", "gitdir: /nowhere\n")
	write(t, broken, "wt/b.md", "b")
	res := scanOK(t, Options{}, broken)
	// git cannot read its config either, which counts as naming programs.
	if g := file(t, res, "wt/b.md").Git; g.State != StateOutside || g.Unknown == "" {
		t.Errorf("a .git git cannot open: %+v", g)
	}
	if g := file(t, res, "top.md").Git; g.Unknown != "" {
		t.Errorf("beside it, no repository: %+v", g)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), "unknown") || strings.Contains(string(b), UnknownNamesPrograms) {
		t.Errorf("the reason is not in the scan's JSON: %s", b)
	}
}

func TestOpenRepo(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	repo := newRepo(t)
	write(t, repo, ".gitignore", "*.log\n")
	write(t, repo, "docs/a.md", "a")
	commitAll(t, repo, "a")

	r, distrusted := OpenRepo(ctx, filepath.Join(repo, "docs"), Options{})
	if r == nil || distrusted || r.Top != real(t, repo) {
		t.Fatalf("OpenRepo: %+v %v", r, distrusted)
	}
	for rel, want := range map[string]bool{"docs/x.log": true, "docs/x.zip": false, "docs/a.md": false} {
		got, err := r.Ignored(ctx, rel)
		if err != nil || got != want {
			t.Errorf("Ignored(%s) = %v %v, want %v", rel, got, err, want)
		}
	}
	// Pathspec magic in a name is read literally.
	for rel, want := range map[string]bool{":(glob)*.log": true, ":(glob)x.txt": false} {
		if got, err := r.Ignored(ctx, rel); err != nil || got != want {
			t.Errorf("a magic name is literal: Ignored(%s) = %v %v", rel, got, err)
		}
	}
	for rel, want := range map[string]bool{"docs/a.md": true, "docs/b.md": false, "docs/*": false} {
		got, err := r.Tracked(ctx, rel)
		if err != nil || got != want {
			t.Errorf("Tracked(%s) = %v %v, want %v", rel, got, err, want)
		}
	}
	p, err := r.GitPath(ctx, "info/exclude")
	if err != nil || p != filepath.Join(real(t, repo), ".git", "info", "exclude") {
		t.Errorf("GitPath: %q %v", p, err)
	}

	gitIn(t, repo, "config", "core.fsmonitor", "/bin/false")
	if r, distrusted := OpenRepo(ctx, repo, Options{}); r != nil || !distrusted {
		t.Errorf("a repository naming programs is not opened: %+v %v", r, distrusted)
	}

	plain := t.TempDir()
	if r, distrusted := OpenRepo(ctx, plain, Options{}); r != nil || distrusted {
		t.Errorf("no repository: %+v %v", r, distrusted)
	}
	if r, _ := OpenRepo(ctx, repo, Options{Git: filepath.Join(t.TempDir(), "none")}); r != nil {
		t.Errorf("without git there is no repository to open")
	}
}
