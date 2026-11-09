package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/smrit-ai/cli/internal/version"
)

func run(t *testing.T, table []Command, tty bool, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), table, args, &out, &errb, tty)
	return code, out.String(), errb.String()
}

func echo() Command {
	return Command{
		Name: "echo", Summary: "Print a thing.",
		Flags: []Flag{{Name: "name", Usage: "who", Default: "world"}, {Name: "loud", Usage: "shout", Bool: true}},
		Run: func(ctx context.Context, env *Env, args []string) int {
			env.Result(map[string]any{"args": args})
			return ExitOK
		},
	}
}

func TestJSONWhenNotATerminal(t *testing.T) {
	table := []Command{echo()}
	cases := []struct {
		name string
		tty  bool
		args []string
		json bool
	}{
		{"pipe defaults to json", false, []string{"echo", "x"}, true},
		{"terminal defaults to text", true, []string{"echo", "x"}, false},
		{"flag forces json on a terminal", true, []string{"--json", "echo", "x"}, true},
		{"false turns it off when piped", false, []string{"--json=false", "echo", "x"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, out, _ := run(t, table, c.tty, c.args...)
			if code != ExitOK {
				t.Fatalf("exit %d", code)
			}
			if got := strings.HasPrefix(out, "{") && strings.HasSuffix(out, "}\n"); got != c.json {
				t.Fatalf("json=%v want %v, output %q", got, c.json, out)
			}
			if c.json && strings.Count(out, "\n") != 1 {
				t.Fatalf("json output must be one line: %q", out)
			}
		})
	}
}

func TestExitCodes(t *testing.T) {
	want := map[string]int{
		"ExitOK": 0, "ExitPartial": 1, "ExitUsage": 2, "ExitNotSignedIn": 3,
		"ExitRefused": 4, "ExitUnavailable": 5, "ExitUpgrade": 6,
	}
	got := map[string]int{
		"ExitOK": ExitOK, "ExitPartial": ExitPartial, "ExitUsage": ExitUsage,
		"ExitNotSignedIn": ExitNotSignedIn, "ExitRefused": ExitRefused,
		"ExitUnavailable": ExitUnavailable, "ExitUpgrade": ExitUpgrade,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
}

func TestRunExitBehaviour(t *testing.T) {
	table := []Command{echo()}
	cases := []struct {
		name string
		args []string
		code int
	}{
		{"version", []string{"--version"}, ExitOK},
		{"help command", []string{"help"}, ExitOK},
		{"no args shows help", nil, ExitOK},
		{"unknown command", []string{"nope"}, ExitUsage},
		{"unknown global flag", []string{"--nope", "echo"}, ExitUsage},
		{"unknown command flag", []string{"echo", "--nope"}, ExitUsage},
		{"known command flag", []string{"echo", "--name=a", "--loud"}, ExitOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, errb := run(t, table, false, c.args...)
			if code != c.code {
				t.Fatalf("exit %d want %d", code, c.code)
			}
			if c.code == ExitUsage {
				if errb == "" || strings.Count(strings.TrimSpace(errb), "\n") != 0 {
					t.Fatalf("want one line on stderr, got %q", errb)
				}
			}
		})
	}
}

func TestVersionPrintsVersion(t *testing.T) {
	code, out, _ := run(t, nil, false, "--version")
	if code != ExitOK || strings.TrimSpace(out) != version.Version {
		t.Fatalf("code %d out %q", code, out)
	}
}

func TestHelpListsCommandsAsPlainText(t *testing.T) {
	_, out, _ := run(t, []Command{echo()}, false, "help")
	if !strings.Contains(out, "echo") || !strings.Contains(out, "Print a thing.") {
		t.Fatalf("help missing command: %q", out)
	}
	if strings.HasPrefix(out, "{") {
		t.Fatalf("help must be plain text: %q", out)
	}
}

func TestNoDashPunctuationInMessages(t *testing.T) {
	_, _, errb := run(t, nil, false, "nope")
	if strings.ContainsAny(errb, "—–") || strings.Contains(errb, " - ") {
		t.Fatalf("dash punctuation in %q", errb)
	}
}
