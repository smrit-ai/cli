package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func scanFixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	const marker = "BODYMARKER5e1d"
	files := map[string]string{
		"guide.md":       "---\ntitle: Guide\n---\nIntro " + marker + ".\n[next](next.md)\n",
		"next.md":        "# Next\n\nText " + marker + "\n![pic](pic.png)\n",
		"data.csv":       "a,b\n" + marker + ",2\n",
		"pic.png":        "\x89PNG\x00" + marker,
		"notes.txt":      marker,
		"leak.md":        "# Keys\nAKIAABCDEFGHIJKLMNOP " + marker + "\n",
		"script.py":      "print('" + marker + "')\n",
		".env":           "A=" + marker,
		"sub/deep.md":    "# Deep\n" + marker + "\n",
		"latin1.md":      "caf\xe9 " + marker,
		"README.md":      "# Read me\n" + marker,
		"credentials.md": marker,
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "guide.md", "data.csv"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return root, marker
}

// No file body reaches stdout or stderr, as JSON or as text.
func TestNoBodiesInOutput(t *testing.T) {
	root, marker := scanFixture(t)
	table := Commands(DefaultServices())
	code, out, errb := run(t, table, false, "scan", root)
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errb)
	}
	var res struct {
		Files []struct {
			Path  string   `json:"path"`
			Flags []string `json:"flags"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("not JSON when not a terminal: %v\n%s", err, out)
	}
	if len(res.Files) != 12 {
		t.Fatalf("want 12 files, got %d: %s", len(res.Files), out)
	}
	if strings.Contains(out, marker) || strings.Contains(errb, marker) {
		t.Fatalf("a file body reached the output:\n%s\n%s", out, errb)
	}
	code, text, errb := run(t, table, true, "scan", root)
	if code != ExitOK || strings.HasPrefix(text, "{") {
		t.Fatalf("text on a terminal, exit %d: %s", code, text)
	}
	if strings.Contains(text, marker) || strings.Contains(errb, marker) {
		t.Fatalf("a file body reached the text output:\n%s", text)
	}
	if !strings.Contains(text, "guide.md") || !strings.Contains(text, "secret") {
		t.Fatalf("text output lists files and flags: %s", text)
	}
}

func TestScanExitCodes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	table := Commands(DefaultServices())
	code, out, errb := run(t, table, false, "scan", home)
	if code != ExitRefused || out != "" || strings.Count(strings.TrimSpace(errb), "\n") != 0 || errb == "" {
		t.Fatalf("home: exit %d out %q err %q", code, out, errb)
	}
	if strings.ContainsAny(errb, "—–") || strings.Contains(errb, " - ") {
		t.Fatalf("dash punctuation in %q", errb)
	}
	if code, _, errb := run(t, table, false, "scan"); code != ExitUsage || errb == "" {
		t.Fatalf("no path: exit %d", code)
	}
	if code, _, _ := run(t, table, false, "scan", filepath.Join(home, "missing")); code != ExitUsage {
		t.Fatalf("missing path: exit %d", code)
	}
}
