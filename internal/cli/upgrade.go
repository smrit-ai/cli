package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/smrit-ai/cli/internal/server"
	"github.com/smrit-ai/cli/internal/upgrade"
	"github.com/smrit-ai/cli/internal/version"
)

// upgradeOutput is an upgrade's or a check's status, as JSON or a sentence.
type upgradeOutput struct {
	upgrade.Status
	check bool
}

func (o upgradeOutput) String() string {
	switch {
	case o.UpToDate:
		return fmt.Sprintf("smrit %s is the latest release.", o.Current)
	case o.check:
		return fmt.Sprintf("smrit %s is installed and the latest release is %s. Run smrit upgrade to install it.", o.Current, o.Latest)
	}
	return fmt.Sprintf("Upgraded smrit at %s from %s to %s.", printable(o.Path), o.Current, o.Latest)
}

func upgradeCommand(s *Services) Command {
	return Command{
		Name:    "upgrade",
		Summary: "Replace this smrit with the latest release, verified against its checksum. A copy a package manager installed is left to that package manager.",
		Flags: []Flag{
			{Name: "check", Usage: "Only report the latest version and this one. Nothing is downloaded.", Bool: true},
		},
		Run: func(ctx context.Context, env *Env, args []string) int {
			if len(args) != 0 {
				return usageError(env.Stderr, "Usage: smrit upgrade [--check]")
			}
			u := s.Upgrader
			if u == nil {
				u = &upgrade.Upgrader{Base: server.ReleaseBase, Current: version.Version, Executable: os.Executable}
			}
			check := env.Flags["check"] == "true"
			var st upgrade.Status
			var err error
			if check {
				st, err = u.Check(ctx)
			} else {
				st, err = u.Run(ctx)
			}
			if err != nil {
				fmt.Fprintln(env.Stderr, err.Error())
				var re *upgrade.RefusedError
				if errors.As(err, &re) {
					return ExitRefused
				}
				return ExitUnavailable
			}
			env.Result(upgradeOutput{Status: st, check: check})
			return ExitOK
		},
	}
}
