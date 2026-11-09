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
	You          json.RawMessage `json:"you"` // memory.you; null without the memory feature
	Spaces       json.RawMessage `json:"spaces"`
	Version      string          `json:"version"`
}

// youName is the name in memory.you, or the raw JSON when it has none.
func youName(raw json.RawMessage) string {
	var y struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &y) == nil && y.Name != "" {
		return y.Name
	}
	return string(raw)
}

func (r statusResult) String() string {
	var o orgInfo
	json.Unmarshal(r.Organization, &o)
	var spaces []string
	json.Unmarshal(r.Spaces, &spaces)
	var b strings.Builder
	fmt.Fprintf(&b, "Signed in to %s\n", o)
	if len(r.You) > 0 && string(r.You) != "null" {
		fmt.Fprintf(&b, "You: %s\n", youName(r.You))
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
			var mem struct {
				You json.RawMessage `json:"you"`
			}
			json.Unmarshal(m["memory"], &mem) // absent without the memory feature
			you := mem.You
			if string(you) == "null" {
				you = nil
			}
			env.Result(statusResult{SignedIn: true, Organization: m["organization"], You: you, Spaces: m["spaces"], Version: version.Version})
			return ExitOK
		},
	}
}
