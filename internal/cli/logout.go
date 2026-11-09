package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/smrit-ai/cli/internal/auth"
	"github.com/smrit-ai/cli/internal/profile"
)

type logoutResult struct {
	SignedIn     bool   `json:"signed_in"`
	Organization string `json:"organization"`
}

func (r logoutResult) String() string { return "Signed out of " + r.Organization + "." }

func logoutCommand(s *Services) Command {
	return Command{
		Name:    "logout",
		Summary: "Sign out and forget the sign in on this machine.",
		Flags:   []Flag{orgFlag},
		Run: func(ctx context.Context, env *Env, args []string) int {
			if len(args) > 0 {
				return usageError(env.Stderr, "smrit logout takes no arguments.")
			}
			p, err := profile.Pick(env.Flags["org"])
			if err != nil {
				return report(env, err)
			}
			if err := s.Logout(ctx, p); err != nil {
				if !errors.Is(err, auth.ErrNotRevoked) {
					return report(env, err)
				}
				fmt.Fprintln(env.Stderr, err.Error())
			}
			env.Result(logoutResult{SignedIn: false, Organization: p.Org})
			return ExitOK
		},
	}
}
