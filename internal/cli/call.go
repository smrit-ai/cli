package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var toolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// parseArgs builds the arguments object: --json-args first, then each --arg
// over it. A value is JSON when it parses as JSON and a string otherwise.
func parseArgs(jsonArgs string, pairs []string) (map[string]any, error) {
	args := map[string]any{}
	if jsonArgs != "" {
		if err := json.Unmarshal([]byte(jsonArgs), &args); err != nil || args == nil {
			return nil, fmt.Errorf("--json-args must be one JSON object, like '{\"document_id\":\"abc\"}'.")
		}
	}
	for _, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--arg takes name=value, and %q is not that.", kv)
		}
		var parsed any
		if err := json.Unmarshal([]byte(v), &parsed); err != nil {
			parsed = v
		}
		args[k] = parsed
	}
	return args, nil
}

func callCommand(s *Services) Command {
	return Command{
		Name:    "call",
		Summary: "Call any Smrit tool: smrit call <tool> [--arg name=value] [--json-args '{...}'].",
		Flags: []Flag{
			orgFlag,
			{Name: "arg", Usage: "A tool argument as name=value. Repeat it. JSON values are sent as JSON.", Multi: true},
			{Name: "json-args", Usage: "The tool arguments as one JSON object; each --arg overrides it."},
		},
		Run: func(ctx context.Context, env *Env, args []string) int {
			if len(args) != 1 || !toolName.MatchString(args[0]) {
				return usageError(env.Stderr, "Usage: smrit call <tool> [--arg name=value] [--json-args '{...}']")
			}
			toolArgs, err := parseArgs(env.Flags["json-args"], env.Lists["arg"])
			if err != nil {
				return usageError(env.Stderr, "%v", err)
			}
			c, code := s.client(env)
			if c == nil {
				return code
			}
			raw, err := c.Call(ctx, args[0], toolArgs)
			if err != nil {
				return report(env, err)
			}
			if env.JSON {
				env.Result(raw)
				return ExitOK
			}
			var v any
			if json.Unmarshal(raw, &v) == nil {
				if str, ok := v.(string); ok {
					env.Result(str)
					return ExitOK
				}
			}
			env.Result(raw)
			return ExitOK
		},
	}
}
