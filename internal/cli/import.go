package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/smrit-ai/cli/internal/importer"
	"github.com/smrit-ai/cli/internal/plan"
	"github.com/smrit-ai/cli/internal/scan"
)

// importOutput is a dry run's report, as JSON or as text naming the
// organization first.
type importOutput struct{ importer.Report }

func (o importOutput) String() string {
	var b strings.Builder
	org := o.Organization
	fmt.Fprintf(&b, "Organization: %s (%s)\n", printable(org.Name), printable(org.Slug))
	for _, e := range o.Entries {
		line := fmt.Sprintf("%s  %s  %s", printable(e.Path), e.Kind, e.Action)
		if e.Space != "" {
			line += "  " + printable(e.Space)
		}
		if e.Action == importer.ActionRefused {
			line += ": " + printable(e.Reason)
		}
		if e.TypeFrom != "" {
			line += fmt.Sprintf("  (type %s is not in this organization's list, used %s)", printable(e.TypeFrom), printable(e.Type))
		}
		b.WriteString(line + "\n")
		for _, s := range e.Similar {
			fmt.Fprintf(&b, "  similar: %s (%s)\n", printable(s.Title), printable(s.Slug))
		}
	}
	fmt.Fprintf(&b, "%d would be created, %d updated, %d unchanged, %d refused. Nothing was written.",
		o.WouldCreate, o.WouldUpdate, o.Unchanged, o.Refused)
	return b.String()
}

func importCommand(s *Services) Command {
	return Command{
		Name:    "import",
		Summary: "Check an import plan and preview it without writing anything: smrit import <plan> --dry-run.",
		Flags: []Flag{
			orgFlag,
			{Name: "dry-run", Usage: "Preview what the plan would do. Nothing is written.", Bool: true},
		},
		Run: func(ctx context.Context, env *Env, args []string) int {
			if len(args) != 1 {
				return usageError(env.Stderr, "Usage: smrit import <plan> --dry-run")
			}
			if env.Flags["dry-run"] != "true" {
				return usageError(env.Stderr, "Importing for real comes in a later version. Run smrit import <plan> --dry-run to preview.")
			}
			p, err := plan.Load(args[0])
			if err != nil {
				return usageError(env.Stderr, "%v", err)
			}
			c, code := s.client(env)
			if c == nil {
				return code
			}
			rep, err := importer.DryRun(ctx, importer.Input{Plan: p, MCP: c, Route: &importer.Route{MCP: c}})
			if err != nil {
				var re *scan.RefusedError
				var ue *scan.UsageError
				switch {
				case errors.As(err, &re):
					fmt.Fprintln(env.Stderr, re.Error())
					return ExitRefused
				case errors.As(err, &ue):
					return usageError(env.Stderr, "%s", ue.Error())
				}
				return report(env, err)
			}
			env.Result(importOutput{rep})
			if rep.Refused > 0 {
				return ExitPartial
			}
			return ExitOK
		},
	}
}
