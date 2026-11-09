package scan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

// swapCase flips the case of every ASCII letter, so a case insensitive disk
// (the macOS default) names the same folder by another string.
func swapCase(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'a' && c <= 'z':
			b[i] = c - 32
		case c >= 'A' && c <= 'Z':
			b[i] = c + 32
		}
	}
	return string(b)
}

func wantRefused(t *testing.T, name, root, home string) {
	t.Helper()
	_, err := Run(context.Background(), []string{root}, Options{Home: home})
	var re *RefusedError
	if !errors.As(err, &re) {
		t.Errorf("%s (%s): want a refusal, got %v", name, root, err)
		return
	}
	if msg := re.Error(); !strings.HasSuffix(msg, ".") || strings.Contains(strings.TrimSuffix(msg, "."), ". ") {
		t.Errorf("%s: want one sentence, got %q", name, msg)
	}
}

// The home folder named by another string for the same folder: another
// case on a case insensitive disk, or macOS's Data volume path. Refusal
// compares the folders themselves, not their names.
func TestRefusesHomeByIdentity(t *testing.T) {
	isolate(t)
	base := real(t, t.TempDir())
	home := filepath.Join(base, "users", "pat")
	if err := os.MkdirAll(filepath.Join(home, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	variants := map[string]string{
		"home in another case":           swapCase(home),
		"the folder above in other case": swapCase(filepath.Dir(home)),
		"home on the Data volume":        "/System/Volumes/Data" + home,
		"above home on the Data volume":  "/System/Volumes/Data" + filepath.Dir(home),
	}
	tried := 0
	for name, root := range variants {
		if _, err := os.Stat(root); err != nil {
			continue // a case sensitive disk, or not macOS
		}
		tried++
		wantRefused(t, name, root, home)
	}
	if tried == 0 {
		t.Skip("this disk names a folder one way only")
	}
	// A folder inside home is still allowed by any name.
	inside := swapCase(filepath.Join(home, "notes"))
	if _, err := os.Stat(inside); err == nil {
		if _, err := Run(context.Background(), []string{inside}, Options{Home: home}); err != nil {
			t.Errorf("a folder inside home by another case: %v", err)
		}
	}
}

func TestRefusesDotFolders(t *testing.T) {
	isolate(t)
	home := real(t, t.TempDir())
	other := real(t, t.TempDir())
	for _, d := range []string{
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".aws"),
		filepath.Join(home, ".config", "x"),
		filepath.Join(other, "proj", ".github"),
		filepath.Join(other, ".hidden", "docs"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		wantRefused(t, "a dot folder", d, home)
	}
	ok := filepath.Join(other, "v1.2", "docs")
	if err := os.MkdirAll(ok, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), []string{ok}, Options{Home: home}); err != nil {
		t.Fatalf("a dot inside a name is not a dot folder: %v", err)
	}
}

// plant makes a repository whose own config would run a program, writing
// a marker file, if git honoured it during a scan.
func TestPlantedGitConfigRunsNothing(t *testing.T) {
	isolate(t)
	script := func(t *testing.T, marker string) string {
		p := filepath.Join(t.TempDir(), "run.sh")
		if err := os.WriteFile(p, []byte("#!/bin/sh\ntouch '"+marker+"'\ncat\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct {
		name  string
		plant func(t *testing.T, repo, prog string)
	}{
		{"clean filter", func(t *testing.T, repo, prog string) {
			gitIn(t, repo, "config", "filter.x.clean", prog)
		}},
		{"process filter", func(t *testing.T, repo, prog string) {
			gitIn(t, repo, "config", "filter.x.process", prog)
		}},
		{"smudge filter", func(t *testing.T, repo, prog string) {
			gitIn(t, repo, "config", "filter.x.smudge", prog)
		}},
		{"fsmonitor", func(t *testing.T, repo, prog string) {
			gitIn(t, repo, "config", "core.fsmonitor", prog)
		}},
		{"signature program", func(t *testing.T, repo, prog string) {
			gitIn(t, repo, "config", "log.showSignature", "true")
			gitIn(t, repo, "config", "gpg.program", prog)
		}},
		{"ssh signature program", func(t *testing.T, repo, prog string) {
			gitIn(t, repo, "config", "gpg.ssh.program", prog)
		}},
		{"hooks path", func(t *testing.T, repo, prog string) {
			gitIn(t, repo, "config", "core.hooksPath", filepath.Dir(prog))
		}},
		{"textconv", func(t *testing.T, repo, prog string) {
			gitIn(t, repo, "config", "diff.x.textconv", prog)
		}},
		{"diff command", func(t *testing.T, repo, prog string) {
			gitIn(t, repo, "config", "diff.x.command", prog)
		}},
		{"included config", func(t *testing.T, repo, prog string) {
			inc := filepath.Join(t.TempDir(), "inc")
			if err := os.WriteFile(inc, []byte("[filter \"x\"]\n\tclean = "+prog+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitIn(t, repo, "config", "include.path", inc)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "RAN")
			prog := script(t, marker)
			upstream := t.TempDir()
			gitIn(t, upstream, "init", "-q", "--bare")
			repo := newRepo(t)
			write(t, repo, ".gitattributes", "*.md filter=x diff=x\n")
			write(t, repo, "a.md", "one\n")
			commitAll(t, repo, "first")
			gitIn(t, repo, "remote", "add", "origin", upstream)
			gitIn(t, repo, "push", "-q", "-u", "origin", "main")
			signedCommit(t, repo)
			c.plant(t, repo, prog)
			// Changed content makes status run the clean filter.
			write(t, repo, "a.md", "two\n")

			res := scanOK(t, Options{}, repo)
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("a program named by the repository's config ran")
			}
			if g := file(t, res, "a.md").Git; g.State != StateOutside || g.Repo != "" {
				t.Errorf("a repository that names programs is outside: %+v", g)
			}
			top := real(t, repo)
			n := 0
			for _, note := range res.Notes {
				if strings.Contains(note, top) {
					n++
				}
			}
			if n != 1 {
				t.Errorf("want one note naming %s, got %v", top, res.Notes)
			}
		})
	}
}

// signedCommit adds a commit carrying a (fake) signature on top of HEAD, so
// log.showSignature would hand it to gpg.program.
func signedCommit(t *testing.T, repo string) {
	t.Helper()
	write(t, repo, "b.md", "b\n")
	gitIn(t, repo, "add", "b.md")
	tree := strings.TrimSpace(gitIn(t, repo, "write-tree"))
	parent := strings.TrimSpace(gitIn(t, repo, "rev-parse", "HEAD"))
	body := "tree " + tree + "\nparent " + parent +
		"\nauthor t <t@example.invalid> 1700000000 +0000\ncommitter t <t@example.invalid> 1700000000 +0000\n" +
		"gpgsig -----BEGIN PGP SIGNATURE-----\n \n abc\n -----END PGP SIGNATURE-----\n\nsigned\n"
	obj := filepath.Join(t.TempDir(), "commit")
	if err := os.WriteFile(obj, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c := strings.TrimSpace(gitIn(t, repo, "hash-object", "-t", "commit", "-w", obj))
	gitIn(t, repo, "update-ref", "refs/heads/main", c)
}

// A repository whose config names no program is read as before, and the
// user's own global config (git-lfs sets filters there) does not count.
func TestGlobalFilterConfigIsTheUsers(t *testing.T) {
	home := isolate(t)
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[filter \"lfs\"]\n\tclean = git-lfs clean -- %f\n[gpg]\n\tprogram = gpg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := newRepo(t)
	write(t, repo, "a.md", "a")
	commitAll(t, repo, "a")
	res := scanOK(t, Options{}, repo)
	if g := file(t, res, "a.md").Git; g.State != StateClean {
		t.Fatalf("global config is the user's own: %+v %v", g, res.Notes)
	}
}

// Every loop of the link parser is counted; the count stays within a fixed
// multiple of the input's size on inputs built to make a naive parser
// quadratic. No time is measured.
func TestLinkWorkIsLinear(t *testing.T) {
	inputs := map[string]func(n int) string{
		"empty fences":    func(n int) string { return strings.Repeat("```\n```\n", n/8) },
		"open brackets":   func(n int) string { return strings.Repeat("[", n) },
		"open images":     func(n int) string { return strings.Repeat("![", n/2) },
		"backtick runs":   func(n int) string { return strings.Repeat("a `` b ` ", n/9) },
		"unclosed parens": func(n int) string { return strings.Repeat("[](", n/3) },
		"unclosed titles": func(n int) string { return strings.Repeat("[](a (", n/6) },
		"titles sharing a closer": func(n int) string {
			return strings.Repeat("[](a (", n/12) + ")" + strings.Repeat(" ", n/2) + "x"
		},
		"destinations sharing spaces": func(n int) string {
			return strings.Repeat("[](a", n/8) + strings.Repeat(" ", n/2) + "(x"
		},
		"long destinations": func(n int) string { return strings.Repeat("[]("+strings.Repeat("a", 97), n/100) },
		"nested links":      func(n int) string { return strings.Repeat("[", n/8) + strings.Repeat("x](a) ", n/8) },
		"links in a fence":  func(n int) string { return "```\n" + strings.Repeat("[a](b) ", n/7) },
		"ordinary":          func(n int) string { return strings.Repeat("See [the guide](guide.md) and `code`.\n", n/38) },
	}
	for name, gen := range inputs {
		for _, n := range []int{20_000, 80_000} {
			body := gen(n)
			_, work := parseLinksCounting(body, "page.md")
			if limit := 64*len(body) + 64; work > limit {
				t.Errorf("%s at %d bytes: %d steps, over %d", name, len(body), work, limit)
			}
		}
	}
}

func TestHeadingLevels(t *testing.T) {
	cases := map[string]string{
		"## Two\n# One\n":        "Two",
		"###### Six\n":           "Six",
		"####### Seven\n# One\n": "One",
		"text\n### Three ###\n":  "Three",
		"#NoSpace\n":             "",
	}
	for in, want := range cases {
		if got := headingOf([]byte(in), false); got != want {
			t.Errorf("headingOf(%q) = %q, want %q", in, got, want)
		}
	}
}

// Headings and link texts are untrusted text bound for an agent's context,
// so each is at most 200 characters, ending in an ellipsis when cut.
func TestCapsUntrustedText(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	long := strings.Repeat("é", 300)
	exact := strings.Repeat("a", 200)
	write(t, root, "long.md", "# "+long+"\n["+long+"](x.md) ["+exact+"](y.md)\n")
	write(t, root, "exact.md", "# "+exact+"\n")
	res := scanOK(t, Options{}, root)
	f := file(t, res, "long.md")
	for _, s := range []string{f.Heading, f.Links[0].Text} {
		if utf8.RuneCountInString(s) != 200 || !strings.HasSuffix(s, "…") || !strings.HasPrefix(s, "éé") {
			t.Errorf("not capped at 200 with an ellipsis: %d runes, %q", utf8.RuneCountInString(s), s[len(s)-6:])
		}
	}
	if f.Links[1].Text != exact || file(t, res, "exact.md").Heading != exact {
		t.Error("200 characters exactly are left whole")
	}
}

// A FIFO is skipped as not regular, and the scan never opens it, so it does
// not block. The wait is a hang limit only.
func TestFIFOIsSkippedNotRegular(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	write(t, root, "a.md", "a")
	if err := syscall.Mkfifo(filepath.Join(root, "pipe.md"), 0o644); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	done := make(chan Result, 1)
	go func() {
		res, err := Run(context.Background(), []string{root}, Options{})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	var res Result
	select {
	case res = <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("the scan blocked on a FIFO")
	}
	found := false
	for _, s := range res.Skipped {
		if s.Path == "pipe.md" && s.Reason == SkipNotRegular {
			found = true
		}
	}
	if !found || len(res.Files) != 1 {
		t.Fatalf("FIFO not skipped as not_regular: %+v %v", res.Skipped, paths(res))
	}
}

func TestCopilotInstructionsKeepRule(t *testing.T) {
	if !keepFile("/w/repo/.github/copilot-instructions.md") || keepFile("/w/repo/docs/copilot-instructions.md") {
		t.Fatal(".github/copilot-instructions.md is keep, elsewhere it is not")
	}
}

// A volume root holding the home folder under another name, as
// /System/Volumes/Data holds /Users on macOS, is refused, and so is a
// folder above it. The file limit bounds the old behaviour, which walked it.
func TestRefusesVolumeHoldingHome(t *testing.T) {
	isolate(t)
	home := real(t, t.TempDir())
	tried := 0
	for _, root := range []string{"/System/Volumes/Data", "/System/Volumes"} {
		if _, err := os.Stat(filepath.Join("/System/Volumes/Data", home)); err != nil {
			continue
		}
		tried++
		_, err := Run(context.Background(), []string{root}, Options{Home: home, MaxFiles: 1})
		var re *RefusedError
		if !errors.As(err, &re) {
			t.Errorf("%s holds the home folder under another name: want a refusal, got %v", root, err)
		}
	}
	if tried == 0 {
		t.Skip("no Data volume alias here")
	}
}

// The walk itself refuses on reaching a folder that is the home folder, by
// identity, whatever the root checks said. Here the root checks are
// bypassed by calling walk directly.
func TestWalkRefusesTheHomeFolder(t *testing.T) {
	isolate(t)
	root := real(t, t.TempDir())
	home := filepath.Join(root, "b", "home")
	write(t, root, "a.md", "a")
	write(t, home, "secret.md", "s")
	write(t, root, "c/after.md", "c")
	g, err := newHomeGuard(home)
	if err != nil {
		t.Fatal(err)
	}
	var files []walked
	var skipped []Skipped
	var leftovers []Leftover
	_, err = walk(context.Background(), root, 100, &files, &skipped, &leftovers, g)
	var re *RefusedError
	if !errors.As(err, &re) {
		t.Fatalf("want a refusal, got %v", err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.rel, "b/home/") {
			t.Fatalf("a file under the home folder was listed: %s", f.rel)
		}
	}
}

// HOME inside the root: refused before any file is read.
func TestRefusesWhenHomeIsInsideRoot(t *testing.T) {
	isolate(t)
	root := real(t, t.TempDir())
	home := filepath.Join(root, "people", "pat")
	write(t, home, "notes.md", "n")
	wantRefused(t, "home inside the root", root, home)
}

// The parent repository is clean, but a submodule's own config names a
// filter. The parent's status must not recurse into it, and no git command
// runs in the submodule.
func TestSubmoduleConfigRunsNothing(t *testing.T) {
	isolate(t)
	marker := filepath.Join(t.TempDir(), "RAN")
	prog := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\ntouch '"+marker+"'\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := newRepo(t)
	write(t, sub, ".gitattributes", "*.md filter=x\n")
	write(t, sub, "a.md", "one\n")
	commitAll(t, sub, "sub")
	parent := newRepo(t)
	write(t, parent, "docs/index.md", "# Index\n")
	commitAll(t, parent, "parent")
	gitIn(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "docs/sub")
	gitIn(t, parent, "commit", "-q", "-m", "add sub")
	inner := filepath.Join(parent, "docs", "sub")
	gitIn(t, inner, "config", "filter.x.clean", prog)
	write(t, inner, "a.md", "two\n")

	res := scanOK(t, Options{}, parent)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the submodule's filter ran")
	}
	if g := file(t, res, "docs/index.md").Git; g.State != StateClean {
		t.Errorf("the parent is read as usual: %+v %v", g, res.Notes)
	}
	if g := file(t, res, "docs/sub/a.md").Git; g.State != StateOutside {
		t.Errorf("the submodule names a program, so its files are outside: %+v", g)
	}
}
