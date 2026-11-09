package cli

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/smrit-ai/cli/internal/version"
)

var update = flag.Bool("update", false, "rewrite testdata/help.snapshot.json")

// The snapshot pins the whole command tree: a changed flag, summary or exit
// code shows up here, so it is a deliberate change. The version is fixed for
// the test so a release does not change the file.
func TestHelpMatchesSnapshot(t *testing.T) {
	old := version.Version
	version.Version = "0.0.0"
	t.Cleanup(func() { version.Version = old })

	code, out, _ := run(t, Commands(DefaultServices()), true, "help", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	path := filepath.Join("..", "..", "testdata", "help.snapshot.json")
	if *update {
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Cannot read the snapshot: %v. Create it with: go test ./internal/cli -run TestHelpMatchesSnapshot -update", err)
	}
	if string(want) != out {
		t.Fatalf("The help tree changed. If that is intended, regenerate the snapshot with: go test ./internal/cli -run TestHelpMatchesSnapshot -update\n\ngot:\n%s", out)
	}
}

func TestHelpJSONIsOptIn(t *testing.T) {
	table := []Command{echo()}
	_, piped, _ := run(t, table, false, "help")
	if len(piped) == 0 || piped[0] == '{' {
		t.Fatalf("plain help must stay text when piped: %q", piped)
	}
	for _, args := range [][]string{{"--json", "help"}, {"help", "--json"}} {
		_, out, _ := run(t, table, true, args...)
		if len(out) == 0 || out[0] != '{' {
			t.Fatalf("%v should give JSON: %q", args, out)
		}
	}
}
