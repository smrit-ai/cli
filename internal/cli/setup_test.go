package cli

import (
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smrit-ai/cli/internal/state"
)

// onboardingSkill stands in for the served smrit:onboarding: the fenced
// block without the project line, as it is served until the skill changes.
const onboardingSkill = "# Setting up Smrit\n\n## 5. The always-on block\n\nIf the file has `<!-- smrit:start -->`, replace through `<!-- smrit:end -->`; otherwise append.\n\n" +
	"```markdown\n<!-- smrit:start -->\n## Smrit\nSmrit is this organization's memory.\n\nCall `start` once per session.\n<!-- smrit:end -->\n```\n\nIn the project file, add one paragraph.\n"

const servedBlock = "<!-- smrit:start -->\n## Smrit\nSmrit is this organization's memory.\n\nCall `start` once per session.\n<!-- smrit:project -->\n<!-- smrit:end -->"

// The fake harness commands record each argument list, one per line with a
// tab after each argument, and the folder each ran in, and keep one smrit
// entry in files of their own. Files in $FAKE_STATE steer them: <name>.raw
// and <name>.code replace what the looking command prints and its exit;
// <name>.addfail fails the next add, <name>.rmfail every remove;
// codex.oldcli makes get --json unknown, as in an older codex. FAKE_TOUCH
// names a file the looking command appends to, as another program writing
// while setup waits.
const fakeCommon = `d="$FAKE_STATE"
n=$(basename "$0")
{ printf '%s\t' "$n"; for a in "$@"; do printf '%s\t' "$a"; done; printf '\n'; } >> "$d/log"
pwd -P >> "$d/cwd"
looks=no
case "$n $1 $2" in "claude mcp get"|"codex mcp get"|"agy mcp list") looks=yes;; esac
if [ "$looks" = yes ]; then
  if [ -n "$FAKE_TOUCH" ]; then printf ' ' >> "$FAKE_TOUCH"; fi
  if [ -f "$d/$n.raw" ]; then cat "$d/$n.raw"; exit "$(cat "$d/$n.code" 2>/dev/null || echo 0)"; fi
fi
if [ "$1 $2" = "mcp add" ] && [ -f "$d/$n.addfail" ]; then rm -f "$d/$n.addfail"; echo 'add failed' >&2; exit 1; fi
if [ "$1 $2" = "mcp remove" ] && [ -f "$d/$n.rmfail" ]; then echo 'remove failed' >&2; exit 1; fi
`

const fakeClaude = "#!/bin/sh\n" + fakeCommon + `case "$1 $2" in
"mcp get")
  if [ -f "$d/claude.url" ]; then
    printf 'smrit:\n  Scope: %s\n  Status: connected\n  Type: http\n  URL: %s\n\nTo remove this server, run: claude mcp remove smrit -s user\n' "$(cat "$d/claude.scope")" "$(cat "$d/claude.url")"
    exit 0
  fi
  echo 'No MCP server named "smrit". Run claude mcp add to add one.'
  exit 1;;
"mcp add")
  scope=user; prev=; for a in "$@"; do [ "$prev" = --scope ] && scope="$a"; prev="$a"; last="$a"; done
  printf '%s' "$last" > "$d/claude.url"
  if [ "$scope" = local ]; then printf 'Local config (private to you in this project)' > "$d/claude.scope"; else printf 'User config (available in all your projects)' > "$d/claude.scope"; fi
  exit 0;;
"mcp remove")
  rm -f "$d/claude.url"; exit 0;;
esac
exit 9
`

const fakeCodex = "#!/bin/sh\n" + fakeCommon + `entry() { printf '{"name":"smrit","enabled":true,"disabled_reason":null,"transport":{"type":"streamable_http","url":"%s","bearer_token_env_var":null,"http_headers":null,"env_http_headers":null,"http_headers_helper":null},"enabled_tools":null,"disabled_tools":null,"startup_timeout_sec":null,"tool_timeout_sec":null}' "$(cat "$d/codex.url")"; }
case "$1 $2" in
"mcp get")
  if [ -f "$d/codex.oldcli" ]; then echo "error: unexpected argument '--json' found" >&2; exit 2; fi
  if [ -f "$d/codex.url" ]; then entry; echo; exit 0; fi
  echo "Error: No MCP server named 'smrit' found." >&2
  exit 1;;
"mcp list")
  if [ -f "$d/codex.url" ]; then printf '['; entry; printf ']\n'; else echo '[]'; fi
  exit 0;;
"mcp add")
  printf '%s' "$5" > "$d/codex.url"; exit 0;;
"mcp remove")
  rm -f "$d/codex.url"; exit 0;;
esac
exit 9
`

const fakeAgy = "#!/bin/sh\n" + fakeCommon + `case "$1 $2" in
"mcp list")
  if [ -f "$d/agy.url" ]; then
    printf 'NAME   TYPE   STATUS   COMMAND/URL\nother  stdio  enabled  echo hi\nsmrit  http   enabled  %s\n' "$(cat "$d/agy.url")"
  else
    echo 'No MCP servers configured.'
  fi
  exit 0;;
"mcp add")
  for a in "$@"; do last="$a"; done
  printf '%s' "$last" > "$d/agy.url"
  mkdir -p "$HOME/.gemini/config"
  printf '{"mcpServers":{"smrit":{"disabled":false,"serverUrl":"%s"}}}\n' "$last" > "$HOME/.gemini/config/mcp_config.json"
  exit 0;;
"mcp remove")
  rm -f "$d/agy.url"; exit 0;;
esac
exit 9
`

type harnessFakes struct {
	state string
}

// fakeHarnesses puts fake claude, codex and agy first on PATH, with nothing
// else of the person's own reachable, and points CLAUDE_CONFIG_DIR and
// CODEX_HOME into the temporary HOME: a test never reads or writes the
// person's real configuration.
func fakeHarnesses(t *testing.T, which ...string) *harnessFakes {
	t.Helper()
	bin, state := t.TempDir(), t.TempDir()
	scripts := map[string]string{"claude": fakeClaude, "codex": fakeCodex, "agy": fakeAgy}
	if len(which) == 0 {
		which = []string{"claude", "codex", "agy"}
	}
	for _, name := range which {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(scripts[name]), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	home := os.Getenv("HOME")
	if !strings.HasPrefix(home, os.TempDir()) && !strings.Contains(home, "/T/") && !strings.HasPrefix(home, "/tmp") {
		t.Fatalf("HOME %s is not a temporary folder; refusing to run harness fakes", home)
	}
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	t.Setenv("FAKE_STATE", state)
	t.Setenv("FAKE_TOUCH", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	return &harnessFakes{state: state}
}

func (h *harnessFakes) set(t *testing.T, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.state, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *harnessFakes) entry(t *testing.T, harness, url string) {
	t.Helper()
	h.set(t, harness+".url", url)
	switch harness {
	case "claude":
		h.set(t, "claude.scope", "User config (available in all your projects)")
	case "agy":
		writeFile(t, filepath.Join(os.Getenv("HOME"), ".gemini", "config", "mcp_config.json"),
			`{"mcpServers":{"other":{"command":"echo","args":["hi"],"disabled":false},"smrit":{"disabled":false,"serverUrl":"`+url+`"}}}`, 0o644)
	}
}

func (h *harnessFakes) url(harness string) string {
	b, _ := os.ReadFile(filepath.Join(h.state, harness+".url"))
	return string(b)
}

// calls is every command a fake ran, each as one space joined line.
func (h *harnessFakes) calls() []string { return h.lines("log") }

// cwds is the folder each command ran in, in the order of calls.
func (h *harnessFakes) cwds() []string { return h.lines("cwd") }

func (h *harnessFakes) lines(name string) []string {
	b, _ := os.ReadFile(filepath.Join(h.state, name))
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line != "" {
			out = append(out, strings.Join(strings.Fields(strings.ReplaceAll(line, "\t", " ")), " "))
		}
	}
	return out
}

// setupEnv is a signed in profile for acme, a fake server serving the
// onboarding skill, a temporary HOME and fake harness commands.
func setupEnv(t *testing.T, which ...string) (*Services, *mcpFake, *harnessFakes, string, string) {
	t.Helper()
	f, srv := newMCP(t)
	f.tool = func(name string, args map[string]any) string {
		if name == "get_document" && args["document_id"] == "smrit:onboarding" {
			return onboardingSkill
		}
		return `{"error":"unexpected"}`
	}
	svc := setup(t, srv, prof("acme", orgID))
	h := fakeHarnesses(t, which...)
	home, _ := os.UserHomeDir()
	return svc, f, h, home, pinnedURL(srv)
}

func pinnedURL(srv *httptest.Server) string { return srv.URL + "/mcp/o/acme/" + orgID }

func writeFile(t *testing.T, path, body string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, mode)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func backups(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), ".smrit-backup-") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func noToken(t *testing.T, outs ...string) {
	t.Helper()
	for _, o := range outs {
		if strings.Contains(o, fakeToken) {
			t.Fatalf("a token reached the output: %s", o)
		}
	}
}

func TestSetupDiffWritesNothing(t *testing.T) {
	svc, _, h, home, url := setupEnv(t)
	project := t.TempDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{"model":"opus"}`, 0o600)
	writeFile(t, filepath.Join(project, "CLAUDE.md"), "# Project\n", 0o644)

	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--block", "both", "--project", project, "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	noToken(t, out, errs)
	for _, want := range []string{
		"+++ " + settings, "+      \"mcp__smrit\"",
		"--- /dev/null", "+++ " + filepath.Join(home, ".claude", "CLAUDE.md"), "+<!-- smrit:start -->", "+<!-- smrit:project -->",
		"+++ " + filepath.Join(project, "CLAUDE.md"),
		"claude mcp add --transport http --scope user smrit " + url,
		"Nothing was written",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the preview lacks %q:\n%s", want, out)
		}
	}
	if readFile(t, settings) != `{"model":"opus"}` || readFile(t, filepath.Join(project, "CLAUDE.md")) != "# Project\n" {
		t.Fatal("a preview wrote a file")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "CLAUDE.md")); err == nil {
		t.Fatal("a preview created a file")
	}
	if b := append(backups(t, home), backups(t, project)...); len(b) != 0 {
		t.Fatalf("a preview made backups: %v", b)
	}
	if c := h.calls(); len(c) != 1 || c[0] != "claude mcp get smrit" {
		t.Fatalf("a preview may only look: %v", c)
	}
}

func TestSetupDiffAsJSON(t *testing.T) {
	svc, _, _, _, url := setupEnv(t)
	code, out, errs := runCmd(t, svc, "setup", "claude-code")
	if code != ExitOK {
		t.Fatalf("exit %d %s", code, errs)
	}
	var r struct {
		Harness  string `json:"harness"`
		Address  string `json:"address"`
		Applied  bool   `json:"applied"`
		Entry    string `json:"entry"`
		Files    []struct{ Path, Action, Diff string }
		Commands []struct {
			Argv []string `json:"argv"`
			Ran  bool     `json:"ran"`
		}
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if r.Harness != "claude-code" || r.Address != url || r.Applied || r.Entry != "add" || len(r.Files) != 2 || len(r.Commands) != 1 || r.Commands[0].Ran {
		t.Fatalf("%+v", r)
	}
}

func TestSetupKeepsOtherEntriesAndOrder(t *testing.T) {
	svc, _, _, home, _ := setupEnv(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{"model":"opus","permissions":{"deny":["Bash(rm:*)"],"allow":["Read(./**)"]},"env":{"A":"1"},"n":1.50}`, 0o600)
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	want := "{\n  \"model\": \"opus\",\n  \"permissions\": {\n    \"deny\": [\n      \"Bash(rm:*)\"\n    ],\n    \"allow\": [\n      \"Read(./**)\",\n      \"mcp__smrit\"\n    ]\n  },\n  \"env\": {\n    \"A\": \"1\"\n  },\n  \"n\": 1.50\n}\n"
	if got := readFile(t, settings); got != want {
		t.Fatalf("settings:\n%s", got)
	}
	// Again: nothing to change, no duplicate rule, no new backup.
	before := len(backups(t, home))
	code, out, _ = runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitOK || readFile(t, settings) != want || len(backups(t, home)) != before {
		t.Fatalf("a second run changed something: exit %d\n%s", code, out)
	}
}

func TestAntigravitySettingsKeepOtherRules(t *testing.T) {
	svc, _, h, home, url := setupEnv(t)
	settings := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	writeFile(t, settings, `{"model":"x","permissions":{"allow":["command(make test)"]},"toolPermission":"ask"}`, 0o600)
	code, out, errs := runCmd(t, svc, "setup", "antigravity", "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	want := "{\n  \"model\": \"x\",\n  \"permissions\": {\n    \"allow\": [\n      \"command(make test)\",\n      \"mcp(smrit/*)\"\n    ]\n  },\n  \"toolPermission\": \"ask\"\n}\n"
	if got := readFile(t, settings); got != want {
		t.Fatalf("settings:\n%s", got)
	}
	if !strings.Contains(readFile(t, filepath.Join(home, ".gemini", "config", "AGENTS.md")), servedBlock) {
		t.Fatal("the global instruction file lacks the block")
	}
	if h.url("agy") != url {
		t.Fatalf("agy entry %q", h.url("agy"))
	}
}

func TestSetupReusesExistingEntry(t *testing.T) {
	svc, _, h, _, url := setupEnv(t)
	h.entry(t, "claude", url)
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if c := h.calls(); len(c) != 1 || c[0] != "claude mcp get smrit" {
		t.Fatalf("an entry already pinned must be left alone: %v", c)
	}
	if !strings.Contains(out, "already") {
		t.Fatalf("say the entry is already there:\n%s", out)
	}
}

func TestSetupPinsBareAddress(t *testing.T) {
	cases := []struct {
		harness, fake string
		want          func(url string) []string
	}{
		{"claude-code", "claude", func(url string) []string {
			return []string{"claude mcp get smrit", "claude mcp remove --scope user smrit", "claude mcp add --transport http --scope user smrit " + url}
		}},
		{"codex", "codex", func(url string) []string {
			return []string{"codex mcp get smrit --json", "codex mcp remove smrit", "codex mcp add smrit --url " + url}
		}},
		{"antigravity", "agy", func(url string) []string {
			return []string{"agy mcp list", "agy mcp add smrit " + url}
		}},
	}
	for _, c := range cases {
		t.Run(c.harness, func(t *testing.T) {
			svc, _, h, _, url := setupEnv(t)
			origin := strings.TrimSuffix(url, "/mcp/o/acme/"+orgID)
			h.entry(t, c.fake, origin+"/mcp")
			code, out, errs := runCmd(t, svc, "setup", c.harness, "--json=false")
			if code != ExitOK {
				t.Fatalf("exit %d\n%s\n%s", code, out, errs)
			}
			for _, cmd := range c.want(url)[1:] {
				if !strings.Contains(out, cmd) {
					t.Fatalf("the preview lacks %q:\n%s", cmd, out)
				}
			}
			if h.url(c.fake) != origin+"/mcp" {
				t.Fatal("a preview changed the entry")
			}
			code, out, errs = runCmd(t, svc, "setup", c.harness, "--apply", "--json=false")
			if code != ExitOK {
				t.Fatalf("exit %d\n%s\n%s", code, out, errs)
			}
			got := h.calls()[1:] // after the preview's one look
			if strings.Join(got, "\n") != strings.Join(c.want(url), "\n") {
				t.Fatalf("commands:\n%s", strings.Join(got, "\n"))
			}
			if h.url(c.fake) != url {
				t.Fatalf("entry %q", h.url(c.fake))
			}
			noToken(t, out, errs)
		})
	}
}

func TestSetupNeverAnotherAddress(t *testing.T) {
	for name, addr := range map[string]string{
		"another host":         "https://elsewhere.example/mcp",
		"another organization": "/mcp/o/other/" + orgID2,
	} {
		t.Run(name, func(t *testing.T) {
			svc, _, h, home, url := setupEnv(t)
			if strings.HasPrefix(addr, "/") {
				addr = strings.TrimSuffix(url, "/mcp/o/acme/"+orgID) + addr
			}
			h.entry(t, "claude", addr)
			code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
			if code != ExitRefused {
				t.Fatalf("exit %d\n%s\n%s", code, out, errs)
			}
			if h.url("claude") != addr || len(h.calls()) != 1 {
				t.Fatalf("the entry was touched: %v", h.calls())
			}
			if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
				t.Fatal("a refusal still wrote files")
			}
			if !strings.Contains(errs, "smrit") {
				t.Fatalf("say why: %s", errs)
			}
		})
	}
}

func TestSetupBlockReplacedKeepsProjectParagraph(t *testing.T) {
	svc, _, _, _, _ := setupEnv(t)
	project := t.TempDir()
	path := filepath.Join(project, "CLAUDE.md")
	writeFile(t, path, "# Rules\n\n<!-- smrit:start -->\nOld words.\n<!-- smrit:project -->\nThis project is in the web space.\n<!-- smrit:end -->\n\nAfter.\n", 0o644)
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--block", "project", "--project", project, "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	want := "# Rules\n\n" + strings.TrimSuffix(servedBlock, "<!-- smrit:end -->") + "This project is in the web space.\n<!-- smrit:end -->\n\nAfter.\n"
	if got := readFile(t, path); got != want {
		t.Fatalf("got:\n%s", got)
	}
}

func TestSetupBlockAppendedWhenAbsent(t *testing.T) {
	svc, _, _, home, _ := setupEnv(t)
	path := filepath.Join(home, ".claude", "CLAUDE.md")
	writeFile(t, path, "# Mine\n\nBe brief.\n", 0o644)
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if got := readFile(t, path); got != "# Mine\n\nBe brief.\n\n"+servedBlock+"\n" {
		t.Fatalf("got:\n%s", got)
	}
}

func TestBackupBeforeWrite(t *testing.T) {
	svc, _, _, home, _ := setupEnv(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	global := filepath.Join(home, ".claude", "CLAUDE.md")
	writeFile(t, settings, `{"a":1}`, 0o640)
	project := t.TempDir()
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--block", "both", "--project", project, "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	stamp := ".smrit-backup-19700101T001640Z"
	b, err := os.ReadFile(settings + stamp)
	if err != nil || string(b) != `{"a":1}` {
		t.Fatalf("backup %q %v", b, err)
	}
	for path, mode := range map[string]fs.FileMode{settings + stamp: 0o640, settings: 0o640, global: 0o644, filepath.Join(project, "CLAUDE.md"): 0o644} {
		st, err := os.Stat(path)
		if err != nil || st.Mode().Perm() != mode {
			t.Fatalf("%s mode %v %v, want %v", path, st.Mode().Perm(), err, mode)
		}
	}
	if b := backups(t, home); len(b) != 1 {
		t.Fatalf("only an existing file is backed up: %v", b)
	}
	if !strings.Contains(out, settings+stamp) {
		t.Fatalf("name the backup:\n%s", out)
	}
	leftovers, _ := filepath.Glob(filepath.Join(home, ".claude", ".*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}

func TestSetupNewSettingsFileIsPrivate(t *testing.T) {
	svc, _, _, home, _ := setupEnv(t)
	code, _, errs := runCmd(t, svc, "setup", "antigravity", "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d %s", code, errs)
	}
	st, err := os.Stat(filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
}

func TestInvalidJSONRefused(t *testing.T) {
	svc, _, h, home, _ := setupEnv(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{"model": "opus",`, 0o600)
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitRefused {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(errs, settings) || !strings.Contains(errs, "not valid JSON") {
		t.Fatalf("say it plainly: %s", errs)
	}
	if readFile(t, settings) != `{"model": "opus",` || len(backups(t, home)) != 0 {
		t.Fatal("the file was touched")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "CLAUDE.md")); err == nil {
		t.Fatal("a refusal still wrote the instruction file")
	}
	if len(h.calls()) > 1 {
		t.Fatalf("a refusal still ran commands: %v", h.calls())
	}
}

func TestSymlinkRefused(t *testing.T) {
	for _, name := range []string{"settings.json", "CLAUDE.md"} {
		t.Run(name, func(t *testing.T) {
			svc, _, _, home, _ := setupEnv(t)
			target := filepath.Join(t.TempDir(), "real")
			writeFile(t, target, "{}", 0o600)
			os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
			link := filepath.Join(home, ".claude", name)
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
			if code != ExitRefused {
				t.Fatalf("exit %d\n%s\n%s", code, out, errs)
			}
			if !strings.Contains(errs, link) {
				t.Fatalf("name the link: %s", errs)
			}
			if readFile(t, target) != "{}" {
				t.Fatal("the link's target was written")
			}
		})
	}
}

func TestOtherHarnessPrintsSteps(t *testing.T) {
	svc, _, h, home, url := setupEnv(t)
	code, out, errs := runCmd(t, svc, "setup", "grok-build", "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{"Grok Build", "grok mcp add --transport http smrit " + url, "check acme before approving", "<!-- smrit:start -->", "~/.grok/AGENTS.md"} {
		if !strings.Contains(out, want) {
			t.Fatalf("lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "{{") {
		t.Fatalf("a placeholder was left:\n%s", out)
	}
	if len(h.calls()) != 0 {
		t.Fatalf("ran %v", h.calls())
	}
	for _, d := range []string{".claude", ".codex", ".gemini", ".grok"} {
		if _, err := os.Stat(filepath.Join(home, d)); err == nil {
			t.Fatalf("wrote %s", d)
		}
	}
}

func TestCodexSaysNoAllowRule(t *testing.T) {
	svc, _, h, home, url := setupEnv(t)
	code, out, errs := runCmd(t, svc, "setup", "codex", "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "Codex has no allow rule") {
		t.Fatalf("say there is no allow rule:\n%s", out)
	}
	if !strings.Contains(readFile(t, filepath.Join(home, ".codex", "AGENTS.md")), servedBlock) {
		t.Fatal("the global AGENTS.md lacks the block")
	}
	if entries, _ := os.ReadDir(filepath.Join(home, ".codex")); len(entries) != 1 {
		t.Fatalf("only AGENTS.md is written: %v", entries)
	}
	if h.url("codex") != url {
		t.Fatalf("entry %q", h.url("codex"))
	}
	if !strings.Contains(out, "codex mcp login smrit --scopes read,write") {
		t.Fatalf("name the login still owed:\n%s", out)
	}
}

func TestSetupCodexHomeAndOverride(t *testing.T) {
	svc, _, _, _, _ := setupEnv(t)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	writeFile(t, filepath.Join(codexHome, "AGENTS.override.md"), "Override.\n", 0o644)
	code, out, errs := runCmd(t, svc, "setup", "codex", "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if got := readFile(t, filepath.Join(codexHome, "AGENTS.override.md")); got != "Override.\n\n"+servedBlock+"\n" {
		t.Fatalf("override:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(codexHome, "AGENTS.md")); err == nil {
		t.Fatal("AGENTS.md written beside a nonempty override")
	}
}

func TestSetupClaudeConfigDir(t *testing.T) {
	svc, _, _, home, _ := setupEnv(t)
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	writeFile(t, filepath.Join(dir, "rules", "smrit.md"), "old rule\n", 0o644)
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	for _, p := range []string{filepath.Join(dir, "settings.json"), filepath.Join(dir, "CLAUDE.md")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
		t.Fatal("wrote ~/.claude while CLAUDE_CONFIG_DIR names another folder")
	}
	if readFile(t, filepath.Join(dir, "rules", "smrit.md")) != "old rule\n" || !strings.Contains(out, filepath.Join(dir, "rules", "smrit.md")) {
		t.Fatalf("the old rule file is reported and left:\n%s", out)
	}
}

func TestSetupClaudeMissingPrintsCommand(t *testing.T) {
	svc, _, _, home, url := setupEnv(t, "codex")
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitPartial {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out+errs, "claude mcp add --transport http --scope user smrit "+url) || !strings.Contains(out+errs, "not found") {
		t.Fatalf("say so and give the command:\n%s\n%s", out, errs)
	}
	if !strings.Contains(readFile(t, filepath.Join(home, ".claude", "settings.json")), "mcp__smrit") {
		t.Fatal("the files are still written")
	}
}

func TestSetupHeadlessRefused(t *testing.T) {
	svc, f, h, home, _ := setupEnv(t)
	svc.Headless = func() (string, bool) { return fakeToken, true }
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply")
	if code != ExitNotSignedIn {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	noToken(t, out, errs)
	if len(f.paths) != 0 || len(h.calls()) != 0 {
		t.Fatal("refused before anything ran")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
		t.Fatal("wrote ~/.claude")
	}
}

func TestSetupNotSignedIn(t *testing.T) {
	_, srv := newMCP(t)
	svc := setup(t, srv)
	fakeHarnesses(t)
	code, _, errs := runCmd(t, svc, "setup", "claude-code")
	if code != ExitNotSignedIn || !strings.Contains(errs, "smrit login") {
		t.Fatalf("exit %d %s", code, errs)
	}
}

func TestSetupUsage(t *testing.T) {
	svc, _, _, _, _ := setupEnv(t)
	for _, args := range [][]string{{"setup"}, {"setup", "nope"}, {"setup", "claude-code", "codex"}, {"setup", "claude-code", "--block", "somewhere"}} {
		if code, _, _ := runCmd(t, svc, args...); code != ExitUsage {
			t.Fatalf("%v: exit %d", args, code)
		}
	}
}

func TestSetupProjectDefaultsToRepositoryTop(t *testing.T) {
	svc, _, _, _, _ := setupEnv(t)
	t.Setenv("PATH", os.Getenv("PATH")+":/opt/homebrew/bin:/usr/local/bin")
	repo := t.TempDir()
	if out, err := execGit(repo, "init", "-q"); err != nil {
		t.Skipf("git is not available: %v %s", err, out)
	}
	sub := filepath.Join(repo, "a", "b")
	os.MkdirAll(sub, 0o755)
	t.Chdir(sub)
	code, out, errs := runCmd(t, svc, "setup", "codex", "--block", "project", "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	real, _ := filepath.EvalSymlinks(repo)
	if !strings.Contains(readFile(t, filepath.Join(real, "AGENTS.md")), servedBlock) {
		t.Fatal("the project file belongs at the repository's top")
	}
}

func execGit(dir string, args ...string) ([]byte, error) {
	return exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
}

func stateDir(t *testing.T) string {
	t.Helper()
	d, err := state.Dir()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// A file written by someone else while setup waited on a harness command is
// neither overwritten nor lost: setup refuses and writes nothing.
func TestSetupRefusesAFileChangedSinceItWasRead(t *testing.T) {
	svc, _, h, home, _ := setupEnv(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{"a":1}`, 0o600)
	t.Setenv("FAKE_TOUCH", settings)
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitRefused {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if got := readFile(t, settings); got != `{"a":1} ` {
		t.Fatalf("the other program's write was lost: %q", got)
	}
	if !strings.Contains(errs, settings) {
		t.Fatalf("name the file: %s", errs)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "CLAUDE.md")); err == nil {
		t.Fatal("nothing is written once a file changed")
	}
	for _, c := range h.calls() {
		if strings.Contains(c, " add ") {
			t.Fatalf("nothing is run once a file changed: %v", h.calls())
		}
	}
}

func TestSetupUnknownOutputRefuses(t *testing.T) {
	cases := []struct {
		name, harness, fake, raw, code string
	}{
		{"claude garbage", "claude-code", "claude", "something new\n", "0"},
		{"claude no address", "claude-code", "claude", "smrit:\n  Scope: User config (available in all your projects)\n  Type: stdio\n  Command: smrit-mcp\n", "0"},
		{"claude other failure", "claude-code", "claude", "Error: the config file is damaged\n", "1"},
		{"codex null", "codex", "codex", "null\n", "0"},
		{"codex other JSON", "codex", "codex", "[1,2]\n", "0"},
		{"codex stdio", "codex", "codex", `{"name":"smrit","enabled":true,"transport":{"type":"stdio","command":"x"}}` + "\n", "0"},
		{"codex other name", "codex", "codex", `{"name":"other","enabled":true,"transport":{"type":"streamable_http","url":"https://x.example/mcp"}}` + "\n", "0"},
		{"agy garbage", "antigravity", "agy", "Servers:\n  smrit\n", "0"},
		{"agy row without address", "antigravity", "agy", "NAME   TYPE   STATUS   COMMAND/URL\nsmrit  http   enabled\n", "0"},
		{"agy failure", "antigravity", "agy", "", "1"},
		{"agy another layout", "antigravity", "agy", "Configured servers: other, smrit\n", "0"},
		{"agy numbered rows", "antigravity", "agy", "1. smrit (http) https://elsewhere.example/mcp\n", "0"},
		{"claude failure naming no server", "claude-code", "claude", "Error: no MCP servers could be loaded, the config is damaged\n", "1"},
		{"codex failure naming no server", "codex", "codex", "Error: No MCP server config could be parsed\n", "1"},
		{"codex empty object", "codex", "codex", "{}\n", "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, _, h, _, _ := setupEnv(t)
			h.set(t, c.fake+".raw", c.raw)
			h.set(t, c.fake+".code", c.code)
			if c.fake == "codex" && c.code != "0" {
				h.set(t, "codex.oldcli", "")
			}
			code, out, errs := runCmd(t, svc, "setup", c.harness, "--apply", "--json=false")
			if code != ExitRefused {
				t.Fatalf("exit %d\n%s\n%s", code, out, errs)
			}
			for _, call := range h.calls() {
				if strings.Contains(call, " add ") || strings.Contains(call, " remove ") {
					t.Fatalf("unknown output led to %q", call)
				}
			}
		})
	}
}

func TestSetupCodexListFallback(t *testing.T) {
	svc, _, h, _, url := setupEnv(t)
	h.set(t, "codex.oldcli", "")
	origin := strings.TrimSuffix(url, "/mcp/o/acme/"+orgID)
	h.entry(t, "codex", origin+"/mcp")
	code, out, errs := runCmd(t, svc, "setup", "codex", "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	want := []string{"codex mcp get smrit --json", "codex mcp list --json", "codex mcp remove smrit", "codex mcp add smrit --url " + url}
	if strings.Join(h.calls(), "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(h.calls(), "\n"))
	}
}

func TestSetupNeverReplacesAnEntryWithMoreThanAnAddress(t *testing.T) {
	cases := []struct {
		name, harness, fake string
		prepare             func(t *testing.T, h *harnessFakes, bare string)
	}{
		{"claude headers", "claude-code", "claude", func(t *testing.T, h *harnessFakes, bare string) {
			h.set(t, "claude.raw", "smrit:\n  Scope: User config (available in all your projects)\n  Status: connected\n  Type: http\n  URL: "+bare+"\n  Headers:\n    X-Team: core\n")
		}},
		{"codex bearer variable", "codex", "codex", func(t *testing.T, h *harnessFakes, bare string) {
			h.set(t, "codex.raw", `{"name":"smrit","enabled":true,"transport":{"type":"streamable_http","url":"`+bare+`","bearer_token_env_var":"MY_VAR","http_headers":null}}`+"\n")
		}},
		{"agy headers", "antigravity", "agy", func(t *testing.T, h *harnessFakes, bare string) {
			h.entry(t, "agy", bare)
			writeFile(t, filepath.Join(os.Getenv("HOME"), ".gemini", "config", "mcp_config.json"),
				`{"mcpServers":{"smrit":{"disabled":false,"serverUrl":"`+bare+`","headers":{"X-Team":"core"}}}}`, 0o644)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, _, h, _, url := setupEnv(t)
			c.prepare(t, h, strings.TrimSuffix(url, "/mcp/o/acme/"+orgID)+"/mcp")
			code, out, errs := runCmd(t, svc, "setup", c.harness, "--apply", "--json=false")
			if code != ExitRefused {
				t.Fatalf("exit %d\n%s\n%s", code, out, errs)
			}
			if !strings.Contains(errs, "by hand") {
				t.Fatalf("say what to change by hand: %s", errs)
			}
			if len(h.calls()) != 1 {
				t.Fatalf("ran %v", h.calls())
			}
		})
	}
}

func TestSetupReusesAPinnedEntryWithMoreThanAnAddress(t *testing.T) {
	svc, _, h, _, url := setupEnv(t)
	h.set(t, "claude.raw", "smrit:\n  Scope: User config (available in all your projects)\n  Type: http\n  URL: "+url+"\n  Headers:\n    X-Team: core\n")
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitOK || len(h.calls()) != 1 {
		t.Fatalf("exit %d %v\n%s\n%s", code, h.calls(), out, errs)
	}
}

func TestSetupRefusesOtherShapesAndQuotesWhatItPrints(t *testing.T) {
	for name, path := range map[string]string{
		"another organization, longer path": "/mcp/o/other/" + orgID2 + "/x",
		"this organization, longer path":    "/mcp/o/acme/" + orgID + "/x",
	} {
		t.Run(name, func(t *testing.T) {
			svc, _, h, _, url := setupEnv(t)
			h.entry(t, "claude", strings.TrimSuffix(url, "/mcp/o/acme/"+orgID)+path)
			code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
			if code != ExitRefused || len(h.calls()) != 1 {
				t.Fatalf("exit %d %v\n%s\n%s", code, h.calls(), out, errs)
			}
		})
	}
	svc, _, h, _, _ := setupEnv(t)
	h.entry(t, "claude", "https://elsewhere.example/a\x1b[31mb")
	code, _, errs := runCmd(t, svc, "setup", "claude-code", "--json=false")
	if code != ExitRefused || strings.Contains(errs, "\x1b") || !strings.Contains(errs, `"https://elsewhere.example/a`) {
		t.Fatalf("exit %d, print it quoted: %q", code, errs)
	}
}

func TestSetupRestoresTheEntryWhenTheAddFails(t *testing.T) {
	svc, _, h, home, url := setupEnv(t)
	bare := strings.TrimSuffix(url, "/mcp/o/acme/"+orgID) + "/mcp"
	h.entry(t, "claude", bare)
	h.set(t, "claude.addfail", "")
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitPartial {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	want := []string{"claude mcp get smrit", "claude mcp remove --scope user smrit",
		"claude mcp add --transport http --scope user smrit " + url, "claude mcp add --transport http --scope user smrit " + bare}
	if strings.Join(h.calls(), "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(h.calls(), "\n"))
	}
	if h.url("claude") != bare || !strings.Contains(errs, "put back") {
		t.Fatalf("entry %q, %s", h.url("claude"), errs)
	}
	if !strings.Contains(readFile(t, filepath.Join(home, ".claude", "settings.json")), "mcp__smrit") {
		t.Fatal("the files are written before the commands")
	}
}

func TestSetupReportsAFailedCommand(t *testing.T) {
	svc, _, h, _, url := setupEnv(t)
	bare := strings.TrimSuffix(url, "/mcp/o/acme/"+orgID) + "/mcp"
	h.entry(t, "codex", bare)
	h.set(t, "codex.rmfail", "")
	code, out, errs := runCmd(t, svc, "setup", "codex", "--apply", "--json=false")
	if code != ExitPartial {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if c := h.calls(); len(c) != 2 || h.url("codex") != bare {
		t.Fatalf("after a failed remove nothing else runs: %v", c)
	}
	if !strings.Contains(errs, "codex mcp remove smrit failed") || !strings.Contains(out, "failed, exit 1") {
		t.Fatalf("report it:\n%s\n%s", out, errs)
	}
}

func TestSetupClaudeScopes(t *testing.T) {
	t.Run("local, read and changed in the project", func(t *testing.T) {
		svc, _, h, _, url := setupEnv(t)
		project := t.TempDir()
		h.entry(t, "claude", strings.TrimSuffix(url, "/mcp/o/acme/"+orgID)+"/mcp")
		h.set(t, "claude.scope", "Local config (private to you in this project)")
		code, out, errs := runCmd(t, svc, "setup", "claude-code", "--project", project, "--apply", "--json=false")
		if code != ExitOK {
			t.Fatalf("exit %d\n%s\n%s", code, out, errs)
		}
		c := h.calls()
		if len(c) != 3 || c[1] != "claude mcp remove --scope local smrit" || c[2] != "claude mcp add --transport http --scope user smrit "+url {
			t.Fatalf("calls %v", c)
		}
		real, _ := filepath.EvalSymlinks(project)
		if cw := h.cwds(); cw[0] != real || cw[1] != real {
			t.Fatalf("the local scope is the project's: %v", cw)
		}
	})
	t.Run("project, refused", func(t *testing.T) {
		svc, _, h, _, url := setupEnv(t)
		h.entry(t, "claude", strings.TrimSuffix(url, "/mcp/o/acme/"+orgID)+"/mcp")
		h.set(t, "claude.scope", "Project config (shared via .mcp.json)")
		code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
		if code != ExitRefused || len(h.calls()) != 1 || !strings.Contains(errs, ".mcp.json") {
			t.Fatalf("exit %d %v\n%s\n%s", code, h.calls(), out, errs)
		}
	})
	t.Run("project, pinned already", func(t *testing.T) {
		svc, _, h, _, url := setupEnv(t)
		h.entry(t, "claude", url)
		h.set(t, "claude.scope", "Project config (shared via .mcp.json)")
		code, _, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
		if code != ExitOK || len(h.calls()) != 1 {
			t.Fatalf("exit %d %v %s", code, h.calls(), errs)
		}
	})
}

func TestSetupBackupNameCollision(t *testing.T) {
	svc, _, _, home, _ := setupEnv(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, settings, `{"a":1}`, 0o600)
	taken := settings + ".smrit-backup-19700101T001640Z"
	writeFile(t, taken, "earlier", 0o600)
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if readFile(t, taken) != "earlier" || readFile(t, taken+"-1") != `{"a":1}` {
		t.Fatal("a backup overwrote another")
	}
}

func TestSetupProjectBackupsLeaveTheWorkingTree(t *testing.T) {
	svc, _, _, _, _ := setupEnv(t)
	project := t.TempDir()
	writeFile(t, filepath.Join(project, "CLAUDE.md"), "# Project\n", 0o644)
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--block", "project", "--project", project, "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if b := backups(t, project); len(b) != 0 {
		t.Fatalf("a backup in the working tree: %v", b)
	}
	want := filepath.Join(stateDir(t), "backups", "19700101T001640Z", "CLAUDE.md")
	if readFile(t, want) != "# Project\n" || !strings.Contains(out, want) {
		t.Fatalf("the backup belongs at %s:\n%s", want, out)
	}
}

func TestSetupKeepsCRLF(t *testing.T) {
	svc, _, _, home, _ := setupEnv(t)
	global := filepath.Join(home, ".claude", "CLAUDE.md")
	settings := filepath.Join(home, ".claude", "settings.json")
	writeFile(t, global, "# Mine\r\nBe brief.\r\n", 0o644)
	writeFile(t, settings, "{\r\n  \"a\": 1\r\n}\r\n", 0o600)
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if got := readFile(t, global); got != "# Mine\r\nBe brief.\r\n\r\n"+strings.ReplaceAll(servedBlock, "\n", "\r\n")+"\r\n" {
		t.Fatalf("instructions %q", got)
	}
	if got := readFile(t, settings); got != "{\r\n  \"a\": 1,\r\n  \"permissions\": {\r\n    \"allow\": [\r\n      \"mcp__smrit\"\r\n    ]\r\n  }\r\n}\r\n" {
		t.Fatalf("settings %q", got)
	}
}

func TestSetupDuplicateKeysRefused(t *testing.T) {
	svc, _, _, home, _ := setupEnv(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	body := `{"permissions":{"allow":["Read"]},"permissions":{"allow":[]}}`
	writeFile(t, settings, body, 0o600)
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitRefused || readFile(t, settings) != body || !strings.Contains(errs, `"permissions"`) {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
}

func TestSetupMalformedBlocksRefused(t *testing.T) {
	for name, body := range map[string]string{
		"two starts": "<!-- smrit:start -->\nA\n<!-- smrit:start -->\nB\n<!-- smrit:end -->\n",
		"end first":  "<!-- smrit:end -->\n<!-- smrit:start -->\nA\n<!-- smrit:end -->\n",
		"no end":     "<!-- smrit:start -->\nA\n",
	} {
		t.Run(name, func(t *testing.T) {
			svc, _, _, home, _ := setupEnv(t)
			global := filepath.Join(home, ".claude", "CLAUDE.md")
			writeFile(t, global, body, 0o644)
			code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
			if code != ExitRefused || readFile(t, global) != body {
				t.Fatalf("exit %d\n%s\n%s", code, out, errs)
			}
		})
	}
}

// When the file that changed comes later, no earlier file is written either.
func TestSetupRefusesBeforeWritingAnyFile(t *testing.T) {
	svc, _, _, home, _ := setupEnv(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	global := filepath.Join(home, ".claude", "CLAUDE.md")
	writeFile(t, settings, `{"a":1}`, 0o600)
	writeFile(t, global, "# Mine\n", 0o644)
	t.Setenv("FAKE_TOUCH", global)
	code, out, errs := runCmd(t, svc, "setup", "claude-code", "--apply", "--json=false")
	if code != ExitRefused {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if readFile(t, settings) != `{"a":1}` || readFile(t, global) != "# Mine\n " || len(backups(t, home)) != 0 {
		t.Fatal("a file was written or backed up after another changed")
	}
}
