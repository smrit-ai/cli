package setup

import (
	"fmt"
	"os"
	"path/filepath"
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
	home, err := os.MkdirTemp("", "smrit-setup-test-home-")
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
