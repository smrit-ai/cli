package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/smrit-ai/cli/internal/retire"
	"github.com/smrit-ai/cli/internal/state"
)

// retireOutput is retire's report, as JSON or as counts, the zip's path and
// each file left with why.
type retireOutput struct{ retire.Report }

func (o retireOutput) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Run: %s\nFolder: %s\n", o.Run, printable(o.Root))
	verb := "would be"
	if o.Applied {
		verb = "were"
	}
	fmt.Fprintf(&b, "%d deleted, %d zipped, %d left.\n", len(o.Deleted), len(o.Zipped), len(o.Left))
	if o.Zip != nil {
		fmt.Fprintf(&b, "Zip: %s\n", printable(*o.Zip))
	}
	if len(o.Deleted) > 0 {
		fmt.Fprintf(&b, "Deleted files %s removed from the working tree only; git can restore them.\n", verb)
	}
	if n := len(o.RecoverableUnpushed); n > 0 {
		fmt.Fprintf(&b, "%d of them are only in local commits that are not pushed:\n", n)
		for _, p := range o.RecoverableUnpushed {
			fmt.Fprintf(&b, "  %s\n", printable(p))
		}
	}
	if o.ExcludeAdded {
		fmt.Fprintf(&b, "The zip %s added to the repository's info/exclude, so git does not list it.\n", verb)
	}
	for _, l := range o.Left {
		fmt.Fprintf(&b, "Left %s: %s\n", printable(l.Path), l.Reason)
	}
	writeLeftovers(&b, o.Leftovers, false)
	if !o.Applied {
		fmt.Fprintf(&b, "Nothing was changed. To do this, run smrit retire %s --apply", o.Run)
	}
	return strings.TrimRight(b.String(), "\n")
}

const retireUsage = "Usage: smrit retire <run> [--apply]"

func retireCommand() Command {
	return Command{
		Name:    "retire",
		Summary: "Remove the files a finished import moved into Smrit, zipping any git cannot restore. Without --apply it only reports: smrit retire <run> [--apply]",
		Flags: []Flag{
			{Name: "apply", Usage: "Remove the files. Without it nothing is touched.", Bool: true},
		},
		Run: func(ctx context.Context, env *Env, args []string) int {
			if len(args) != 1 {
				return usageError(env.Stderr, retireUsage)
			}
			dir, err := state.Dir()
			if err != nil {
				fmt.Fprintln(env.Stderr, err.Error())
				return ExitRefused
			}
			rep, err := retire.Run(ctx, args[0], retire.Options{StateDir: dir, Apply: env.Flags["apply"] == "true"})
			var re *retire.RefusedError
			var se *retire.StoppedError
			switch {
			case err == nil:
				env.Result(retireOutput{rep})
				return ExitOK
			case errors.As(err, &re):
				fmt.Fprintln(env.Stderr, re.Error())
				return ExitRefused
			case errors.As(err, &se):
				// What was removed and what is still in place, first.
				env.Result(retireOutput{rep})
				fmt.Fprintln(env.Stderr, printable(se.Error()))
				return ExitPartial
			}
			fmt.Fprintln(env.Stderr, printable(err.Error()))
			return ExitPartial
		},
	}
}
