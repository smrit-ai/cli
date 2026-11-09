package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ServerName is the name the Smrit entry has in every harness.
const ServerName = "smrit"

// commandTimeout bounds one harness command. claude mcp get checks the
// server's health, which takes a moment, and never this long.
const commandTimeout = 2 * time.Minute

// ErrCommandMissing means the harness's own command is not on PATH.
var ErrCommandMissing = errors.New("command not found")

// Exec runs argv directly, never through a shell, and returns what it wrote
// and its exit code. A command that is not on PATH is ErrCommandMissing.
type Exec func(ctx context.Context, argv []string) (stdout, stderr string, code int, err error)

// SystemExec is the real Exec.
func SystemExec(ctx context.Context, argv []string) (string, string, int, error) {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return "", "", -1, ErrCommandMissing
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, argv[1:]...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.String(), errb.String(), ee.ExitCode(), nil
	}
	if err != nil {
		return "", "", -1, err
	}
	return out.String(), errb.String(), 0, nil
}

// entry is what a harness says about its smrit server.
type entry struct {
	present bool
	url     string
	// scope is Claude Code's: user, local or project.
	scope    string
	disabled bool
	// missing means the harness's command is not on PATH.
	missing bool
}

// harness is one of the harnesses setup changes for itself.
type harness struct {
	id, name, bin string
	// settings is the file the allow rule goes in, "" for none.
	settings func(c *Config) string
	allow    string
	// global is the instruction file read in every project.
	global      func(c *Config) string
	projectFile string
	inspect     func(ctx context.Context, c *Config) (entry, error)
	add         func(url, scope string) []string
	// remove is nil when add replaces an entry by itself.
	remove   func(e entry) ([]string, error)
	approval func(c *Config) string
}

// Harnesses lists the ids setup changes by itself.
var Harnesses = []string{"claude-code", "codex", "antigravity"}

func builtin(id string) (*harness, bool) {
	switch id {
	case "claude-code":
		return claudeCode, true
	case "codex":
		return codex, true
	case "antigravity":
		return antigravity, true
	}
	return nil, false
}

// absEnv is the value of an environment variable when it is an absolute
// path, and "" otherwise.
func absEnv(c *Config, name string) string {
	if v := c.Getenv(name); v != "" && filepath.IsAbs(v) {
		return filepath.Clean(v)
	}
	return ""
}

func claudeDir(c *Config) string {
	if d := absEnv(c, "CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	return filepath.Join(c.Home, ".claude")
}

func codexHome(c *Config) string {
	if d := absEnv(c, "CODEX_HOME"); d != "" {
		return d
	}
	return filepath.Join(c.Home, ".codex")
}

func failed(argv []string, code int) error {
	return &Refused{fmt.Sprintf("%s failed with exit %d, so setup cannot tell how the smrit server is set up. Run it yourself to see why.", strings.Join(argv, " "), code)}
}

func ran(c *Config, ctx context.Context, argv []string) (string, string, int, error) {
	out, errs, code, err := c.Exec(ctx, argv)
	if err != nil && !errors.Is(err, ErrCommandMissing) {
		return "", "", -1, &Refused{fmt.Sprintf("Could not run %s: %v", argv[0], err)}
	}
	return out, errs, code, err
}

func noServer(s string) bool { return strings.Contains(strings.ToLower(s), "no mcp server") }

var claudeCode = &harness{
	id: "claude-code", name: "Claude Code", bin: "claude",
	settings:    func(c *Config) string { return filepath.Join(claudeDir(c), "settings.json") },
	allow:       "mcp__smrit",
	global:      func(c *Config) string { return filepath.Join(claudeDir(c), "CLAUDE.md") },
	projectFile: "CLAUDE.md",
	inspect: func(ctx context.Context, c *Config) (entry, error) {
		argv := []string{"claude", "mcp", "get", ServerName}
		out, errs, code, err := ran(c, ctx, argv)
		if errors.Is(err, ErrCommandMissing) {
			return entry{missing: true}, nil
		} else if err != nil {
			return entry{}, err
		}
		if code != 0 {
			if noServer(out + errs) {
				return entry{}, nil
			}
			return entry{}, failed(argv, code)
		}
		e := entry{present: true}
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(line, "URL:"); ok && e.url == "" {
				e.url = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(line, "Scope:"); ok && e.scope == "" {
				v = strings.ToLower(v)
				switch {
				case strings.Contains(v, "user"):
					e.scope = "user"
				case strings.Contains(v, "local"):
					e.scope = "local"
				case strings.Contains(v, "project"):
					e.scope = "project"
				}
			}
		}
		return e, nil
	},
	add: func(url, scope string) []string {
		if scope == "" {
			scope = "user"
		}
		return []string{"claude", "mcp", "add", "--transport", "http", "--scope", scope, ServerName, url}
	},
	remove: func(e entry) ([]string, error) {
		switch e.scope {
		case "project":
			return nil, &Refused{"Claude Code's smrit server comes from this project's .mcp.json, which other people share, so setup does not change it. Change its address there, then run setup again."}
		case "local":
			return []string{"claude", "mcp", "remove", "--scope", "local", ServerName}, nil
		}
		return []string{"claude", "mcp", "remove", "--scope", "user", ServerName}, nil
	},
	approval: func(c *Config) string {
		return fmt.Sprintf("Restart Claude Code, run /mcp, select smrit and sign in, checking that you approve %s.", c.Org)
	},
}

var codex = &harness{
	id: "codex", name: "Codex", bin: "codex",
	global: func(c *Config) string {
		home := codexHome(c)
		override := filepath.Join(home, "AGENTS.override.md")
		if b, err := os.ReadFile(override); err == nil && len(bytes.TrimSpace(b)) > 0 {
			return override
		}
		return filepath.Join(home, "AGENTS.md")
	},
	projectFile: "AGENTS.md",
	inspect: func(ctx context.Context, c *Config) (entry, error) {
		argv := []string{"codex", "mcp", "get", ServerName, "--json"}
		out, errs, code, err := ran(c, ctx, argv)
		if errors.Is(err, ErrCommandMissing) {
			return entry{missing: true}, nil
		} else if err != nil {
			return entry{}, err
		}
		type server struct {
			Name      string `json:"name"`
			Enabled   *bool  `json:"enabled"`
			Transport struct {
				URL string `json:"url"`
			} `json:"transport"`
		}
		from := func(s server) entry {
			return entry{present: true, url: s.Transport.URL, disabled: s.Enabled != nil && !*s.Enabled}
		}
		if code == 0 {
			var s server
			if json.Unmarshal([]byte(out), &s) != nil {
				return entry{}, &Refused{"codex mcp get smrit --json answered in a shape setup cannot read, so it cannot tell how the smrit server is set up."}
			}
			return from(s), nil
		}
		if noServer(out + errs) {
			return entry{}, nil
		}
		// An older codex without get --json: the list, as JSON.
		listArgv := []string{"codex", "mcp", "list", "--json"}
		out, _, code, err = ran(c, ctx, listArgv)
		if err != nil {
			return entry{}, err
		}
		var all []server
		if code != 0 || json.Unmarshal([]byte(out), &all) != nil {
			return entry{}, failed(argv, code)
		}
		for _, s := range all {
			if s.Name == ServerName {
				return from(s), nil
			}
		}
		return entry{}, nil
	},
	add: func(url, _ string) []string {
		return []string{"codex", "mcp", "add", ServerName, "--url", url}
	},
	remove: func(entry) ([]string, error) {
		return []string{"codex", "mcp", "remove", ServerName}, nil
	},
	approval: func(c *Config) string {
		return fmt.Sprintf("Run codex mcp login smrit --scopes read,write and sign in, checking that you approve %s. Then start a new Codex session.", c.Org)
	},
}

var antigravity = &harness{
	id: "antigravity", name: "Antigravity", bin: "agy",
	settings: func(c *Config) string {
		return filepath.Join(c.Home, ".gemini", "antigravity-cli", "settings.json")
	},
	allow:       "mcp(smrit/*)",
	global:      func(c *Config) string { return filepath.Join(c.Home, ".gemini", "config", "AGENTS.md") },
	projectFile: "AGENTS.md",
	inspect: func(ctx context.Context, c *Config) (entry, error) {
		argv := []string{"agy", "mcp", "list"}
		out, _, code, err := ran(c, ctx, argv)
		if errors.Is(err, ErrCommandMissing) {
			return entry{missing: true}, nil
		} else if err != nil {
			return entry{}, err
		}
		if code != 0 {
			return entry{}, failed(argv, code)
		}
		// NAME TYPE STATUS COMMAND/URL, one server a line.
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) == 0 || f[0] != ServerName {
				continue
			}
			e := entry{present: true}
			for _, v := range f[1:] {
				if strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://") {
					e.url = v
				}
				if v == "disabled" {
					e.disabled = true
				}
			}
			return e, nil
		}
		return entry{}, nil
	},
	// agy mcp add adds or updates, so a replacement is one command.
	add: func(url, _ string) []string {
		return []string{"agy", "mcp", "add", ServerName, url}
	},
	approval: func(c *Config) string {
		return fmt.Sprintf("Start a new agy session and sign in to Smrit when it asks, checking that you approve %s. agy mcp list shows the server.", c.Org)
	},
}
