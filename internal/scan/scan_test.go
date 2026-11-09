package scan

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// isolate points HOME at an empty directory so no global git config or
// credential helper is read, and returns that home.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return home
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	return dir
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", msg)
}

func real(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func scanOK(t *testing.T, opts Options, roots ...string) Result {
	t.Helper()
	res, err := Run(context.Background(), roots, opts)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return res
}

func file(t *testing.T, res Result, path string) File {
	t.Helper()
	for _, f := range res.Files {
		if f.Path == path {
			return f
		}
	}
	var have []string
	for _, f := range res.Files {
		have = append(have, f.Path)
	}
	t.Fatalf("%s not in the scan; have %v", path, have)
	return File{}
}

func paths(res Result) []string {
	var out []string
	for _, f := range res.Files {
		out = append(out, f.Path)
	}
	return out
}

func hasFlag(f File, flag string) bool { return slices.Contains(f.Flags, flag) }

func TestRefusesHomeAndAbove(t *testing.T) {
	isolate(t)
	base := real(t, t.TempDir())
	home := filepath.Join(base, "users", "pat")
	inside := filepath.Join(home, "notes")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	// The same home reached through a symbolic link: refusal compares real paths.
	linked := filepath.Join(t.TempDir(), "homelink")
	if err := os.Symlink(home, linked); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(t.TempDir(), "rootlink")
	if err := os.Symlink(inside, rootLink); err != nil {
		t.Fatal(err)
	}
	refused := []struct {
		name, root, home string
	}{
		{"home itself", home, home},
		{"the folder above home", filepath.Join(base, "users"), home},
		{"far above home", base, home},
		{"the whole disk", "/", home},
		{"home reached through a link", filepath.Join(base, "users"), linked},
		{"home given with a trailing slash", home + "/", home},
		{"a root that is a symbolic link", rootLink, home},
	}
	for _, c := range refused {
		_, err := Run(context.Background(), []string{c.root}, Options{Home: c.home})
		var re *RefusedError
		if !errors.As(err, &re) {
			t.Errorf("%s: want a refusal, got %v", c.name, err)
			continue
		}
		msg := re.Error()
		if !strings.HasSuffix(msg, ".") || strings.Contains(strings.TrimSuffix(msg, "."), ". ") || strings.ContainsAny(msg, "—–") {
			t.Errorf("%s: want one sentence, got %q", c.name, msg)
		}
	}
	write(t, inside, "a.md", "# A\n")
	res := scanOK(t, Options{Home: home}, inside)
	if got := paths(res); !slices.Equal(got, []string{"a.md"}) {
		t.Fatalf("a folder inside home is allowed: %v", got)
	}
}

func TestRootErrors(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	write(t, dir, "sub/a.md", "a")
	cases := map[string][]string{
		"missing":     {filepath.Join(dir, "nope")},
		"a file":      {filepath.Join(dir, "sub", "a.md")},
		"overlapping": {dir, filepath.Join(dir, "sub")},
		"none":        {},
	}
	for name, roots := range cases {
		_, err := Run(context.Background(), roots, Options{})
		var ue *UsageError
		if !errors.As(err, &ue) {
			t.Errorf("%s: want a usage error, got %v", name, err)
		}
	}
}

func TestNeverFollowsSymlinks(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	outside := t.TempDir()
	write(t, root, "a.md", "# A\n")
	write(t, outside, "secret.md", "# Outside\n")
	if err := os.Symlink(filepath.Join(root, "a.md"), filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dirlink")); err != nil {
		t.Fatal(err)
	}
	res := scanOK(t, Options{}, root)
	if got := paths(res); !slices.Equal(got, []string{"a.md"}) {
		t.Fatalf("files %v, want only a.md", got)
	}
	skipped := map[string]string{}
	for _, s := range res.Skipped {
		skipped[s.Path] = s.Reason
	}
	for _, p := range []string{"link.md", "dirlink"} {
		if skipped[p] != "symlink" {
			t.Errorf("%s: skipped as %q, want symlink (all: %v)", p, skipped[p], skipped)
		}
	}
}

func TestSkipList(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	write(t, root, "docs/keep.md", "k")
	write(t, root, ".env", "X=1")
	for _, d := range []string{".hidden", "node_modules", "vendor", "dist", "build", "target", "__pycache__", "venv", ".venv", "docs/.vitepress", "docs/node_modules"} {
		write(t, root, d+"/inner.md", "x")
	}
	res := scanOK(t, Options{}, root)
	if got := paths(res); !slices.Equal(got, []string{".env", "docs/keep.md"}) {
		t.Fatalf("files %v", got)
	}
	reasons := map[string]string{}
	for _, s := range res.Skipped {
		reasons[s.Path] = s.Reason
	}
	if reasons["node_modules"] != "skip_list" || reasons["docs/.vitepress"] != "skip_list" {
		t.Fatalf("skipped folders not reported: %v", reasons)
	}
	if res.Truncated {
		t.Fatal("not truncated")
	}
}

func TestTruncatesAtTheLimit(t *testing.T) {
	isolate(t)
	if DefaultMaxFiles != 5000 {
		t.Fatalf("the limit is 5000 files, not %d", DefaultMaxFiles)
	}
	root := t.TempDir()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		write(t, root, n+".md", n)
	}
	res := scanOK(t, Options{MaxFiles: 3}, root)
	if len(res.Files) != 3 || !res.Truncated {
		t.Fatalf("files %v truncated %v", paths(res), res.Truncated)
	}
}

func TestKindByExtension(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	cases := map[string]struct{ body, kind string }{
		"a.md":       {"# a", "document"},
		"b.markdown": {"b", "document"},
		"c.txt":      {"c", "document"},
		"UP.MD":      {"u", "document"},
		"d.csv":      {"x,y\n1,2\n", "workbook"},
		"e.png":      {"\x89PNG\r\n\x1a\n\x00\x00", "object"},
		"f.pdf":      {"%PDF-1.4 text looking", "object"},
		"g.svg":      {"<svg/>", "object"},
		"h.bin":      {"ab\x00cd", "object"},
		"i.py":       {"print(1)\n", "other"},
		"j.yml":      {"a: 1\n", "other"},
		"noext":      {"plain", "other"},
	}
	for name, c := range cases {
		write(t, root, name, c.body)
	}
	res := scanOK(t, Options{}, root)
	for name, c := range cases {
		if got := file(t, res, name).Kind; got != c.kind {
			t.Errorf("%s: kind %q, want %q", name, got, c.kind)
		}
	}
}

func TestFileFacts(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	write(t, root, "front.md", "---\ntitle: \"From: the front\"\ntags: [a]\n---\n# Not this\n")
	write(t, root, "atx.md", "intro\n```\n# fenced\n```\n# The heading #\n## Two\n")
	write(t, root, "none.md", "no heading here\n####### Seven is not one\n")
	write(t, root, "latin1.md", "caf\xe9\n")
	write(t, root, "big.md", strings.Repeat("a", 512*1024+1))
	write(t, root, "edge.md", strings.Repeat("a", 512*1024))
	write(t, root, "README.rst", "r")
	write(t, root, "LICENCE", "l")
	write(t, root, "copying.txt", "c")
	write(t, root, "AGENTS.md", "a")
	write(t, root, ".cursorrules", "c")
	write(t, root, "notes.md", "n")
	write(t, root, "leak.md", "# Heading\nkey AKIAABCDEFGHIJKLMNOP\n[x](y.md)\n")
	write(t, root, "leak.png", "\x89PNG\x00\x00 AKIAABCDEFGHIJKLMNOP")
	write(t, root, "leak.py", "KEY = 'AKIAABCDEFGHIJKLMNOP'\n")
	write(t, root, "nul.md", "a\x00b AKIAABCDEFGHIJKLMNOP\n")
	res := scanOK(t, Options{}, root)

	if h := file(t, res, "front.md").Heading; h != "From: the front" {
		t.Errorf("front matter title: %q", h)
	}
	if h := file(t, res, "atx.md").Heading; h != "The heading" {
		t.Errorf("atx heading: %q", h)
	}
	if h := file(t, res, "none.md").Heading; h != "" {
		t.Errorf("no heading: %q", h)
	}
	f := file(t, res, "front.md")
	if f.Size != int64(len("---\ntitle: \"From: the front\"\ntags: [a]\n---\n# Not this\n")) || len(f.SHA256) != 64 {
		t.Errorf("size %d sha %q", f.Size, f.SHA256)
	}
	if got := file(t, res, "notes.md").SHA256; got != "1b16b1df538ba12dc3f97edbb85caa7050d46c148134290feba80f8236c83db9" {
		t.Errorf("sha256 of n: %s", got)
	}
	if !hasFlag(file(t, res, "latin1.md"), "not_utf8") || hasFlag(file(t, res, "notes.md"), "not_utf8") {
		t.Error("not_utf8")
	}
	if !hasFlag(file(t, res, "big.md"), "too_large") || hasFlag(file(t, res, "edge.md"), "too_large") {
		t.Error("too_large is over 512 KiB")
	}
	for _, k := range []string{"README.rst", "LICENCE", "copying.txt", "AGENTS.md", ".cursorrules"} {
		if !hasFlag(file(t, res, k), "keep") {
			t.Errorf("%s should be keep", k)
		}
	}
	if hasFlag(file(t, res, "notes.md"), "keep") {
		t.Error("notes.md is not keep")
	}
	leak := file(t, res, "leak.md")
	if !hasFlag(leak, "secret") || !hasFlag(file(t, res, "leak.py"), "secret") {
		t.Error("a text file with a secret is flagged")
	}
	if leak.Heading != "" || len(leak.Links) != 0 {
		t.Errorf("a secret file reports no heading or links: %q %v", leak.Heading, leak.Links)
	}
	if !hasFlag(file(t, res, "nul.md"), "secret") {
		t.Error("a document is text by its name, so a NUL byte does not stop the secret scan")
	}
	if hasFlag(file(t, res, "leak.png"), "secret") {
		t.Error("secret is for text files only")
	}
}

func TestSensitiveNames(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	flagged := []string{".env", ".env.local", "server.pem", "tls.KEY", "cert.p12", "cert.pfx", "id_rsa", "id_rsa.pub", "id_ed25519", "aws_credentials.json", "Credentials.md", "client_secret.json", "my-secrets.md", ".npmrc", ".pypirc", ".netrc", "kubeconfig", "kubeconfig.yaml"}
	plain := []string{"environment.md", "keys.md", "pem.md", "notes.md", "key.md.bak", "netrc.md"}
	for _, n := range append(append([]string{}, flagged...), plain...) {
		write(t, root, n, "x")
	}
	res := scanOK(t, Options{}, root)
	for _, n := range flagged {
		if !hasFlag(file(t, res, n), "sensitive_name") {
			t.Errorf("%s should be sensitive_name", n)
		}
	}
	for _, n := range plain {
		if hasFlag(file(t, res, n), "sensitive_name") {
			t.Errorf("%s should not be sensitive_name", n)
		}
	}
}

func TestUsedByDocsConfigAndCode(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	write(t, repo, "mkdocs.yml", "nav:\n  - Guide: guide.md\n")
	write(t, repo, "scripts/load.py", "open('docs/data.csv')\n")
	write(t, repo, "docs/guide.md", "# Guide\n")
	write(t, repo, "docs/data.csv", "a,b\n")
	write(t, repo, "docs/index.md", "See [orphan](orphan.md) and docs/sub/page.md.\n")
	write(t, repo, "docs/sub/page.md", "# Page\n")
	write(t, repo, "docs/orphan.md", "# Orphan\n")
	write(t, repo, "docs/.vitepress/config.js", "export default { page: 'docs/hidden.md' }\n")
	write(t, repo, "docs/hidden.md", "# Hidden\n")
	write(t, repo, "docs/ab", "short\n")
	write(t, repo, "notes.txt", "ab and docs/ab\n")
	commitAll(t, repo, "init")
	write(t, repo, "untracked.txt", "orphan.md docs/index.md\n")

	res := scanOK(t, Options{}, filepath.Join(repo, "docs"))
	for p, want := range map[string]bool{
		"guide.md":  true,  // named by mkdocs.yml, root relative
		"data.csv":  true,  // read by a script, repository relative
		"hidden.md": true,  // a tracked config in a skipped folder is outside the scanned set
		"orphan.md": false, // only linked from a scanned file, and from an untracked one
		"index.md":  false, // only named by an untracked file
		"ab":        true,  // the repository relative path docs/ab is long enough
	} {
		if got := hasFlag(file(t, res, p), "used"); got != want {
			t.Errorf("%s used = %v, want %v", p, got, want)
		}
	}

	// A root two folders down: docs/index.md is outside it, so it counts,
	// even though every tracked file under the root was scanned.
	res = scanOK(t, Options{}, filepath.Join(repo, "docs", "sub"))
	if !hasFlag(file(t, res, "page.md"), "used") {
		t.Error("page.md is named by docs/index.md, outside the root")
	}

	// A root at the repository top: the path ab is too short to search for.
	res = scanOK(t, Options{}, repo)
	if hasFlag(file(t, res, "docs/ab"), "used") {
		t.Error("docs/ab is named only by scanned files now")
	}

	// Outside a repository, never used.
	plain := t.TempDir()
	write(t, plain, "guide.md", "g")
	write(t, plain, "ref.txt", "guide.md")
	res = scanOK(t, Options{}, plain)
	if hasFlag(file(t, res, "guide.md"), "used") {
		t.Error("outside a repository nothing is used")
	}
}

func TestGitStates(t *testing.T) {
	isolate(t)
	upstream := t.TempDir()
	gitIn(t, upstream, "init", "-q", "--bare")
	repo := newRepo(t)
	write(t, repo, ".gitignore", "ign.md\nignored_dir/\n")
	write(t, repo, "clean.md", "c")
	write(t, repo, "sub/inner.md", "i")
	write(t, repo, "mod.md", "m")
	commitAll(t, repo, "first")
	gitIn(t, repo, "remote", "add", "origin", upstream)
	gitIn(t, repo, "push", "-q", "-u", "origin", "main")
	write(t, repo, "unpushed.md", "u")
	commitAll(t, repo, "second")
	write(t, repo, "mod.md", "changed")
	write(t, repo, "new.md", "n")
	write(t, repo, "ign.md", "i")
	write(t, repo, "staged.md", "s")
	write(t, repo, "ignored_dir/x.md", "x")
	write(t, repo, "new_dir/y.md", "y")
	write(t, repo, "sub/new.md", "n")
	gitIn(t, repo, "add", "staged.md")

	res := scanOK(t, Options{}, repo)
	tru, fal := true, false
	want := map[string]struct {
		state  string
		pushed *bool
	}{
		"clean.md":         {"clean", &tru},
		"mod.md":           {"modified", &tru},
		"unpushed.md":      {"clean", &fal},
		"new.md":           {"untracked", nil},
		"ign.md":           {"ignored", nil},
		"staged.md":        {"modified", nil},
		"ignored_dir/x.md": {"ignored", nil},
		"new_dir/y.md":     {"untracked", nil},
		"sub/inner.md":     {"clean", &tru},
	}
	top := real(t, repo)
	for p, w := range want {
		g := file(t, res, p).Git
		if g.State != w.state || g.Repo != top {
			t.Errorf("%s: state %q repo %q, want %q in %q", p, g.State, g.Repo, w.state, top)
		}
		if (g.Pushed == nil) != (w.pushed == nil) || (g.Pushed != nil && *g.Pushed != *w.pushed) {
			t.Errorf("%s: pushed %v, want %v", p, show(g.Pushed), show(w.pushed))
		}
	}

	// A root below the repository top is read under that folder only.
	res = scanOK(t, Options{}, filepath.Join(repo, "sub"))
	if g := file(t, res, "inner.md").Git; g.State != "clean" || g.Repo != top || g.Pushed == nil || !*g.Pushed {
		t.Errorf("sub/inner.md from a root below the top: %+v", g)
	}
	if g := file(t, res, "new.md").Git; g.State != "untracked" {
		t.Errorf("sub/new.md: %+v", g)
	}

	// Without an upstream, Pushed is null.
	solo := newRepo(t)
	write(t, solo, "a.md", "a")
	commitAll(t, solo, "a")
	res = scanOK(t, Options{}, solo)
	if g := file(t, res, "a.md").Git; g.State != "clean" || g.Pushed != nil {
		t.Errorf("no upstream: %+v", g)
	}

	// Outside any repository.
	plain := t.TempDir()
	write(t, plain, "a.md", "a")
	res = scanOK(t, Options{}, plain)
	if g := file(t, res, "a.md").Git; g.State != "outside" || g.Repo != "" || g.Pushed != nil {
		t.Errorf("outside: %+v", g)
	}
	if len(res.Notes) != 0 {
		t.Errorf("no notes expected: %v", res.Notes)
	}
}

func TestNestedRepositoryIsItsOwn(t *testing.T) {
	isolate(t)
	outer := newRepo(t)
	write(t, outer, "a.md", "a")
	commitAll(t, outer, "a")
	inner := filepath.Join(outer, "inner")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, inner, "init", "-q")
	write(t, inner, "b.md", "b")
	commitAll(t, inner, "b")
	write(t, outer, "wt/.git", "gitdir: /nowhere\n")
	res := scanOK(t, Options{}, outer)
	for _, f := range res.Files {
		if path.Base(f.Path) == ".git" {
			t.Fatalf("a .git file is listed: %s", f.Path)
		}
	}
	for _, s := range res.Skipped {
		if path.Base(s.Path) == ".git" {
			t.Fatalf("a .git entry is reported: %+v", s)
		}
	}
	if g := file(t, res, "inner/b.md").Git; g.Repo != real(t, inner) || g.State != "clean" {
		t.Fatalf("nested: %+v", g)
	}
	if g := file(t, res, "a.md").Git; g.Repo != real(t, outer) || g.State != "clean" {
		t.Fatalf("outer: %+v", g)
	}
}

func TestMissingGitMeansOutside(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	write(t, repo, "mkdocs.yml", "guide.md\n")
	write(t, repo, "docs/guide.md", "g")
	commitAll(t, repo, "init")
	res := scanOK(t, Options{Git: filepath.Join(t.TempDir(), "no-git-here")}, filepath.Join(repo, "docs"))
	f := file(t, res, "guide.md")
	if f.Git.State != "outside" || hasFlag(f, "used") {
		t.Fatalf("without git: %+v %v", f.Git, f.Flags)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "git") {
		t.Fatalf("one note about git: %v", res.Notes)
	}
}

func show(b *bool) string {
	if b == nil {
		return "null"
	}
	if *b {
		return "true"
	}
	return "false"
}

func TestLinksIgnoreCode(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	body := strings.Join([]string{
		"# Page",
		"See [the guide](../guide.md#setup) and ![logo](img/logo.png \"Logo\").",
		"Inline `[not a link](code.md)` and ``[nor `this`](x.md)`` stay code.",
		"```md",
		"[fenced](fenced.md)",
		"```",
		"~~~",
		"[tilde](tilde.md)",
		"~~~",
		"[web](https://example.com/a#b) [cdn](//cdn.example.com/x) [mail](mailto:a@b.example)",
		"[out](../../outside.md) [top](/top.md) [here](#local) [spaced](<my file.md>) [pct](my%20file.md)",
		"[ref][1] and \\[escaped](no.md)",
		"",
		"[1]: ref.md",
		"[![badge](badge.svg)](https://ci.example.com)",
		"[nested (parens)](a_(b).md)",
	}, "\n")
	write(t, root, "sub/page.md", body)
	res := scanOK(t, Options{}, root)
	got := file(t, res, "sub/page.md").Links
	want := []Link{
		{Text: "the guide", Target: "../guide.md#setup", Resolved: "guide.md", Anchor: "#setup"},
		{Text: "logo", Target: "img/logo.png", Image: true, Resolved: "sub/img/logo.png"},
		{Text: "web", Target: "https://example.com/a#b", External: true, Anchor: "#b"},
		{Text: "cdn", Target: "//cdn.example.com/x", External: true},
		{Text: "mail", Target: "mailto:a@b.example", External: true},
		{Text: "out", Target: "../../outside.md"},
		{Text: "top", Target: "/top.md", Resolved: "top.md"},
		{Text: "here", Target: "#local", Anchor: "#local"},
		{Text: "spaced", Target: "my file.md", Resolved: "sub/my file.md"},
		{Text: "pct", Target: "my%20file.md", Resolved: "sub/my file.md"},
		{Text: "![badge](badge.svg)", Target: "https://ci.example.com", External: true},
		{Text: "badge", Target: "badge.svg", Image: true, Resolved: "sub/badge.svg"},
		{Text: "nested (parens)", Target: "a_(b).md", Resolved: "sub/a_(b).md"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d links, want %d:\n%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("link %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func TestNoBodiesInOutput(t *testing.T) {
	// The command level test in internal/cli runs the JSON end to end; this
	// one checks every string field the scan fills.
	isolate(t)
	repo := newRepo(t)
	const marker = "BODYMARKER7f3a9c"
	write(t, repo, "a.md", "# Title\n\nParagraph "+marker+" here.\n[link](b.md)\n")
	write(t, repo, "b.csv", "h\n"+marker+"\n")
	write(t, repo, "c.txt", marker)
	write(t, repo, "d.py", "x = '"+marker+"'\n")
	write(t, repo, "e.md", "password = "+marker+"\n")
	commitAll(t, repo, "x")
	res := scanOK(t, Options{}, repo)
	for _, f := range res.Files {
		all := []string{f.Root, f.Path, f.SHA256, f.Heading, f.Kind, f.Git.State, f.Git.Repo}
		all = append(all, f.Flags...)
		for _, l := range f.Links {
			all = append(all, l.Text, l.Target, l.Resolved, l.Anchor)
		}
		for _, s := range all {
			if strings.Contains(s, marker) {
				t.Fatalf("%s: a body reached the output: %q", f.Path, s)
			}
		}
	}
}

// git grep -o prints only the leftmost match, so guide.md inside the printed
// old/guide.md is found by the containment step, not by git.
func TestUsedCountsPathsInsideLongerMatches(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	write(t, repo, "docs/guide.md", "g")
	write(t, repo, "docs/old/guide.md", "o")
	write(t, repo, "docs/other.md", "x")
	write(t, repo, "notes.txt", "see old/guide.md\n")
	commitAll(t, repo, "init")
	res := scanOK(t, Options{}, filepath.Join(repo, "docs"))
	for p, want := range map[string]bool{"guide.md": true, "old/guide.md": true, "other.md": false} {
		if got := hasFlag(file(t, res, p), "used"); got != want {
			t.Errorf("%s used = %v, want %v", p, got, want)
		}
	}
}
