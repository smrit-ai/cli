package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/smrit-ai/cli/internal/auth"
	"github.com/smrit-ai/cli/internal/mcp"
	"github.com/smrit-ai/cli/internal/version"
)

type orgInfo struct {
	Slug string `json:"slug"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (o orgInfo) String() string {
	if o.Name == "" || o.Name == o.Slug {
		return o.Slug
	}
	return fmt.Sprintf("%s (%s)", o.Name, o.Slug)
}

type loginResult struct {
	SignedIn     bool    `json:"signed_in"`
	Organization orgInfo `json:"organization"`
}

func (r loginResult) String() string { return "Signed in to " + r.Organization.String() + "." }

// startOrg reads the organization out of a start result.
func startOrg(raw json.RawMessage) (orgInfo, error) {
	var r struct {
		Organization orgInfo `json:"organization"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || r.Organization.Slug == "" || r.Organization.ID == "" {
		return orgInfo{}, errors.New("Smrit did not say which organization you signed in to.")
	}
	return r.Organization, nil
}

func loginCommand(s *Services) Command {
	return Command{
		Name:    "login",
		Summary: "Sign in through your browser.",
		Flags:   []Flag{orgFlag},
		Run: func(ctx context.Context, env *Env, args []string) int {
			if len(args) > 0 {
				return usageError(env.Stderr, "smrit login takes no arguments.")
			}
			var org orgInfo
			opts := auth.LoginOptions{
				Org:    env.Flags["org"],
				Notice: env.Stderr,
				OpenBrowser: func(url string) error {
					// Shown first so a person without a browser opener can paste it.
					fmt.Fprintf(env.Stderr, "Opening your browser to sign in. If it does not open, visit:\n%s\n", url)
					return s.OpenBrowser(url)
				},
				Verify: func(ctx context.Context, accessToken string) (string, string, error) {
					c := &mcp.Client{Origin: s.Origin, Path: "/mcp", Version: version.Version, HTTP: s.HTTP, Now: s.Now, Sleep: s.Sleep,
						Token: func(context.Context) (string, error) { return accessToken, nil }}
					raw, err := c.Call(ctx, "start", nil)
					if err != nil {
						return "", "", err
					}
					o, err := startOrg(raw)
					if err != nil {
						return "", "", err
					}
					org = o
					return o.Slug, o.ID, nil
				},
			}
			p, err := s.Login(ctx, opts)
			if err != nil {
				return report(env, err)
			}
			if org.Slug == "" {
				org = orgInfo{Slug: p.Org, ID: p.OrgID}
			}
			env.Result(loginResult{SignedIn: true, Organization: org})
			return ExitOK
		},
	}
}
