package retire

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/smrit-ai/cli/internal/plan"
	"github.com/smrit-ai/cli/internal/scan"
	"github.com/smrit-ai/cli/internal/state"
)

// isolate points HOME at an empty folder, so no global git config is read
// and no real state directory is touched.
func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
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
	dir := realPath(t, t.TempDir())
	gitIn(t, dir, "init", "-q")
	return dir
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", msg)
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
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

func sum(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

var today = time.Date(2026, 11, 9, 15, 0, 0, 0, time.Local)

const zipName = "smrit-import-2026-11-09.zip"

// fixture is a finished import run over root, saved in its own state
// directory.
type fixture struct {
	t      *testing.T
	dir    string
	root   string
	run    *state.Run
	n      int
	zips   ZipStore
	rename Renamer
	link   func(root *os.Root, oldname, newname string) error
	git    string
}

func newFixture(t *testing.T, root string) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: t.TempDir(), root: root}
	f.run = &state.Run{ID: "20261109T143005Z-a1b2c3", Root: root, Org: state.Org{ID: "org", Name: "Org", Slug: "org"},
		Plan: plan.Plan{Version: 1, Root: root}, Entries: map[string]state.EntryState{}, Phase: state.PhaseFinished}
	return f
}

func kindOf(rel string) string {
	switch path.Ext(rel) {
	case ".md":
		return "document"
	case ".csv":
		return "workbook"
	}
	return "object"
}

// imported records rel as imported with the hash of body, with status and
// id, without touching the file.
func (f *fixture) imported(rel, body, status, id string) {
	f.run.Plan.Entries = append(f.run.Plan.Entries, plan.PlanEntry{Path: rel, Kind: kindOf(rel)})
	f.run.Entries[rel] = state.EntryState{Kind: kindOf(rel), SourceSHA256: sum(body), Status: status, ID: id}
}

// done writes body to rel and records it as created in Smrit.
func (f *fixture) done(rel, body string) {
	f.t.Helper()
	write(f.t, f.root, rel, body)
	f.n++
	f.imported(rel, body, state.StatusCreated, "Doc"+strings.Repeat("A", 17)+string(rune('a'+f.n%26)))
}

func (f *fixture) retire(apply bool) (Report, error) {
	f.t.Helper()
	if err := state.SaveRun(f.dir, f.run); err != nil {
		f.t.Fatal(err)
	}
	return Run(context.Background(), f.run.ID, Options{StateDir: f.dir, Apply: apply, Now: func() time.Time { return today }, Zips: f.zips, Rename: f.rename, Link: f.link, Scan: scan.Options{Git: f.git}})
}

func (f *fixture) mustRetire(apply bool) Report {
	f.t.Helper()
	rep, err := f.retire(apply)
	if err != nil {
		f.t.Fatalf("retire: %v (%+v)", err, rep)
	}
	return rep
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Lstat(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

func content(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// zipMembers reads the zip and returns its members' bodies by name.
func zipMembers(t *testing.T, p string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(p)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	out := map[string]string{}
	for _, m := range zr.File {
		rc, err := m.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[m.Name] = string(b)
	}
	return out
}

func zipsIn(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "smrit-import-*.zip"))
	return m
}

func leftReason(rep Report, p string) string {
	for _, l := range rep.Left {
		if l.Path == p {
			return l.Reason
		}
	}
	return ""
}

func sorted(s []string) []string {
	s = slices.Clone(s)
	sort.Strings(s)
	return s
}

func statusOf(t *testing.T, repo string) string {
	t.Helper()
	return gitIn(t, repo, "status", "--porcelain=v1", "--untracked-files=all")
}

// Row one: tracked and identical to HEAD is deleted from the working tree,
// not staged and not committed, and never zipped.
func TestCleanTrackedIsDeletedNotStaged(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	f := newFixture(t, repo)
	f.done("docs/a.md", "# A\n")
	commitAll(t, repo, "a")
	head := gitIn(t, repo, "rev-parse", "HEAD")

	rep := f.mustRetire(true)
	if !slices.Equal(rep.Deleted, []string{"docs/a.md"}) || len(rep.Zipped) != 0 || rep.Zip != nil || !rep.Applied {
		t.Fatalf("report: %+v", rep)
	}
	if exists(t, filepath.Join(repo, "docs/a.md")) {
		t.Fatal("the clean file is still there")
	}
	if !exists(t, filepath.Join(repo, "docs")) {
		t.Error("a folder left empty stays")
	}
	if s := statusOf(t, repo); s != " D docs/a.md\n" {
		t.Errorf("only an unstaged deletion: %q", s)
	}
	if staged := gitIn(t, repo, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("nothing staged: %q", staged)
	}
	if h := gitIn(t, repo, "rev-parse", "HEAD"); h != head {
		t.Error("a commit was made")
	}
	if z := zipsIn(t, repo); len(z) != 0 {
		t.Errorf("no zip for files git holds: %v", z)
	}
}

// Row two: tracked with uncommitted changes goes into the zip, then is
// removed.
func TestModifiedIsZippedThenRemoved(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	write(t, repo, "a.md", "committed\n")
	commitAll(t, repo, "a")
	f := newFixture(t, repo)
	f.done("a.md", "edited, then imported\n")

	rep := f.mustRetire(true)
	zp := filepath.Join(repo, zipName)
	if rep.Zip == nil || *rep.Zip != zp || !slices.Equal(rep.Zipped, []string{"a.md"}) || len(rep.Deleted) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if exists(t, filepath.Join(repo, "a.md")) {
		t.Fatal("the modified file is still there")
	}
	if m := zipMembers(t, zp); len(m) != 1 || m["a.md"] != "edited, then imported\n" {
		t.Errorf("zip holds the edited bytes: %v", m)
	}
}

// Row three: ignored, untracked, or outside a repository go into the zip,
// then are removed. The zip is private, its members root relative.
func TestIgnoredUntrackedAndOutsideAreZipped(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	write(t, repo, ".gitignore", "ign.md\n")
	commitAll(t, repo, "ignore")
	f := newFixture(t, repo)
	f.done("new.md", "untracked\n")
	f.done("ign.md", "ignored\n")
	f.done("deep/sub/c.md", "deep\n")

	rep := f.mustRetire(true)
	want := []string{"deep/sub/c.md", "ign.md", "new.md"}
	if !slices.Equal(sorted(rep.Zipped), want) || len(rep.Left) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	zp := filepath.Join(repo, zipName)
	m := zipMembers(t, zp)
	if len(m) != 3 || m["new.md"] != "untracked\n" || m["ign.md"] != "ignored\n" || m["deep/sub/c.md"] != "deep\n" {
		t.Errorf("zip: %v", m)
	}
	st, err := os.Stat(zp)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("zip mode: %v %v", st, err)
	}
	for _, p := range want {
		if exists(t, filepath.Join(repo, p)) {
			t.Errorf("%s is still there", p)
		}
	}
	if !exists(t, filepath.Join(repo, "deep/sub")) {
		t.Error("a folder left empty stays")
	}

	plain := realPath(t, t.TempDir())
	g := newFixture(t, plain)
	g.done("x.md", "outside\n")
	g.done("pic.png", "\x89PNG\x00\x01")
	rep = g.mustRetire(true)
	if !slices.Equal(sorted(rep.Zipped), []string{"pic.png", "x.md"}) || rep.ExcludeAdded {
		t.Fatalf("outside: %+v", rep)
	}
	if m := zipMembers(t, filepath.Join(plain, zipName)); m["x.md"] != "outside\n" || m["pic.png"] != "\x89PNG\x00\x01" {
		t.Errorf("outside zip: %v", m)
	}
}

// Row four: failed, skipped, refused or outside the plan stays, and is
// never in the zip.
func TestNotDoneIsLeftAndNeverZipped(t *testing.T) {
	isolate(t)
	root := realPath(t, t.TempDir())
	f := newFixture(t, root)
	f.done("ok.md", "ok\n")
	cases := map[string]string{
		"failed.md":  state.StatusFailed,
		"skipped.md": state.StatusSkipped,
		"refused.md": state.StatusRefused,
	}
	for p, status := range cases {
		write(t, root, p, p)
		f.imported(p, p, status, "")
	}
	write(t, root, "noid.md", "noid")
	f.imported("noid.md", "noid", state.StatusCreated, "")
	// A workbook Smrit already had counts as done, but holds no id.
	write(t, root, "tasks.csv", "a,b\n")
	f.imported("tasks.csv", "a,b\n", state.StatusExists, "")
	// In the plan with no record in the run.
	write(t, root, "norecord.md", "n")
	f.run.Plan.Entries = append(f.run.Plan.Entries, plan.PlanEntry{Path: "norecord.md", Kind: "document"})
	// Not in the plan at all.
	write(t, root, "unplanned.md", "u")

	rep := f.mustRetire(true)
	if !slices.Equal(rep.Zipped, []string{"ok.md"}) {
		t.Fatalf("report: %+v", rep)
	}
	for _, p := range []string{"failed.md", "skipped.md", "refused.md", "noid.md", "tasks.csv", "norecord.md"} {
		if leftReason(rep, p) == "" {
			t.Errorf("%s is left with a reason: %+v", p, rep.Left)
		}
	}
	if leftReason(rep, "unplanned.md") != "" {
		t.Errorf("a file outside the plan is not reported: %+v", rep.Left)
	}
	for _, p := range []string{"failed.md", "skipped.md", "refused.md", "noid.md", "tasks.csv", "norecord.md", "unplanned.md"} {
		if !exists(t, filepath.Join(root, p)) {
			t.Errorf("%s was removed", p)
		}
	}
	if m := zipMembers(t, filepath.Join(root, zipName)); len(m) != 1 {
		t.Errorf("only the done file is zipped: %v", m)
	}
}

// Row five, and the used flag: README, licences, instruction files and
// anything the repository names stay, even when the plan named them.
func TestKeepAndUsedAreLeft(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	root := filepath.Join(repo, "docs")
	f := newFixture(t, root)
	for _, p := range []string{"README.md", "LICENSE", "CLAUDE.md", "AGENTS.md", "guide.md", "other.md"} {
		f.done(p, "# "+p+"\n")
	}
	// A tracked file outside the scanned folder names guide.md.
	write(t, repo, "mkdocs.yml", "nav:\n  - docs/guide.md\n")
	commitAll(t, repo, "all")

	rep := f.mustRetire(true)
	if !slices.Equal(rep.Deleted, []string{"other.md"}) || len(rep.Zipped) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	for _, p := range []string{"README.md", "LICENSE", "CLAUDE.md", "AGENTS.md"} {
		if r := leftReason(rep, p); !strings.Contains(r, "kept") {
			t.Errorf("%s: reason %q", p, r)
		}
		if !exists(t, filepath.Join(root, p)) {
			t.Errorf("%s was removed", p)
		}
	}
	if r := leftReason(rep, "guide.md"); !strings.Contains(r, "names it") {
		t.Errorf("used: reason %q", r)
	}
	if !exists(t, filepath.Join(root, "guide.md")) {
		t.Error("a used file was removed")
	}
}

// Retired from the repository top, a page the site config names is kept:
// the config is in the scanned folder too, and is still read for names.
func TestUsedAtRepositoryTopIsLeft(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	f := newFixture(t, repo)
	f.done("docs/guide.md", "# Guide\n")
	f.done("docs/other.md", "# Other\n")
	write(t, repo, "mkdocs.yml", "nav:\n  - docs/guide.md\n")
	commitAll(t, repo, "all")

	rep := f.mustRetire(true)
	if !slices.Equal(rep.Deleted, []string{"docs/other.md"}) || len(rep.Zipped) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if r := leftReason(rep, "docs/guide.md"); !strings.Contains(r, "names it") {
		t.Errorf("used: reason %q", r)
	}
	if !exists(t, filepath.Join(repo, "docs/guide.md")) {
		t.Fatal("a page the site config names was removed")
	}
}

func TestEditedSinceImportIsLeft(t *testing.T) {
	isolate(t)
	root := realPath(t, t.TempDir())
	f := newFixture(t, root)
	f.done("a.md", "as imported\n")
	write(t, root, "a.md", "edited afterwards\n")

	rep := f.mustRetire(true)
	if r := leftReason(rep, "a.md"); !strings.Contains(r, "changed") || len(rep.Zipped) != 0 || rep.Zip != nil {
		t.Fatalf("report: %+v", rep)
	}
	if content(t, filepath.Join(root, "a.md")) != "edited afterwards\n" {
		t.Error("the edited file was touched")
	}
	if z := zipsIn(t, root); len(z) != 0 {
		t.Errorf("no zip: %v", z)
	}
}

// A repository whose config names programs is not outside every
// repository: its files stay, unzipped, and none of its programs runs.
func TestPlantedConfigRepoIsLeftUntouched(t *testing.T) {
	isolate(t)
	marker := filepath.Join(t.TempDir(), "RAN")
	prog := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\ntouch '"+marker+"'\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := newRepo(t)
	write(t, repo, ".gitattributes", "*.md filter=x\n")
	f := newFixture(t, repo)
	f.done("clean.md", "clean\n")
	write(t, repo, "mod.md", "old\n")
	commitAll(t, repo, "first")
	f.done("mod.md", "new\n")
	f.done("new.md", "untracked\n")
	gitIn(t, repo, "config", "filter.x.clean", prog)
	exclude := filepath.Join(repo, ".git", "info", "exclude")
	before, _ := os.ReadFile(exclude)

	rep := f.mustRetire(true)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a program named by the repository's config ran")
	}
	if len(rep.Deleted)+len(rep.Zipped) != 0 || rep.Zip != nil || len(rep.Left) != 3 {
		t.Fatalf("report: %+v", rep)
	}
	for _, p := range []string{"clean.md", "mod.md", "new.md"} {
		if r := leftReason(rep, p); !strings.Contains(r, "config") {
			t.Errorf("%s: reason %q", p, r)
		}
		if !exists(t, filepath.Join(repo, p)) {
			t.Errorf("%s was removed", p)
		}
	}
	if z := zipsIn(t, repo); len(z) != 0 {
		t.Errorf("no zip: %v", z)
	}
	if after, _ := os.ReadFile(exclude); string(after) != string(before) {
		t.Error("the exclude file changed")
	}
}

// A file replaced by a symbolic link after the import is left, and so is
// what it points to.
func TestSymlinkSwappedIn(t *testing.T) {
	isolate(t)
	root := realPath(t, t.TempDir())
	f := newFixture(t, root)
	f.done("a.md", "same\n")
	write(t, root, "target.md", "same\n")
	if err := os.Remove(filepath.Join(root, "a.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.md", filepath.Join(root, "a.md")); err != nil {
		t.Fatal(err)
	}

	rep := f.mustRetire(true)
	if r := leftReason(rep, "a.md"); !strings.Contains(r, "symbolic link") || rep.Zip != nil {
		t.Fatalf("report: %+v", rep)
	}
	if st, err := os.Lstat(filepath.Join(root, "a.md")); err != nil || st.Mode()&fs.ModeSymlink == 0 {
		t.Error("the link was touched")
	}
	if content(t, filepath.Join(root, "target.md")) != "same\n" {
		t.Error("the link's target was touched")
	}
}

// faultyZips is the real zip store with one step made to fail.
type faultyZips struct {
	disk
	failWrite, failOpen, corrupt bool
}

type failingWriter struct{ ZipFile }

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

type corruptReader struct {
	ZipReader
	size int64
}

// ReadAt flips every byte of the first half of the file, where member
// data is, and leaves the central directory at the end readable.
func (c corruptReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.ZipReader.ReadAt(p, off)
	for i := 0; i < n; i++ {
		if off+int64(i) > 40 && off+int64(i) < c.size/2 {
			p[i] ^= 0xff
		}
	}
	return n, err
}

func (z faultyZips) Create(root *os.Root, name string) (ZipFile, error) {
	f, err := z.disk.Create(root, name)
	if err != nil || !z.failWrite {
		return f, err
	}
	return failingWriter{f}, nil
}

func (z faultyZips) Open(root *os.Root, name string) (ZipReader, int64, error) {
	if z.failOpen {
		return nil, 0, errors.New("cannot open")
	}
	r, size, err := z.disk.Open(root, name)
	if err != nil || !z.corrupt {
		return r, size, err
	}
	return corruptReader{r, size}, size, nil
}

// The zip is written and read back before anything is removed; a failure
// at any step deletes the zip and leaves every file.
func TestZipVerifiedBeforeRemoval(t *testing.T) {
	for name, z := range map[string]faultyZips{
		"write fails":     {failWrite: true},
		"read back fails": {failOpen: true},
		"read back wrong": {corrupt: true},
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			repo := newRepo(t)
			f := newFixture(t, repo)
			f.done("clean.md", "clean\n")
			commitAll(t, repo, "clean")
			body := strings.Repeat("untracked line\n", 200)
			f.done("new.md", body)
			f.zips = z

			rep, err := f.retire(true)
			var se *StoppedError
			if !errors.As(err, &se) {
				t.Fatalf("want a stop, got %v (%+v)", err, rep)
			}
			if len(rep.Deleted)+len(rep.Zipped) != 0 || rep.Zip != nil || rep.ExcludeAdded {
				t.Errorf("nothing done: %+v", rep)
			}
			if content(t, filepath.Join(repo, "clean.md")) != "clean\n" || content(t, filepath.Join(repo, "new.md")) != body {
				t.Error("a file was touched")
			}
			if z := zipsIn(t, repo); len(z) != 0 {
				t.Errorf("the unverified zip stays: %v", z)
			}
			if s := statusOf(t, repo); s != "?? new.md\n" {
				t.Errorf("status: %q", s)
			}
			if b, _ := os.ReadFile(filepath.Join(repo, ".git", "info", "exclude")); strings.Contains(string(b), "smrit-import") {
				t.Error("the exclude file names a zip that is gone")
			}
		})
	}
}

func TestZipSuffix(t *testing.T) {
	isolate(t)
	root := realPath(t, t.TempDir())
	write(t, root, zipName, "an earlier zip")
	f := newFixture(t, root)
	f.done("a.md", "a\n")
	rep := f.mustRetire(true)
	second := filepath.Join(root, "smrit-import-2026-11-09-2.zip")
	if rep.Zip == nil || *rep.Zip != second {
		t.Fatalf("zip: %+v", rep)
	}
	if content(t, filepath.Join(root, zipName)) != "an earlier zip" {
		t.Error("the earlier zip was touched")
	}

	g := newFixture(t, root)
	g.run.ID = "20261109T150000Z-d4e5f6"
	g.done("b.md", "b\n")
	rep = g.mustRetire(true)
	if rep.Zip == nil || *rep.Zip != filepath.Join(root, "smrit-import-2026-11-09-3.zip") {
		t.Fatalf("third: %+v", rep)
	}
	if m := zipMembers(t, second); len(m) != 1 || m["a.md"] != "a\n" {
		t.Errorf("each zip holds only what its run removed: %v", m)
	}
	if m := zipMembers(t, *rep.Zip); len(m) != 1 || m["b.md"] != "b\n" {
		t.Errorf("third zip: %v", m)
	}
}

// The zip goes into info/exclude, which nothing tracks, and then git does
// not list it at all.
func TestExcludeFileNotTracked(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	write(t, repo, "top.md", "top\n")
	root := filepath.Join(repo, "docs")
	f := newFixture(t, root)
	f.done("clean.md", "clean\n")
	commitAll(t, repo, "first")
	f.done("new.md", "new\n")

	rep := f.mustRetire(true)
	if !rep.ExcludeAdded || rep.Zip == nil {
		t.Fatalf("report: %+v", rep)
	}
	if s := statusOf(t, repo); s != " D docs/clean.md\n" {
		t.Errorf("the only change git sees is the deletion: %q", s)
	}
	if ign := gitIn(t, repo, "status", "--porcelain=v1", "--ignored"); !strings.Contains(ign, "!! docs/"+zipName) {
		t.Errorf("the zip is ignored: %q", ign)
	}
	if tracked := gitIn(t, repo, "ls-files"); tracked != "docs/clean.md\ntop.md\n" {
		t.Errorf("tracked files: %q", tracked)
	}
	b := content(t, filepath.Join(repo, ".git", "info", "exclude"))
	if !strings.HasSuffix(b, "\n/docs/"+zipName+"\n") {
		t.Errorf("exclude file: %q", b)
	}

	// A second run the same day adds its own zip once.
	g := newFixture(t, root)
	g.run.ID = "20261109T150000Z-d4e5f6"
	g.done("again.md", "again\n")
	rep = g.mustRetire(true)
	if !rep.ExcludeAdded {
		t.Fatalf("second: %+v", rep)
	}
	b = content(t, filepath.Join(repo, ".git", "info", "exclude"))
	if strings.Count(b, zipName) != 1 || strings.Count(b, "smrit-import-2026-11-09-2.zip") != 1 {
		t.Errorf("exclude file: %q", b)
	}

	// Already ignored: the exclude file is left as it is.
	other := newRepo(t)
	write(t, other, ".gitignore", "*.zip\n")
	commitAll(t, other, "ignore zips")
	h := newFixture(t, other)
	h.done("x.md", "x\n")
	excl := filepath.Join(other, ".git", "info", "exclude")
	before := content(t, excl)
	rep = h.mustRetire(true)
	if rep.ExcludeAdded || rep.Zip == nil || content(t, excl) != before {
		t.Errorf("already ignored: %+v", rep)
	}
}

func TestUnpushedReported(t *testing.T) {
	isolate(t)
	upstream := t.TempDir()
	gitIn(t, upstream, "init", "-q", "--bare")
	repo := newRepo(t)
	f := newFixture(t, repo)
	f.done("pushed.md", "pushed\n")
	commitAll(t, repo, "first")
	gitIn(t, repo, "remote", "add", "origin", upstream)
	gitIn(t, repo, "push", "-q", "-u", "origin", "main")
	f.done("local.md", "local\n")
	commitAll(t, repo, "second")

	rep := f.mustRetire(true)
	if !slices.Equal(sorted(rep.Deleted), []string{"local.md", "pushed.md"}) || !slices.Equal(rep.RecoverableUnpushed, []string{"local.md"}) {
		t.Fatalf("report: %+v", rep)
	}

	solo := newRepo(t)
	g := newFixture(t, solo)
	g.done("a.md", "a\n")
	commitAll(t, solo, "a")
	rep = g.mustRetire(false)
	if !slices.Equal(rep.RecoverableUnpushed, []string{"a.md"}) {
		t.Fatalf("no upstream: %+v", rep)
	}
}

// snapshot is every path under dir, .git included, with its mode and the
// hash of its bytes.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		v := info.Mode().String()
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += " " + sum(string(b))
		}
		out[p] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWithoutApplyTouchesNothing(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	f := newFixture(t, repo)
	f.done("clean.md", "clean\n")
	write(t, repo, "mod.md", "old\n")
	commitAll(t, repo, "first")
	f.done("mod.md", "new\n")
	f.done("new.md", "untracked\n")
	before := snapshot(t, repo)

	rep := f.mustRetire(false)
	if rep.Applied || !slices.Equal(rep.Deleted, []string{"clean.md"}) || !slices.Equal(sorted(rep.Zipped), []string{"mod.md", "new.md"}) {
		t.Fatalf("report: %+v", rep)
	}
	if rep.Zip == nil || *rep.Zip != filepath.Join(repo, zipName) || !rep.ExcludeAdded {
		t.Errorf("says what it would do: %+v", rep)
	}
	after := snapshot(t, repo)
	if len(before) != len(after) {
		t.Fatalf("paths changed: %d before, %d after", len(before), len(after))
	}
	for p, v := range before {
		if after[p] != v {
			t.Errorf("%s changed: %s to %s", p, v, after[p])
		}
	}
}

func TestNotFinishedRunRefused(t *testing.T) {
	isolate(t)
	root := realPath(t, t.TempDir())
	f := newFixture(t, root)
	f.done("a.md", "a\n")
	f.run.Phase = state.PhaseLinks
	rep, err := f.retire(true)
	var re *RefusedError
	if !errors.As(err, &re) || !strings.Contains(re.Msg, "finished") {
		t.Fatalf("want a refusal, got %v (%+v)", err, rep)
	}
	if !exists(t, filepath.Join(root, "a.md")) || len(zipsIn(t, root)) != 0 {
		t.Error("something was touched")
	}

	for _, id := range []string{"20261109T000000Z-000000", "../../etc/passwd", ""} {
		_, err := Run(context.Background(), id, Options{StateDir: f.dir, Apply: true})
		if !errors.As(err, &re) {
			t.Errorf("%q: want a refusal, got %v", id, err)
		}
	}
}

func TestRemovalFailureMidwayReports(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes files from a read only folder")
	}
	isolate(t)
	repo := newRepo(t)
	f := newFixture(t, repo)
	f.done("z/clean.md", "clean\n")
	commitAll(t, repo, "clean")
	f.done("a/x.md", "x\n")
	f.done("b/y.md", "y\n")
	f.done("c/z.md", "z\n")
	locked := filepath.Join(repo, "b")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })

	rep, err := f.retire(true)
	var se *StoppedError
	if !errors.As(err, &se) {
		t.Fatalf("want a stop, got %v (%+v)", err, rep)
	}
	if !slices.Equal(rep.Zipped, []string{"a/x.md"}) || len(rep.Deleted) != 0 || rep.Zip == nil {
		t.Fatalf("report: %+v", rep)
	}
	if r := leftReason(rep, "b/y.md"); !strings.Contains(r, "could not be removed") || !strings.Contains(r, "zip") {
		t.Errorf("b/y.md: %q", r)
	}
	for _, p := range []string{"c/z.md", "z/clean.md"} {
		if r := leftReason(rep, p); !strings.Contains(r, "stopped") {
			t.Errorf("%s: %q", p, r)
		}
	}
	if exists(t, filepath.Join(repo, "a/x.md")) {
		t.Error("a/x.md was removed before the stop")
	}
	for _, p := range []string{"b/y.md", "c/z.md", "z/clean.md"} {
		if !exists(t, filepath.Join(repo, p)) {
			t.Errorf("%s was removed after the stop", p)
		}
	}
	if m := zipMembers(t, *rep.Zip); len(m) != 3 {
		t.Errorf("the zip holds every zipped file: %v", m)
	}
}

// A name the repository tracks is taken even when the file is gone, since
// a zip there would change a tracked file.
func TestTrackedZipNameIsSkipped(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	write(t, repo, zipName, "tracked")
	commitAll(t, repo, "a tracked zip")
	if err := os.Remove(filepath.Join(repo, zipName)); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, repo)
	f.done("a.md", "a\n")
	rep := f.mustRetire(true)
	if rep.Zip == nil || filepath.Base(*rep.Zip) != "smrit-import-2026-11-09-2.zip" {
		t.Fatalf("report: %+v", rep)
	}
	if exists(t, filepath.Join(repo, zipName)) {
		t.Error("the tracked name was written")
	}
	if s := statusOf(t, repo); s != " D "+zipName+"\n" {
		t.Errorf("status: %q", s)
	}
}

func TestExcludeLineEscapes(t *testing.T) {
	for in, want := range map[string]string{
		"docs/smrit-import-2026-11-09.zip": "/docs/smrit-import-2026-11-09.zip",
		`a*b/[x]?\#!.zip`:                  `/a\*b/\[x]\?\\#!.zip`,
		"bad\nname.zip":                    "",
	} {
		if got := excludeLine(in); got != want {
			t.Errorf("excludeLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafePath(t *testing.T) {
	for p, want := range map[string]bool{
		"a.md": true, "docs/a.md": true, "": false, ".": false, "/etc/passwd": false, "../a.md": false,
		"docs/../a.md": false, "docs//a.md": false, "./a.md": false, "docs/": false, `docs\a.md`: false, "a\x00.md": false,
	} {
		if got := safePath(p); got != want {
			t.Errorf("safePath(%q) = %v, want %v", p, got, want)
		}
	}
}

// git status can be told not to look: assume-unchanged, skip-worktree and
// core.ignoreStat make an edited file read clean. Its bytes are compared
// with the blob HEAD holds, so it is zipped, never deleted.
func TestHiddenEditsAreZippedNotDeleted(t *testing.T) {
	for name, hide := range map[string]func(t *testing.T, repo string){
		"assume-unchanged": func(t *testing.T, repo string) {
			gitIn(t, repo, "update-index", "--assume-unchanged", "a.md")
		},
		"skip-worktree": func(t *testing.T, repo string) {
			gitIn(t, repo, "update-index", "--skip-worktree", "a.md")
		},
		"core.ignoreStat": nil,
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			repo := newRepo(t)
			if hide == nil {
				gitIn(t, repo, "config", "core.ignoreStat", "true")
			}
			write(t, repo, "a.md", "committed\n")
			commitAll(t, repo, "a")
			if hide != nil {
				hide(t, repo)
			}
			f := newFixture(t, repo)
			f.done("a.md", "edited, never committed\n")
			if s := statusOf(t, repo); s != "" {
				t.Fatalf("the setup must hide the edit from status: %q", s)
			}

			rep := f.mustRetire(true)
			if len(rep.Deleted) != 0 || !slices.Equal(rep.Zipped, []string{"a.md"}) || rep.Zip == nil {
				t.Fatalf("an edit git status cannot see is zipped: %+v", rep)
			}
			if m := zipMembers(t, *rep.Zip); m["a.md"] != "edited, never committed\n" {
				t.Errorf("zip: %v", m)
			}
		})
	}
}

// editingZips runs edit when the zip is reopened for the read back: after
// the zip holds the checked bytes, before anything is removed.
type editingZips struct {
	disk
	edit func()
}

func (z editingZips) Open(root *os.Root, name string) (ZipReader, int64, error) {
	z.edit()
	return z.disk.Open(root, name)
}

// Each file is hashed again just before it is removed: an edit after the
// zip was checked keeps its new bytes.
func TestEditAfterReadBackIsLeft(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	f := newFixture(t, repo)
	f.done("clean.md", "clean\n")
	commitAll(t, repo, "clean")
	f.done("new.md", "new\n")
	f.done("other.md", "other\n")
	f.zips = editingZips{edit: func() {
		write(t, repo, "clean.md", "clean, edited late\n")
		write(t, repo, "new.md", "new, edited late\n")
	}}

	rep := f.mustRetire(true)
	if !slices.Equal(rep.Zipped, []string{"other.md"}) || len(rep.Deleted) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	for p, body := range map[string]string{"clean.md": "clean, edited late\n", "new.md": "new, edited late\n"} {
		if r := leftReason(rep, p); !strings.Contains(r, "changed") {
			t.Errorf("%s: reason %q", p, r)
		}
		if content(t, filepath.Join(repo, p)) != body {
			t.Errorf("%s lost its late edit", p)
		}
	}
	if m := zipMembers(t, *rep.Zip); m["new.md"] != "new\n" {
		t.Errorf("the zip keeps the checked copy: %v", m)
	}
	assertNoTemporaries(t, repo)
}

// assertNoTemporaries fails when retire left a temporary name behind.
func assertNoTemporaries(t *testing.T, dir string) {
	t.Helper()
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), "smrit-retire") {
			t.Errorf("a temporary file is left: %s", p)
		}
		return nil
	})
}

func writeTestZip(t *testing.T, p string, members map[string]string) {
	t.Helper()
	out, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	names := make([]string, 0, len(members))
	for n := range members {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(w, members[n])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	out.Close()
}

// The read back compares the member set with the files meant to be in it.
func TestVerifyZipMemberSet(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	items := []item{{path: "a.md", sha: sum("a")}, {path: "b/c.md", sha: sum("c")}}
	cases := map[string]map[string]string{
		"exact":   {"a.md": "a", "b/c.md": "c"},
		"extra":   {"a.md": "a", "b/c.md": "c", "x.md": "x"},
		"missing": {"a.md": "a"},
		"wrong":   {"a.md": "a", "b/c.md": "not c"},
	}
	for name, members := range cases {
		writeTestZip(t, filepath.Join(dir, name+".zip"), members)
		err := verifyZip(root, disk{}, name+".zip", items)
		if (err == nil) != (name == "exact") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A zip holding one member twice is refused too.
func TestVerifyZipRefusesDuplicateMember(t *testing.T) {
	dir := t.TempDir()
	out, _ := os.Create(filepath.Join(dir, "d.zip"))
	zw := zip.NewWriter(out)
	for range 2 {
		w, _ := zw.Create("a.md")
		io.WriteString(w, "a")
	}
	zw.Close()
	out.Close()
	root, _ := os.OpenRoot(dir)
	defer root.Close()
	if err := verifyZip(root, disk{}, "d.zip", []item{{path: "a.md", sha: sum("a")}}); err == nil {
		t.Error("a duplicate member passed")
	}
}

// The exclude file and the info folder are never written through a
// symbolic link.
func TestExcludeNeverFollowsLinks(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "target")
	write(t, outside, "target", "untouched")

	gitDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(gitDir, "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(gitDir, "info", "exclude")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := (&excludePlan{file: link, line: "/x.zip"}).apply(); err == nil || content(t, target) != "untouched" {
		t.Errorf("wrote through a linked exclude file: %v", err)
	}
	dangling := filepath.Join(outside, "created")
	os.Remove(link)
	if err := os.Symlink(dangling, link); err != nil {
		t.Fatal(err)
	}
	if err := (&excludePlan{file: link, line: "/x.zip"}).apply(); err == nil || exists(t, dangling) {
		t.Errorf("created a file through a dangling link: %v", err)
	}

	linkedInfo := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(linkedInfo, "info")); err != nil {
		t.Fatal(err)
	}
	if err := (&excludePlan{file: filepath.Join(linkedInfo, "info", "exclude"), line: "/x.zip"}).apply(); err == nil || exists(t, filepath.Join(outside, "exclude")) {
		t.Errorf("wrote into a linked info folder: %v", err)
	}
}

// End to end: a linked exclude file stops retire before anything is removed.
func TestLinkedExcludeStopsRetire(t *testing.T) {
	for name, plant := range map[string]func(t *testing.T, gitDir, outside string){
		"exclude file": func(t *testing.T, gitDir, outside string) {
			os.Remove(filepath.Join(gitDir, "info", "exclude"))
			if err := os.Symlink(filepath.Join(outside, "target"), filepath.Join(gitDir, "info", "exclude")); err != nil {
				t.Fatal(err)
			}
		},
		"info folder": func(t *testing.T, gitDir, outside string) {
			if err := os.RemoveAll(filepath.Join(gitDir, "info")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(gitDir, "info")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			isolate(t)
			outside := t.TempDir()
			write(t, outside, "target", "untouched")
			repo := newRepo(t)
			plant(t, filepath.Join(repo, ".git"), outside)
			f := newFixture(t, repo)
			f.done("new.md", "new\n")
			_, err := f.retire(true)
			var se *StoppedError
			if !errors.As(err, &se) {
				t.Fatalf("want a stop: %v", err)
			}
			if content(t, filepath.Join(outside, "target")) != "untouched" || exists(t, filepath.Join(outside, "exclude")) {
				t.Error("wrote outside the repository")
			}
			if !exists(t, filepath.Join(repo, "new.md")) || len(zipsIn(t, repo)) != 0 {
				t.Error("something was removed, or the zip stayed")
			}
		})
	}
}

func TestExcludeWithoutTrailingNewline(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	write(t, repo, ".git/info/exclude", "*.tmp")
	f := newFixture(t, repo)
	f.done("new.md", "new\n")
	rep := f.mustRetire(true)
	if !rep.ExcludeAdded {
		t.Fatalf("report: %+v", rep)
	}
	if b := content(t, filepath.Join(repo, ".git/info/exclude")); b != "*.tmp\n/"+zipName+"\n" {
		t.Errorf("exclude file: %q", b)
	}
}

// renameHook is the real rename with an action just before or just after it.
type renameHook struct {
	before, after func(oldpath string)
}

func (h renameHook) Rename(root *os.Root, oldname, newname string) error {
	if h.before != nil {
		h.before(oldname)
	}
	err := rootRename{}.Rename(root, oldname, newname)
	if h.after != nil {
		h.after(oldname)
	}
	return err
}

// A file is moved aside, hashed where it now is, and only then unlinked: an
// edit landing between the last check and the removal is kept.
func TestRemovalRace(t *testing.T) {
	t.Run("edited before the move", func(t *testing.T) {
		isolate(t)
		root := realPath(t, t.TempDir())
		f := newFixture(t, root)
		f.done("a.md", "as imported\n")
		f.rename = renameHook{before: func(old string) {
			if filepath.Base(old) == "a.md" {
				write(t, root, "a.md", "edited at the last moment\n")
			}
		}}
		rep := f.mustRetire(true)
		if r := leftReason(rep, "a.md"); !strings.Contains(r, "changed") || len(rep.Zipped) != 0 {
			t.Fatalf("report: %+v", rep)
		}
		if content(t, filepath.Join(root, "a.md")) != "edited at the last moment\n" {
			t.Error("the late edit was lost")
		}
		assertNoTemporaries(t, root)
	})
	t.Run("new file after the move", func(t *testing.T) {
		isolate(t)
		root := realPath(t, t.TempDir())
		f := newFixture(t, root)
		f.done("a.md", "as imported\n")
		f.rename = renameHook{after: func(old string) {
			if filepath.Base(old) == "a.md" {
				write(t, root, "a.md", "saved again\n")
			}
		}}
		rep := f.mustRetire(true)
		if !slices.Equal(rep.Zipped, []string{"a.md"}) {
			t.Fatalf("report: %+v", rep)
		}
		if content(t, filepath.Join(root, "a.md")) != "saved again\n" {
			t.Error("the file saved after the move was touched")
		}
		assertNoTemporaries(t, root)
	})
}

// When git cannot say whether the zip's name is tracked, apply stops, and
// the preview says it would stop rather than promising a zip it has no
// name for.
func TestZipNameUnknownStopsPreviewAndApply(t *testing.T) {
	isolate(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\ncase \"$*\" in *smrit-import-*) exit 128;; esac\nexec '" + realGit + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := newRepo(t)
	f := newFixture(t, repo)
	f.done("new.md", "new\n")
	f.git = wrapper

	for _, apply := range []bool{false, true} {
		rep, err := f.retire(apply)
		var se *StoppedError
		if !errors.As(err, &se) {
			t.Fatalf("apply %v: want a stop, got %v (%+v)", apply, err, rep)
		}
		if rep.Zip != nil || len(rep.Zipped) != 0 || !strings.Contains(leftReason(rep, "new.md"), "stop") {
			t.Errorf("apply %v: report %+v", apply, rep)
		}
		if !exists(t, filepath.Join(repo, "new.md")) || len(zipsIn(t, repo)) != 0 {
			t.Errorf("apply %v: something changed", apply)
		}
	}
}

func TestDuplicatePlanEntries(t *testing.T) {
	isolate(t)
	root := realPath(t, t.TempDir())
	f := newFixture(t, root)
	f.done("a.md", "a\n")
	f.run.Plan.Entries = append(f.run.Plan.Entries, f.run.Plan.Entries[0])
	rep := f.mustRetire(true)
	if !slices.Equal(rep.Zipped, []string{"a.md"}) || len(rep.Left) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if m := zipMembers(t, *rep.Zip); len(m) != 1 {
		t.Errorf("zip: %v", m)
	}
}

// HEAD is asked again at removal: a commit landing after the check that
// takes these bytes out of HEAD leaves the file.
func TestHeadMovedBeforeRemovalIsLeft(t *testing.T) {
	isolate(t)
	repo := newRepo(t)
	f := newFixture(t, repo)
	f.done("a.md", "a\n")
	commitAll(t, repo, "a")
	f.rename = renameHook{before: func(old string) {
		if filepath.Base(old) == "a.md" {
			write(t, repo, "a.md", "b\n")
			commitAll(t, repo, "b")
			write(t, repo, "a.md", "a\n")
		}
	}}
	rep := f.mustRetire(true)
	if r := leftReason(rep, "a.md"); !strings.Contains(r, "changed") || len(rep.Deleted) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if content(t, filepath.Join(repo, "a.md")) != "a\n" {
		t.Error("the file was not put back")
	}
	assertNoTemporaries(t, repo)
}

// A file that changed is put back, but never over a file that took its
// name meanwhile: then it stays under the temporary name, and the report
// says where.
func TestPutBackNeverReplaces(t *testing.T) {
	isolate(t)
	root := realPath(t, t.TempDir())
	f := newFixture(t, root)
	f.done("a.md", "as imported\n")
	f.rename = renameHook{
		before: func(old string) {
			if filepath.Base(old) == "a.md" {
				write(t, root, "a.md", "edited\n")
			}
		},
		after: func(old string) {
			if filepath.Base(old) == "a.md" {
				write(t, root, "a.md", "saved again\n")
			}
		},
	}
	rep := f.mustRetire(true)
	r := leftReason(rep, "a.md")
	if !strings.Contains(r, "kept as .smrit-retire-") {
		t.Fatalf("report: %+v", rep)
	}
	if content(t, filepath.Join(root, "a.md")) != "saved again\n" {
		t.Error("the new file was replaced")
	}
	kept := strings.Fields(strings.TrimPrefix(r[strings.Index(r, "kept as ")+len("kept as "):], ""))[0]
	kept = strings.TrimSuffix(kept, ",")
	if content(t, filepath.Join(root, kept)) != "edited\n" {
		t.Errorf("the edited bytes are not at %s", kept)
	}
}

// Retire moves and puts back through the folder it opened, never by a path
// rebuilt from its name: a folder swapped for another while retire runs is
// left alone, and the file in the folder retire opened is put back there.
func TestFolderSwappedMidRunIsNotTouched(t *testing.T) {
	isolate(t)
	parent := realPath(t, t.TempDir())
	root := filepath.Join(parent, "notes")
	moved := filepath.Join(parent, "notes-moved")
	f := newFixture(t, root)
	f.done("a.md", "as imported\n")
	f.rename = renameHook{before: func(old string) {
		if filepath.Base(old) != "a.md" {
			return
		}
		write(t, root, "a.md", "edited at the last moment\n")
		if err := os.Rename(root, moved); err != nil {
			t.Fatal(err)
		}
		write(t, root, "a.md", "decoy\n")
	}}
	rep, _ := f.retire(true)
	if len(rep.Zipped) != 0 || len(rep.Deleted) != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if content(t, filepath.Join(root, "a.md")) != "decoy\n" {
		t.Error("the decoy was touched")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 {
		t.Errorf("the decoy folder changed: %v %v", entries, err)
	}
	if content(t, filepath.Join(moved, "a.md")) != "edited at the last moment\n" {
		t.Error("the edited file was not put back in the folder retire opened")
	}
	assertNoTemporaries(t, moved)
}

// A file an earlier retire moved aside and never finished with is listed in
// the preview and after apply, and is never zipped, deleted or changed.
func TestLeftoversListedNeverTouched(t *testing.T) {
	isolate(t)
	root := realPath(t, t.TempDir())
	f := newFixture(t, root)
	f.done("a.md", "as imported\n")
	const name = ".smrit-retire-0123456789abcdef.tmp"
	write(t, root, name, "moved aside\n")
	f.imported(name, "moved aside\n", state.StatusCreated, "DocLeftoverAAAAAAAAAA")
	for _, apply := range []bool{false, true} {
		rep := f.mustRetire(apply)
		if len(rep.Leftovers) != 1 || rep.Leftovers[0].Path != name || rep.Leftovers[0].Folder != root || rep.Leftovers[0].Note == "" {
			t.Fatalf("apply %v: leftovers %+v", apply, rep.Leftovers)
		}
		if slices.Contains(rep.Zipped, name) || slices.Contains(rep.Deleted, name) {
			t.Fatalf("apply %v: report %+v", apply, rep)
		}
		if content(t, filepath.Join(root, name)) != "moved aside\n" {
			t.Fatalf("apply %v: the leftover changed", apply)
		}
		if apply {
			if !slices.Equal(rep.Zipped, []string{"a.md"}) {
				t.Fatalf("report %+v", rep)
			}
			if _, in := zipMembers(t, *rep.Zip)[name]; in {
				t.Fatal("the leftover went into the zip")
			}
		}
	}
}

// In a folder that takes no hard link, a file that must go back is left
// under its temporary name: a rename back would replace whatever took the
// name meanwhile. The report names where it is kept.
func TestPutBackWithoutHardLinksKeepsTheTemporaryName(t *testing.T) {
	isolate(t)
	root := realPath(t, t.TempDir())
	f := newFixture(t, root)
	f.done("a.md", "as imported\n")
	f.rename = renameHook{before: func(old string) {
		if filepath.Base(old) == "a.md" {
			write(t, root, "a.md", "edited\n")
		}
	}}
	f.link = func(*os.Root, string, string) error { return errors.ErrUnsupported }
	rep := f.mustRetire(true)
	r := leftReason(rep, "a.md")
	if !strings.Contains(r, "kept as .smrit-retire-") || !strings.Contains(r, "hard link") {
		t.Fatalf("report: %+v", rep)
	}
	if exists(t, filepath.Join(root, "a.md")) {
		t.Fatal("the file was renamed back over its name")
	}
	kept := strings.TrimSuffix(strings.Fields(r[strings.Index(r, "kept as ")+len("kept as "):])[0], ",")
	if content(t, filepath.Join(root, kept)) != "edited\n" {
		t.Errorf("the edited bytes are not at %s", kept)
	}
	if len(rep.Zipped) != 0 || len(rep.Deleted) != 0 {
		t.Fatalf("report: %+v", rep)
	}
}
