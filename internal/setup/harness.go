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
	"sort"
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

// Exec runs argv in dir ("" for the current folder), directly, never through
// a shell, and returns what it wrote and its exit code. A command that is
// not on PATH is ErrCommandMissing.
type Exec func(ctx context.Context, dir string, argv []string) (stdout, stderr string, code int, err error)

// SystemExec is the real Exec.
func SystemExec(ctx context.Context, dir string, argv []string) (string, string, int, error) {
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return "", "", -1, ErrCommandMissing
	}
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, argv[1:]...)
	cmd.Dir = dir
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
	// extras names the settings the entry has beyond its address, such as
	// headers or an environment variable. An entry with any is never
	// removed or replaced, because they would be lost.
	extras []string
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
	// extras reads settings the inspecting command does not show; nil when
	// it shows them all.
	extras func(c *Config) ([]string, error)
	add    func(url, scope string) []string
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
	return &Refused{fmt.Sprintf("%s failed with exit %d, so setup cannot tell how the smrit server is set up and changes nothing. Run it yourself to see why.", strings.Join(argv, " "), code)}
}

// unknown is the refusal for output setup does not recognise: it never
// guesses that an entry is absent, because adding one would overwrite it.
func unknown(argv []string) error {
	return &Refused{fmt.Sprintf("%s answered in a form setup does not recognise, so it cannot tell how the smrit server is set up and changes nothing. Check that entry yourself, or upgrade smrit.", strings.Join(argv, " "))}
}

func ran(c *Config, ctx context.Context, argv []string) (string, string, int, error) {
	out, errs, code, err := c.Exec(ctx, c.WorkDir, argv)
	if err != nil && !errors.Is(err, ErrCommandMissing) {
		return "", "", -1, &Refused{fmt.Sprintf("Could not run %s: %v", argv[0], err)}
	}
	return out, errs, code, err
}

var claudeCode = &harness{
	id: "claude-code", name: "Claude Code", bin: "claude",
	settings:    func(c *Config) string { return filepath.Join(claudeDir(c), "settings.json") },
	allow:       "mcp__smrit",
	global:      func(c *Config) string { return filepath.Join(claudeDir(c), "CLAUDE.md") },
	projectFile: "CLAUDE.md",
	inspect:     inspectClaude,
	add: func(url, scope string) []string {
		if scope == "" {
			scope = "user"
		}
		return []string{"claude", "mcp", "add", "--transport", "http", "--scope", scope, ServerName, url}
	},
	remove: func(e entry) ([]string, error) {
		switch e.scope {
		case "project":
			return nil, &Refused{"Claude Code's smrit server comes from this project's .mcp.json, which other people share, so setup does not change it. Change its address there by hand, then run setup again."}
		case "local":
			return []string{"claude", "mcp", "remove", "--scope", "local", ServerName}, nil
		case "user":
			return []string{"claude", "mcp", "remove", "--scope", "user", ServerName}, nil
		}
		return nil, &Refused{"claude mcp get smrit did not say which scope the smrit server is in, so setup does not remove it. Change its address by hand, then run setup again."}
	},
	approval: func(c *Config) string {
		return fmt.Sprintf("Restart Claude Code, run /mcp, select smrit and sign in, checking that you approve %s.", c.Org)
	},
}

// inspectClaude reads claude mcp get smrit: on success a "smrit:" line,
// then indented "Key: value" lines up to a blank line. Scope, Status, Type,
// URL and Issue are known; any other key is a setting beyond the address.
func inspectClaude(ctx context.Context, c *Config) (entry, error) {
	argv := []string{"claude", "mcp", "get", ServerName}
	out, errs, code, err := ran(c, ctx, argv)
	if errors.Is(err, ErrCommandMissing) {
		return entry{missing: true}, nil
	} else if err != nil {
		return entry{}, err
	}
	if code != 0 {
		if code == 1 && strings.HasPrefix(strings.TrimSpace(out+errs), `No MCP server named "smrit"`) {
			return entry{}, nil
		}
		return entry{}, failed(argv, code)
	}
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i == len(lines) || strings.TrimSpace(lines[i]) != ServerName+":" {
		return entry{}, unknown(argv)
	}
	e := entry{present: true}
	known := map[string]bool{"Scope": true, "Status": true, "Type": true, "URL": true, "Issue": true}
	extras := map[string]bool{}
	for _, line := range lines[i+1:] {
		if strings.TrimSpace(line) == "" {
			break
		}
		if line == strings.TrimLeft(line, " \t") {
			return entry{}, unknown(argv)
		}
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || key == "" {
			return entry{}, unknown(argv)
		}
		value = strings.TrimSpace(value)
		switch {
		case !known[key]:
			extras[key] = true
		case key == "URL":
			e.url = value
		case key == "Scope":
			v := strings.ToLower(value)
			switch {
			case strings.HasPrefix(v, "user"):
				e.scope = "user"
			case strings.HasPrefix(v, "local"):
				e.scope = "local"
			case strings.HasPrefix(v, "project"):
				e.scope = "project"
			}
		}
	}
	if e.url == "" {
		return entry{}, &Refused{"claude mcp get smrit shows no address for the smrit server, so it is not Smrit's and setup leaves it alone. Rename or remove it in Claude Code, then run setup again."}
	}
	e.extras = sortedKeys(extras)
	return e, nil
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
	inspect:     inspectCodex,
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

// inspectCodex reads codex mcp get smrit --json, or, from a codex that does
// not know --json there, the smrit item of codex mcp list --json.
func inspectCodex(ctx context.Context, c *Config) (entry, error) {
	argv := []string{"codex", "mcp", "get", ServerName, "--json"}
	out, errs, code, err := ran(c, ctx, argv)
	if errors.Is(err, ErrCommandMissing) {
		return entry{missing: true}, nil
	} else if err != nil {
		return entry{}, err
	}
	if code == 0 {
		return codexEntry([]byte(out), argv)
	}
	said := strings.TrimSpace(out + errs)
	if strings.Contains(said, "No MCP server named 'smrit' found") {
		return entry{}, nil
	}
	if !strings.Contains(said, "'--json'") {
		return entry{}, failed(argv, code)
	}
	listArgv := []string{"codex", "mcp", "list", "--json"}
	out, _, code, err = ran(c, ctx, listArgv)
	if err != nil {
		return entry{}, err
	}
	if code != 0 {
		return entry{}, failed(listArgv, code)
	}
	var all []json.RawMessage
	if json.Unmarshal([]byte(out), &all) != nil || all == nil {
		return entry{}, unknown(listArgv)
	}
	for _, raw := range all {
		var named struct {
			Name *string `json:"name"`
		}
		if json.Unmarshal(raw, &named) != nil || named.Name == nil {
			return entry{}, unknown(listArgv)
		}
		if *named.Name == ServerName {
			return codexEntry(raw, listArgv)
		}
	}
	return entry{}, nil
}

// codexEntry reads one server as codex writes it in JSON. Keys it knows
// carry no setting of their own; any other key with a value, and any
// transport key but type and url with a value, is a setting beyond the
// address.
func codexEntry(raw []byte, argv []string) (entry, error) {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || top == nil {
		return entry{}, unknown(argv)
	}
	var name string
	var transport map[string]json.RawMessage
	if json.Unmarshal(top["name"], &name) != nil || name != ServerName ||
		json.Unmarshal(top["transport"], &transport) != nil || transport == nil {
		return entry{}, unknown(argv)
	}
	var kind, url string
	json.Unmarshal(transport["type"], &kind)
	if kind != "streamable_http" {
		return entry{}, &Refused{fmt.Sprintf("Codex's smrit server is not an http server (%s), so it is not Smrit's and setup leaves it alone. Rename or remove it in Codex, then run setup again.", quoted(kind))}
	}
	if json.Unmarshal(transport["url"], &url) != nil || url == "" {
		return entry{}, unknown(argv)
	}
	e := entry{present: true, url: url}
	var enabled bool
	if json.Unmarshal(top["enabled"], &enabled) == nil && !enabled && top["enabled"] != nil {
		e.disabled = true
	}
	extras := map[string]bool{}
	plain := map[string]bool{"name": true, "enabled": true, "disabled_reason": true, "transport": true, "auth_status": true}
	for k, v := range top {
		if !plain[k] && !isNull(v) {
			extras[k] = true
		}
	}
	for k, v := range transport {
		if k != "type" && k != "url" && !isNull(v) {
			extras[k] = true
		}
	}
	e.extras = sortedKeys(extras)
	return e, nil
}

func isNull(v json.RawMessage) bool { return strings.TrimSpace(string(v)) == "null" }

var antigravity = &harness{
	id: "antigravity", name: "Antigravity", bin: "agy",
	settings: func(c *Config) string {
		return filepath.Join(c.Home, ".gemini", "antigravity-cli", "settings.json")
	},
	allow:       "mcp(smrit/*)",
	global:      func(c *Config) string { return filepath.Join(c.Home, ".gemini", "config", "AGENTS.md") },
	projectFile: "AGENTS.md",
	inspect:     inspectAgy,
	extras:      agyExtras,
	// agy mcp add adds or updates, so a replacement is one command.
	add: func(url, _ string) []string {
		return []string{"agy", "mcp", "add", ServerName, url}
	},
	approval: func(c *Config) string {
		return fmt.Sprintf("Start a new agy session and sign in to Smrit when it asks, checking that you approve %s. agy mcp list shows the server.", c.Org)
	},
}

// inspectAgy reads agy mcp list: exactly "No MCP servers configured.", or a
// NAME TYPE STATUS COMMAND/URL header and one row a server.
func inspectAgy(ctx context.Context, c *Config) (entry, error) {
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
	out = strings.TrimSpace(strings.ReplaceAll(out, "\r\n", "\n"))
	if out == "No MCP servers configured." {
		return entry{}, nil
	}
	lines := strings.Split(out, "\n")
	if strings.Join(strings.Fields(lines[0]), " ") != "NAME TYPE STATUS COMMAND/URL" {
		return entry{}, unknown(argv)
	}
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 4 || (f[2] != "enabled" && f[2] != "disabled") {
			return entry{}, unknown(argv)
		}
		if f[0] != ServerName {
			continue
		}
		e := entry{present: true, disabled: f[2] == "disabled"}
		if f[1] != "http" {
			return entry{}, &Refused{fmt.Sprintf("Antigravity's smrit server is not an http server (%s), so it is not Smrit's and setup leaves it alone. Rename or remove it with agy mcp remove smrit, then run setup again.", quoted(f[1]))}
		}
		if len(f) != 4 {
			return entry{}, unknown(argv)
		}
		e.url = f[3]
		return e, nil
	}
	return entry{}, nil
}

// agyExtras reads the smrit entry in Antigravity's MCP config, which agy
// mcp list does not show in full: any key but serverUrl and disabled, such
// as headers, is a setting beyond the address.
func agyExtras(c *Config) ([]string, error) {
	path := filepath.Join(c.Home, ".gemini", "config", "mcp_config.json")
	data, _, exists, err := readTarget(path)
	if err != nil {
		return nil, err
	}
	cannot := &Refused{fmt.Sprintf("setup cannot read the smrit entry in %s, so it cannot tell whether replacing it would lose a setting, and changes nothing. Change its address by hand with agy mcp add smrit <address>, then run setup again.", path)}
	if !exists {
		return nil, cannot
	}
	root, err := parseJSON(data)
	if err != nil || root.kind != 'o' {
		return nil, cannot
	}
	servers := root.get("mcpServers")
	if servers == nil || servers.kind != 'o' {
		return nil, cannot
	}
	smrit := servers.get(ServerName)
	if smrit == nil || smrit.kind != 'o' {
		return nil, cannot
	}
	extras := map[string]bool{}
	for _, k := range smrit.keys {
		if k != "serverUrl" && k != "disabled" {
			extras[k] = true
		}
	}
	return sortedKeys(extras), nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
