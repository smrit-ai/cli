package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"

	"github.com/smrit-ai/cli/internal/version"
)

type helpFlag struct {
	Name       string `json:"name"`
	Usage      string `json:"usage"`
	Default    string `json:"default"`
	Bool       bool   `json:"bool"`
	Repeatable bool   `json:"repeatable"`
}

type helpCommand struct {
	Name    string     `json:"name"`
	Summary string     `json:"summary"`
	Flags   []helpFlag `json:"flags"`
}

type helpExit struct {
	Code    int    `json:"code"`
	Meaning string `json:"meaning"`
}

type helpDoc struct {
	Program     string        `json:"program"`
	Version     string        `json:"version"`
	GlobalFlags []helpFlag    `json:"global_flags"`
	Commands    []helpCommand `json:"commands"`
	ExitCodes   []helpExit    `json:"exit_codes"`
}

// exitMeanings is the documented meaning of each exit code, in code order.
var exitMeanings = []helpExit{
	{ExitOK, "The command did what was asked."},
	{ExitPartial, "The command ran but some items failed or were skipped."},
	{ExitUsage, "The command line was wrong: an unknown command or flag, or a missing argument."},
	{ExitNotSignedIn, "There is no usable sign in; run smrit login."},
	{ExitRefused, "The server refused the request, for example because of permissions or limits."},
	{ExitUnavailable, "The server could not be reached or failed; try again later."},
	{ExitUpgrade, "This version of smrit is too old for the server; install a newer release."},
}

// writeHelpJSON writes the whole command tree, built from the table itself so
// it cannot drift from what the commands accept.
func writeHelpJSON(w io.Writer, table []Command) {
	doc := helpDoc{
		Program: "smrit",
		Version: version.Version,
		GlobalFlags: []helpFlag{
			{Name: "json", Usage: "Write JSON. This is the default when output is not a terminal.", Default: "false", Bool: true},
			{Name: "version", Usage: "Print the version.", Default: "false", Bool: true},
		},
		Commands:  []helpCommand{},
		ExitCodes: exitMeanings,
	}
	cmds := append([]Command(nil), table...)
	sort.Slice(cmds, func(a, b int) bool { return cmds[a].Name < cmds[b].Name })
	for _, c := range cmds {
		hc := helpCommand{Name: c.Name, Summary: c.Summary, Flags: []helpFlag{}}
		for _, f := range c.Flags {
			hc.Flags = append(hc.Flags, helpFlag{f.Name, f.Usage, f.Default, f.Bool, f.Multi})
		}
		doc.Commands = append(doc.Commands, hc)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(doc)
	w.Write(buf.Bytes())
}
