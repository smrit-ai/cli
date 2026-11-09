package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain points HOME, the harnesses' config folders and the state folder
// at a fresh temporary folder, and PATH at the system folders only, before
// any test runs. A test that forgets fakeHarnesses then still cannot reach
// the developer's real Claude Code, Codex or Antigravity configuration, nor
// run their real harness commands.
func TestMain(m *testing.M) {
	os.Exit(isolated(m))
}

func isolated(m *testing.M) int {
	home, err := os.MkdirTemp("", "smrit-cli-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(home)
	for k, v := range map[string]string{
		"HOME":              home,
		"CLAUDE_CONFIG_DIR": filepath.Join(home, ".claude"),
		"CODEX_HOME":        filepath.Join(home, ".codex"),
		"XDG_STATE_HOME":    filepath.Join(home, ".local", "state"),
		"PATH":              "/usr/bin:/bin",
	} {
		os.Setenv(k, v)
	}
	return m.Run()
}

// underTemp reports whether path is inside the system's temporary folder,
// both with their symbolic links resolved.
func underTemp(path string) bool {
	p, err1 := filepath.EvalSymlinks(path)
	tmp, err2 := filepath.EvalSymlinks(os.TempDir())
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(tmp, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func TestUnderTemp(t *testing.T) {
	if !underTemp(t.TempDir()) {
		t.Fatal("a test's temporary folder is under the temporary folder")
	}
	real, _ := os.UserHomeDir()
	for _, p := range []string{"/", "/Users", os.TempDir(), filepath.Join(os.TempDir(), ".."), "/tmpfoo"} {
		if underTemp(p) {
			t.Fatalf("%s is not under the temporary folder", p)
		}
	}
	if os.Getenv("HOME") == real && !underTemp(real) {
		t.Fatal("TestMain must have replaced HOME")
	}
}

func TestMainIsolatesTheEnvironment(t *testing.T) {
	if !underTemp(os.Getenv("HOME")) || !strings.HasPrefix(os.Getenv("CLAUDE_CONFIG_DIR"), os.Getenv("HOME")) || os.Getenv("PATH") != "/usr/bin:/bin" ||
		!strings.HasPrefix(os.Getenv("CODEX_HOME"), os.Getenv("HOME")) {
		t.Fatalf("HOME %q CLAUDE_CONFIG_DIR %q CODEX_HOME %q PATH %q", os.Getenv("HOME"), os.Getenv("CLAUDE_CONFIG_DIR"), os.Getenv("CODEX_HOME"), os.Getenv("PATH"))
	}
}
