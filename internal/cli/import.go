package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/smrit-ai/cli/internal/importer"
	"github.com/smrit-ai/cli/internal/plan"
	"github.com/smrit-ai/cli/internal/scan"
	"github.com/smrit-ai/cli/internal/state"
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

// runOutput is a real run's report, as JSON or as text naming the
// organization first.
type runOutput struct{ importer.RunReport }

func (o runOutput) String() string {
	var b strings.Builder
	org := o.Organization
	fmt.Fprintf(&b, "Organization: %s (%s)\n", printable(org.Name), printable(org.Slug))
	fmt.Fprintf(&b, "Run: %s\n", o.Run)
	for _, e := range o.Entries {
		line := fmt.Sprintf("%s  %s  %s", printable(e.Path), e.Kind, e.Status)
		if e.ID != "" {
			line += "  " + printable(e.ID)
		}
		if e.Error != "" {
			line += ": " + printable(e.Error)
		}
		b.WriteString(line + "\n")
	}
	for _, l := range o.Links {
		fmt.Fprintf(&b, "  link in %s to %s: %s\n", printable(l.Path), printable(l.Target), printable(l.Reason))
	}
	if o.Map != "" {
		fmt.Fprintf(&b, "Import map: %s\n", printable(o.Map))
	}
	if o.MapError != "" {
		fmt.Fprintf(&b, "Import map: %s\n", printable(o.MapError))
	}
	exists := ""
	if o.Exists > 0 {
		exists = fmt.Sprintf(" %d already in Smrit,", o.Exists)
	}
	fmt.Fprintf(&b, "%d created, %d updated, %d unchanged,%s %d failed, %d refused, %d skipped.",
		o.Created, o.Updated, o.Unchanged, exists, o.Failed, o.Refused, o.Skipped)
	return b.String()
}

const importUsage = "Usage: smrit import <plan> [--dry-run | --resume <run>]"

func importCommand(s *Services) Command {
	return Command{
		Name:    "import",
		Summary: "Import a plan into Smrit, or preview it with --dry-run, which writes nothing.",
		Flags: []Flag{
			orgFlag,
			{Name: "dry-run", Usage: "Preview what the plan would do. Nothing is written.", Bool: true},
			{Name: "resume", Usage: "Carry on the run with this id, skipping what it already imported."},
		},
		Run: func(ctx context.Context, env *Env, args []string) int {
			if len(args) != 1 {
				return usageError(env.Stderr, importUsage)
			}
			dry, resume := env.Flags["dry-run"] == "true", env.Flags["resume"]
			if dry && resume != "" {
				return usageError(env.Stderr, "A dry run writes nothing, so it has no run to resume. Use --dry-run or --resume, not both.")
			}
			p, planHash, err := plan.LoadHashed(args[0])
			if err != nil {
				return usageError(env.Stderr, "%v", err)
			}
			if resume != "" {
				if dir, err := state.Dir(); err == nil && state.RunPath(dir, resume) == "" {
					return usageError(env.Stderr, "No import run has that id. The id is printed when a run begins.")
				}
			}
			c, code := s.client(env)
			if c == nil {
				return code
			}
			if dry {
				rep, err := importer.DryRun(ctx, importer.Input{Plan: p, MCP: c, Route: &importer.Route{MCP: c}})
				if err != nil {
					return importError(env, err)
				}
				env.Result(importOutput{rep})
				return refusedExit(rep.Refused, 0, len(rep.Entries))
			}
			dir, err := state.Ensure()
			if err != nil {
				return report(env, err)
			}
			rep, err := importer.Run(ctx, p, importer.RunOptions{
				MCP: c, Route: &importer.Route{MCP: c}, StateDir: dir, PlanSHA256: planHash, Resume: resume,
				Began: func(org importer.Org, run string) {
					fmt.Fprintf(env.Stderr, "Importing into %s (%s) as run %s.\n", printable(org.Name), printable(org.Slug), run)
				},
			})
			if err != nil {
				// What was done before the stop is reported first, with the
				// run's id, so nothing done is lost from view.
				if rep.Run != "" {
					env.Result(runOutput{rep})
				}
				code := importError(env, err)
				if rep.Run != "" {
					fmt.Fprintf(env.Stderr, "The run %s stopped. To carry on, run smrit import %s --resume %s\n", rep.Run, printable(args[0]), rep.Run)
				}
				return code
			}
			env.Result(runOutput{rep})
			return refusedExit(rep.Refused, rep.Failed+rep.Skipped+rep.Exists, len(rep.Entries))
		},
	}
}

// importError reports an error from a dry run or a run with its exit code.
func importError(env *Env, err error) int {
	var re *scan.RefusedError
	var ue *scan.UsageError
	var se *importer.ResumeError
	switch {
	case errors.As(err, &re):
		fmt.Fprintln(env.Stderr, re.Error())
		return ExitRefused
	case errors.As(err, &ue):
		return usageError(env.Stderr, "%s", ue.Error())
	case errors.As(err, &se):
		return usageError(env.Stderr, "%s", se.Error())
	}
	return report(env, err)
}

// refusedExit is 4 when every entry was refused, 1 when some were refused or
// not done, and 0 when every entry was.
func refusedExit(refused, notDone, entries int) int {
	switch {
	case refused > 0 && refused == entries:
		return ExitRefused
	case refused > 0 || notDone > 0:
		return ExitPartial
	}
	return ExitOK
}
