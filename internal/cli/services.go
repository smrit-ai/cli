package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/smrit-ai/cli/internal/auth"
	"github.com/smrit-ai/cli/internal/importer"
	"github.com/smrit-ai/cli/internal/mcp"
	"github.com/smrit-ai/cli/internal/profile"
	"github.com/smrit-ai/cli/internal/server"
	"github.com/smrit-ai/cli/internal/upgrade"
	"github.com/smrit-ai/cli/internal/version"
)

// Services is everything a command reaches outside the process through, so a
// test can stand a fake in for each. DefaultServices wires the real ones.
type Services struct {
	// Origin is the compiled server address.
	Origin string
	HTTP   *http.Client
	Now    func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error

	Token func(ctx context.Context, p profile.Profile) (string, error)
	// Refresh forces a new token after the server refused one, refused. Nil
	// in tests that never see a 401.
	Refresh     func(ctx context.Context, p profile.Profile, refused string) (string, error)
	Login       func(ctx context.Context, o auth.LoginOptions) (profile.Profile, error)
	Logout      func(ctx context.Context, p profile.Profile) error
	Headless    func() (string, bool)
	OpenBrowser func(url string) error
	// Upgrader replaces the binary; nil is the compiled release address
	// and the running binary.
	Upgrader *upgrade.Upgrader
}

// DefaultServices is the real wiring.
func DefaultServices() *Services {
	return &Services{
		Origin:      server.Address,
		Token:       auth.AccessToken,
		Refresh:     auth.ForceRefresh,
		Login:       auth.Login,
		Logout:      auth.Logout,
		Headless:    auth.HeadlessToken,
		OpenBrowser: auth.OpenSystemBrowser,
	}
}

// Commands is the command table for the given services.
func Commands(s *Services) []Command {
	return []Command{loginCommand(s), logoutCommand(s), statusCommand(s), callCommand(s), scanCommand(), importCommand(s), retireCommand(), setupCommand(s), upgradeCommand(s)}
}

var orgFlag = Flag{Name: "org", Usage: "The organization to use, when signed in to several."}

// client builds the MCP client for the chosen profile, or for the headless
// token on the bare endpoint. On failure it has already reported and returns
// the exit code.
func (s *Services) client(env *Env) (*mcp.Client, int) {
	c := &mcp.Client{Origin: s.Origin, Version: version.Version, HTTP: s.HTTP, Now: s.Now, Sleep: s.Sleep}
	if tok, ok := s.Headless(); ok {
		if env.Flags["org"] != "" {
			return nil, usageError(env.Stderr, "SMRIT_TOKEN and --org cannot be used together.")
		}
		fmt.Fprintln(env.Stderr, "Using the token in SMRIT_TOKEN.")
		c.Path = "/mcp"
		c.Token = func(context.Context) (string, error) { return tok, nil }
		return c, 0
	}
	p, err := profile.Pick(env.Flags["org"])
	if err != nil {
		return nil, report(env, err)
	}
	c.Path = p.Endpoint
	c.Token = func(ctx context.Context) (string, error) { return s.Token(ctx, p) }
	if s.Refresh != nil {
		c.Refresh = func(ctx context.Context, refused string) (string, error) { return s.Refresh(ctx, p, refused) }
	}
	return c, 0
}

// report writes err as one sentence on stderr and returns its exit code.
func report(env *Env, err error) int {
	fmt.Fprintln(env.Stderr, err.Error())
	return exitCode(err)
}

func exitCode(err error) int {
	var te *mcp.ToolError
	var re *importer.RouteError
	switch {
	case errors.Is(err, profile.ErrSeveralProfiles):
		return ExitUsage
	case errors.Is(err, profile.ErrNoProfile), errors.Is(err, profile.ErrDamagedProfile),
		errors.Is(err, auth.ErrNotSignedIn), errors.Is(err, mcp.ErrSignedOut):
		return ExitNotSignedIn
	case errors.As(err, &te), errors.As(err, &re), errors.Is(err, auth.ErrOrgMismatch), errors.Is(err, auth.ErrLoginFailed):
		return ExitRefused
	case errors.Is(err, mcp.ErrUpgrade):
		return ExitUpgrade
	}
	return ExitUnavailable
}
