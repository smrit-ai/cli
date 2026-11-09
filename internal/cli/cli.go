// Package cli is the command table and the shared behaviour every command has:
// exit codes, JSON versus text output, and flag handling.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/smrit-ai/cli/internal/version"
)

// Exit codes. Scripts and agents rely on these, so they do not change.
const (
	ExitOK          = 0
	ExitPartial     = 1
	ExitUsage       = 2
	ExitNotSignedIn = 3
	ExitRefused     = 4
	ExitUnavailable = 5
	ExitUpgrade     = 6
)

// Flag describes one flag of a command.
type Flag struct {
	Name    string
	Usage   string
	Default string
	Bool    bool
}

// Command is one entry in the command table.
type Command struct {
	Name    string
	Summary string
	Flags   []Flag
	Run     func(ctx context.Context, env *Env, args []string) int
}

// Env is what a command runs against. Progress goes to Stderr only.
type Env struct {
	Stdout, Stderr io.Writer
	JSON           bool
	// Flags holds the parsed value of each of the command's flags, by name.
	// A bool flag reads "true" or "false".
	Flags map[string]string
}

// Result writes v as one JSON object and a newline under JSON mode, and as
// readable text otherwise.
func (e *Env) Result(v any) {
	if e.JSON {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(v); err != nil {
			fmt.Fprintf(e.Stderr, "Could not encode the result: %v\n", err)
			return
		}
		e.Stdout.Write(buf.Bytes())
		return
	}
	switch x := v.(type) {
	case string:
		fmt.Fprintln(e.Stdout, x)
	case fmt.Stringer:
		fmt.Fprintln(e.Stdout, x.String())
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(e.Stdout, "%s: %v\n", k, x[k])
		}
	default:
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			fmt.Fprintf(e.Stdout, "%v\n", v)
			return
		}
		fmt.Fprintln(e.Stdout, string(b))
	}
}

// IsTerminal reports whether f is a character device, using the standard library only.
func IsTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// Main runs the process: real stdout, real terminal detection.
func Main(table []Command, args []string) int {
	return Run(context.Background(), table, args, os.Stdout, os.Stderr, IsTerminal(os.Stdout))
}

// Validate checks the table before anything runs. The json flag belongs to
// the shell around every command, so a command may not declare its own.
func Validate(table []Command) error {
	for _, c := range table {
		for _, f := range c.Flags {
			if f.Name == "json" {
				return fmt.Errorf("Command %s declares a json flag, which is reserved for every command.", c.Name)
			}
		}
	}
	return nil
}

func usageError(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, format+"\n", a...)
	return ExitUsage
}

// Run parses global flags, finds the command and runs it. JSON output is the
// default when stdout is not a terminal; --json and --json=false override it.
func Run(ctx context.Context, table []Command, args []string, stdout, stderr io.Writer, stdoutIsTerminal bool) int {
	if err := Validate(table); err != nil {
		return usageError(stderr, "%v", err)
	}
	jsonMode := !stdoutIsTerminal
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			break
		}
		switch a {
		case "--version":
			fmt.Fprintln(stdout, version.Version)
			return ExitOK
		case "--help", "-h":
			writeHelp(stdout, table)
			return ExitOK
		case "--json", "--json=true":
			jsonMode = true
		case "--json=false":
			jsonMode = false
		default:
			return usageError(stderr, "Unknown flag %s. Run smrit help to see what is available.", a)
		}
	}
	if i >= len(args) {
		writeHelp(stdout, table)
		return ExitOK
	}
	name, rest := args[i], args[i+1:]
	if name == "help" {
		writeHelp(stdout, table)
		return ExitOK
	}
	var cmd *Command
	for j := range table {
		if table[j].Name == name {
			cmd = &table[j]
			break
		}
	}
	if cmd == nil {
		return usageError(stderr, "Unknown command %s. Run smrit help to see the commands.", name)
	}

	fs := flag.NewFlagSet(cmd.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	vals := map[string]*string{}
	bools := map[string]*bool{}
	for _, f := range cmd.Flags {
		if f.Bool {
			bools[f.Name] = fs.Bool(f.Name, f.Default == "true", f.Usage)
		} else {
			vals[f.Name] = fs.String(f.Name, f.Default, f.Usage)
		}
	}
	// Commands may also take --json after the command name.
	jsonFlag := fs.Bool("json", false, "")
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeCommandHelp(stdout, cmd)
			return ExitOK
		}
		msg := strings.TrimPrefix(err.Error(), "flag provided but not defined: ")
		return usageError(stderr, "Unknown or invalid flag %s for smrit %s.", msg, cmd.Name)
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "json" {
			jsonMode = *jsonFlag
		}
	})
	flags := map[string]string{}
	for n, p := range vals {
		flags[n] = *p
	}
	for n, p := range bools {
		flags[n] = fmt.Sprint(*p)
	}
	env := &Env{Stdout: stdout, Stderr: stderr, JSON: jsonMode, Flags: flags}
	return cmd.Run(ctx, env, fs.Args())
}

func writeHelp(w io.Writer, table []Command) {
	fmt.Fprintln(w, "smrit is the command line for your Smrit workspace.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Usage: smrit [--json[=false]] <command> [flags] [arguments]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintf(w, "  %-12s %s\n", "help", "List the commands.")
	cmds := append([]Command(nil), table...)
	sort.Slice(cmds, func(a, b int) bool { return cmds[a].Name < cmds[b].Name })
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-12s %s\n", c.Name, c.Summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Flags:")
	fmt.Fprintln(w, "  --json       Write JSON. This is the default when output is not a terminal.")
	fmt.Fprintln(w, "  --version    Print the version.")
}

func writeCommandHelp(w io.Writer, c *Command) {
	fmt.Fprintf(w, "smrit %s: %s\n", c.Name, c.Summary)
	for _, f := range c.Flags {
		fmt.Fprintf(w, "  --%-12s %s\n", f.Name, f.Usage)
	}
}
