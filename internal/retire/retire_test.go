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
	t     *testing.T
	dir   string
	root  string
	run   *state.Run
	n     int
	zips  ZipStore
	apply bool
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
	return Run(context.Background(), f.run.ID, Options{StateDir: f.dir, Apply: apply, Now: func() time.Time { return today }, Zips: f.zips})
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
