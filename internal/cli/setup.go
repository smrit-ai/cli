package cli

import (
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

	"github.com/smrit-ai/cli/internal/profile"
	harnesssetup "github.com/smrit-ai/cli/internal/setup"
	"github.com/smrit-ai/cli/internal/state"
)

// onboardingSkillID is the served skill whose always on block setup writes.
const onboardingSkillID = "smrit:onboarding"

func setupCommand(s *Services) Command {
	return Command{
		Name:    "setup",
		Summary: "Connect a harness to Smrit: smrit setup <harness>. Shows the exact changes; --apply makes them.",
		Flags: []Flag{
			orgFlag,
			{Name: "apply", Usage: "Make the changes. Without it setup only shows them.", Bool: true},
			{Name: "block", Usage: "Where the always on block goes: everywhere, project or both.", Default: harnesssetup.BlockEverywhere},
			{Name: "project", Usage: "The project folder for the project file. The default is the top of the current git repository."},
		},
		Run: func(ctx context.Context, env *Env, args []string) int {
			return runSetup(ctx, s, env, args)
		},
	}
}

func harnessIDs() []string {
	ids := append([]string(nil), harnesssetup.Harnesses...)
	if hs, err := harnesssetup.Catalogue(); err == nil {
		for _, h := range hs {
			if !contains(ids, h.ID) {
				ids = append(ids, h.ID)
			}
		}
	}
	sort.Strings(ids)
	return ids
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func runSetup(ctx context.Context, s *Services, env *Env, args []string) int {
	usage := "Usage: smrit setup <harness> [--apply] [--block everywhere|project|both] [--project <dir>]. Harnesses: " + strings.Join(harnessIDs(), ", ") + "."
	if len(args) != 1 {
		return usageError(env.Stderr, "%s", usage)
	}
	id := args[0]
	builtin := contains(harnesssetup.Harnesses, id)
	other, inCatalogue := harnesssetup.Find(id)
	if !builtin && !inCatalogue {
		return usageError(env.Stderr, "%s", usage)
	}
	where := env.Flags["block"]
	if where != harnesssetup.BlockEverywhere && where != harnesssetup.BlockProject && where != harnesssetup.BlockBoth {
		return usageError(env.Stderr, "--block takes everywhere, project or both.")
	}
	if _, ok := s.Headless(); ok {
		fmt.Fprintln(env.Stderr, "smrit setup pins your harness to the organization you signed in to, so it needs smrit login rather than SMRIT_TOKEN.")
		return ExitNotSignedIn
	}
	p, err := profile.Pick(env.Flags["org"])
	if err != nil {
		return report(env, err)
	}
	var project string
	if builtin && (where != harnesssetup.BlockEverywhere || env.Flags["project"] != "") {
		if project, err = projectDir(ctx, env.Flags["project"]); err != nil {
			return usageError(env.Stderr, "%v", err)
		}
	}

	c, code := s.client(env)
	if c == nil {
		return code
	}
	raw, err := c.Call(ctx, "get_document", map[string]any{"document_id": onboardingSkillID})
	if err != nil {
		return report(env, err)
	}
	var skill string
	if json.Unmarshal(raw, &skill) != nil {
		skill = string(raw)
	}
	block, err := harnesssetup.ExtractBlock(skill)
	if err != nil {
		fmt.Fprintln(env.Stderr, err.Error())
		return ExitUnavailable
	}
	address := s.Origin + p.Endpoint

	if !builtin {
		env.Result(other.Rendered(map[string]string{
			"mcp_url": address, "server_name": harnesssetup.ServerName, "org_name": p.Org, "instructions": block,
		}))
		return ExitOK
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fmt.Fprintln(env.Stderr, "Could not find your home folder, where the harness's settings are.")
		return ExitRefused
	}
	now := s.Now
	if now == nil {
		now = time.Now
	}
	// Harness commands run in the project, so Claude Code's local scope is
	// the project's; without one, in the current folder.
	workDir := project
	if workDir == "" {
		if workDir, err = os.Getwd(); err != nil {
			workDir = ""
		}
	}
	stateDir, _ := state.Dir()
	cfg := &harnesssetup.Config{
		Harness: id, Origin: s.Origin, Endpoint: p.Endpoint, Org: p.Org, OrgID: p.OrgID,
		Block: block, Where: where, Project: project, Home: home,
		WorkDir: workDir, StateDir: stateDir,
		Getenv: os.Getenv, Exec: harnesssetup.SystemExec, Now: now,
	}
	plan, err := harnesssetup.Prepare(ctx, cfg)
	if err != nil {
		fmt.Fprintln(env.Stderr, err.Error())
		var refused *harnesssetup.Refused
		if errors.As(err, &refused) {
			return ExitRefused
		}
		return ExitUnavailable
	}
	if env.Flags["apply"] != "true" {
		env.Result(plan)
		return ExitOK
	}
	err = plan.Apply(ctx, cfg)
	env.Result(plan)
	if err != nil {
		fmt.Fprintln(env.Stderr, err.Error())
		var refused *harnesssetup.Refused
		if errors.As(err, &refused) {
			return ExitRefused
		}
		return ExitPartial
	}
	return ExitOK
}

// projectDir is the folder the project file goes in: dir when given, else
// the top of the git repository the current folder is in.
func projectDir(ctx context.Context, dir string) (string, error) {
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("Could not resolve %s: %v", dir, err)
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			return "", fmt.Errorf("--project %s is not a folder.", dir)
		}
		return abs, nil
	}
	notInRepo := errors.New("This folder is not in a git repository. Pass --project <dir> to say where the project file goes.")
	git, err := exec.LookPath("git")
	if err != nil {
		return "", notInRepo
	}
	cmd := exec.CommandContext(ctx, git, "-c", "core.fsmonitor=false", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	top := strings.TrimRight(string(out), "\n")
	if err != nil || top == "" {
		return "", notInRepo
	}
	return top, nil
}
