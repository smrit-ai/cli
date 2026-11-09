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
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
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

func TestScanAllowsAListedFixtureOnly(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	commit(t, dir, "testdata/fake.txt", fakeKey+"\n", "test: fixture")
	commit(t, dir, "other.txt", fakeKey+"\n", "test: elsewhere")
	allow, err := ParseAllowlist("# fake\ntestdata/fake.txt aws-access-key-id\n\n")
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
	if _, err := ParseAllowlist("just-a-path\n"); err == nil {
		t.Fatal("accepted a line with one field")
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
	os.WriteFile(allow, []byte("fx.txt aws-access-key-id\n"), 0o644)
	if out, err := runScript(t, dir, allow); err != nil {
		t.Fatalf("allowlisted fixture failed: %v %s", err, out)
	}
}
