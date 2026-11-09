package historyscan

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "maintenance.auto=false", "-c", "gc.auto=0", "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func commit(t *testing.T, dir, path, content, message string) {
	t.Helper()
	full := filepath.Join(dir, path)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", path)
	run(t, dir, "commit", "-q", "-m", message)
}

const fakeKey = "AKIA" + "ABCDEFGHIJKLMNOP"

func TestScanCatchesASecretThatWasLaterRemoved(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	commit(t, dir, "notes.md", "key "+fakeKey+"\n", "docs: add notes")
	commit(t, dir, "notes.md", "clean\n", "docs: clean notes")
	res, err := Scan(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := res.Findings()
	if len(f) != 1 || f[0].Path != "notes.md" || f[0].Pattern != "aws-access-key-id" || len(f[0].Commit) != 40 {
		t.Fatalf("findings %+v", f)
	}
	if res.Commits != 2 {
		t.Fatalf("commits %d", res.Commits)
	}
}

func blobOf(t *testing.T, dir, path string) string {
	t.Helper()
	return strings.TrimSpace(run(t, dir, "rev-parse", "HEAD:"+path))
}

func TestScanAllowsAListedFixtureOnly(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	commit(t, dir, "testdata/fake.txt", fakeKey+"\n", "test: fixture")
	blob := blobOf(t, dir, "testdata/fake.txt")
	commit(t, dir, "other.txt", fakeKey+"\n", "test: elsewhere")
	allow, err := ParseAllowlist("# fake\ntestdata/fake.txt aws-access-key-id " + blob + "\n\n")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Scan(dir, allow)
	if err != nil {
		t.Fatal(err)
	}
	f := res.Findings()
	if len(f) != 1 || f[0].Path != "other.txt" || len(res.Hits) != 2 {
		t.Fatalf("hits %+v", res.Hits)
	}
}

// An allowlist line covers one version of a file: a key committed later
// into the same allowlisted file is a finding.
func TestScanPinsTheAllowlistToBlobs(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	commit(t, dir, "internal/x_test.go", "const k = \""+fakeKey+"\"\n", "test: fixture")
	allow, err := ParseAllowlist("internal/x_test.go aws-access-key-id " + blobOf(t, dir, "internal/x_test.go") + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := Scan(dir, allow); err != nil || len(res.Findings()) != 0 || len(res.Hits) != 1 {
		t.Fatalf("the listed version: %v %+v", err, res.Hits)
	}
	planted := "AKIA" + "QRSTUVWXYZ234567"
	commit(t, dir, "internal/x_test.go", "const k = \""+fakeKey+"\"\nconst real = \""+planted+"\"\n", "test: more")
	res, err := Scan(dir, allow)
	if err != nil {
		t.Fatal(err)
	}
	f := res.Findings()
	if len(f) != 1 || f[0].Path != "internal/x_test.go" || f[0].Blob != blobOf(t, dir, "internal/x_test.go") || len(res.Hits) != 2 {
		t.Fatalf("a new version of an allowlisted file passed: %+v", res.Hits)
	}
}

// Lines prints every hit as an allowlist line, one per path, pattern and
// blob, for a person to read before adding it.
func TestLinesListEveryHitWithItsBlob(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	commit(t, dir, "a.txt", fakeKey+"\n", "test: a")
	first := blobOf(t, dir, "a.txt")
	commit(t, dir, "a.txt", fakeKey+"\nmore\n", "test: a again")
	second := blobOf(t, dir, "a.txt")
	res, err := Scan(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := Lines(res)
	want := []string{"a.txt aws-access-key-id " + first, "a.txt aws-access-key-id " + second}
	if first > second {
		want[0], want[1] = want[1], want[0]
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines %q, want %q", got, want)
	}
	if allow, err := ParseAllowlist(strings.Join(got, "\n")); err != nil || len(allow) != 2 {
		t.Fatalf("the lines do not parse: %v %+v", err, allow)
	}
}

func TestScanReportsNeverCarryTheMatchingText(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	commit(t, dir, "a.txt", fakeKey+"\n", "docs: a")
	res, _ := Scan(dir, nil)
	if strings.Contains(strings.Join([]string{res.Hits[0].Commit, res.Hits[0].Path, res.Hits[0].Pattern}, " "), fakeKey) {
		t.Fatal("report holds the match")
	}
}

func TestParseAllowlistRefusesABadLine(t *testing.T) {
	for _, line := range []string{
		"just-a-path",
		"a.txt aws-access-key-id", // a path and pattern with no blob covers every version
		"a.txt aws-access-key-id HEAD",
		"a.txt aws-access-key-id 0123456789abcdef",
		"a.txt aws-access-key-id " + strings.Repeat("A", 40),
	} {
		if _, err := ParseAllowlist(line + "\n"); err == nil {
			t.Errorf("accepted %q", line)
		}
	}
	for _, n := range []int{40, 64} {
		if _, err := ParseAllowlist("a.txt aws-access-key-id " + strings.Repeat("a", n) + "\n"); err != nil {
			t.Errorf("refused a %d character blob id: %v", n, err)
		}
	}
}

func scriptPath(t *testing.T) string {
	p, err := filepath.Abs("../../../scripts/check_history.sh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func runScript(t *testing.T, dir, allow string) (string, error) {
	cmd := exec.Command("sh", scriptPath(t))
	cmd.Env = append(os.Environ(), "CHECK_HISTORY_TARGET="+dir, "CHECK_HISTORY_ALLOWLIST="+allow)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestScriptCatchesATrailer(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	commit(t, dir, "a.txt", "hello\n", "docs: a\n\nCo-Authored-By: Someone <x@example.com>")
	allow := filepath.Join(t.TempDir(), "allow")
	os.WriteFile(allow, nil, 0o644)
	out, err := runScript(t, dir, allow)
	if err == nil || !strings.Contains(out, "trailer") {
		t.Fatalf("script passed or said nothing: %v %s", err, out)
	}
}

func TestScriptCatchesASecretAndPassesWhenAllowed(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	commit(t, dir, "fx.txt", fakeKey+"\n", "test: fixture")
	allow := filepath.Join(t.TempDir(), "allow")
	os.WriteFile(allow, nil, 0o644)
	out, err := runScript(t, dir, allow)
	if err == nil || !strings.Contains(out, "FOUND") || strings.Contains(out, fakeKey) {
		t.Fatalf("expected a finding without the text: %v %s", err, out)
	}
	os.WriteFile(allow, []byte("fx.txt aws-access-key-id "+blobOf(t, dir, "fx.txt")+"\n"), 0o644)
	if out, err := runScript(t, dir, allow); err != nil {
		t.Fatalf("allowlisted fixture failed: %v %s", err, out)
	}
}
