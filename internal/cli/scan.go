package cli

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/smrit-ai/cli/internal/scan"
)

// scanOutput is a scan's result, as JSON or as one line per file.
type scanOutput struct {
	scan.Result
}

func (o scanOutput) String() string {
	var b strings.Builder
	roots := make([]string, len(o.Roots))
	for i, r := range o.Roots {
		roots[i] = r.Path
	}
	fmt.Fprintf(&b, "%d files in %s\n", len(o.Files), strings.Join(roots, ", "))
	for _, f := range o.Files {
		name := f.Path
		if len(o.Roots) > 1 {
			name = path.Join(f.Root, f.Path)
		}
		line := fmt.Sprintf("%s  %s  %s", name, f.Kind, f.Git.State)
		if len(f.Flags) > 0 {
			line += "  " + strings.Join(f.Flags, ", ")
		}
		b.WriteString(line + "\n")
	}
	for _, s := range o.Skipped {
		fmt.Fprintf(&b, "Skipped %s: %s\n", s.Path, s.Reason)
	}
	if o.Truncated {
		fmt.Fprintf(&b, "Stopped at %d files; scan a smaller folder to see the rest.\n", len(o.Files))
	}
	for _, n := range o.Notes {
		b.WriteString(n + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func scanCommand() Command {
	return Command{
		Name:    "scan",
		Summary: "List the files in folders with their git state, links and flags, never their contents: smrit scan <path>...",
		Run: func(ctx context.Context, env *Env, args []string) int {
			if len(args) == 0 {
				return usageError(env.Stderr, "Usage: smrit scan <path>...")
			}
			res, err := scan.Run(ctx, args, scan.Options{})
			if err != nil {
				var re *scan.RefusedError
				var ue *scan.UsageError
				switch {
				case errors.As(err, &re):
					fmt.Fprintln(env.Stderr, re.Error())
					return ExitRefused
				case errors.As(err, &ue):
					return usageError(env.Stderr, "%s", ue.Error())
				case ctx.Err() != nil:
					fmt.Fprintln(env.Stderr, "The scan was stopped before it finished.")
					return ExitPartial
				}
				fmt.Fprintf(env.Stderr, "The scan failed: %v\n", err)
				return ExitPartial
			}
			env.Result(scanOutput{res})
			// Some files were left out: past the limit, or unreadable.
			if res.Truncated {
				return ExitPartial
			}
			for _, s := range res.Skipped {
				if s.Reason == scan.SkipUnreadable {
					return ExitPartial
				}
			}
			return ExitOK
		},
	}
}
