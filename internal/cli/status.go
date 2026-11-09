package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/smrit-ai/cli/internal/version"
)

type statusResult struct {
	SignedIn     bool            `json:"signed_in"`
	Organization json.RawMessage `json:"organization"`
	You          json.RawMessage `json:"you"`
	Spaces       json.RawMessage `json:"spaces"`
	Version      string          `json:"version"`
}

func (r statusResult) String() string {
	var o orgInfo
	json.Unmarshal(r.Organization, &o)
	var spaces []string
	json.Unmarshal(r.Spaces, &spaces)
	var b strings.Builder
	fmt.Fprintf(&b, "Signed in to %s\n", o)
	if len(r.You) > 0 && string(r.You) != "null" {
		fmt.Fprintf(&b, "You: %s\n", r.You)
	}
	fmt.Fprintf(&b, "Spaces: %s\n", strings.Join(spaces, ", "))
	fmt.Fprintf(&b, "smrit %s", r.Version)
	return b.String()
}

func statusCommand(s *Services) Command {
	return Command{
		Name:    "status",
		Summary: "Show who you are signed in as and what you can see.",
		Flags:   []Flag{orgFlag},
		Run: func(ctx context.Context, env *Env, args []string) int {
			if len(args) > 0 {
				return usageError(env.Stderr, "smrit status takes no arguments.")
			}
			c, code := s.client(env)
			if c == nil {
				return code
			}
			raw, err := c.Call(ctx, "start", nil)
			if err != nil {
				return report(env, err)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return report(env, fmt.Errorf("Smrit sent an answer this CLI cannot read."))
			}
			env.Result(statusResult{SignedIn: true, Organization: m["organization"], You: m["you"], Spaces: m["spaces"], Version: version.Version})
			return ExitOK
		},
	}
}
