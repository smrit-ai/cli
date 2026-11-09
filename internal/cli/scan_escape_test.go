package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A file name can hold terminal escapes; the text output shows them quoted
// rather than sending them to the terminal.
func TestScanTextEscapesControlCharacters(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	name := "a\x1b[2Jred.md"
	if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, text, errb := run(t, Commands(DefaultServices()), true, "scan", root)
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, errb)
	}
	if strings.ContainsRune(text, 0x1b) {
		t.Fatalf("an escape reached the terminal: %q", text)
	}
	if !strings.Contains(text, `a\x1b[2Jred.md`) {
		t.Fatalf("the name is shown escaped: %q", text)
	}
}
