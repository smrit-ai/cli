package scan

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitRunner calls the git binary with an explicit argument list, never
// through a shell.
type gitRunner struct {
	bin string
	env []string
}

// noGitNote is said once when git cannot be found.
const noGitNote = "git was not found, so every file is listed as outside a repository and none is marked used."

// newGit finds the git binary. It returns nil when there is none.
func newGit(bin string) *gitRunner {
	if bin == "" {
		bin = "git"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil
	}
	// Variables that would point git at another repository than the one
	// the folder is in are dropped. Optional locks are off, so status does
	// not write the index of a repository it only reads.
	drop := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_COMMON_DIR": true, "GIT_NAMESPACE": true, "GIT_PREFIX": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			env = append(env, kv)
		}
	}
	env = append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	return &gitRunner{bin: path, env: env}
}

// run calls git in dir and returns its standard output and exit code. The
// config overrides keep a repository's own settings from changing the
// output's shape, and core.fsmonitor off means no hook program a repository
// names is ever run by a scan.
func (g *gitRunner) run(ctx context.Context, dir string, args ...string) ([]byte, int, error) {
	full := append([]string{
		"-C", dir,
		"-c", "core.fsmonitor=false",
		"-c", "core.quotepath=off",
		"-c", "color.ui=never",
		"-c", "color.grep=never",
		"-c", "grep.lineNumber=false",
		"-c", "grep.column=false",
		"-c", "grep.fullName=false",
	}, args...)
	cmd := exec.CommandContext(ctx, g.bin, full...)
	cmd.Env = g.env
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.Bytes(), ee.ExitCode(), nil
	}
	if err != nil {
		return nil, -1, err
	}
	return out.Bytes(), 0, nil
}

// toplevel is the real path of the repository holding dir, or "" when dir is
// in none git will open.
func (g *gitRunner) toplevel(ctx context.Context, dir string) string {
	out, code, err := g.run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || code != 0 {
		return ""
	}
	top := strings.TrimRight(string(out), "\n")
	if top == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(top); err == nil {
		top = r
	}
	return top
}

// repoInfo is what one status, one ls-files and the upstream comparison
// say about a repository.
type repoInfo struct {
	top       string
	tracked   map[string]bool
	changed   map[string]string // tracked paths that differ, to their XY code
	ignored   map[string]bool
	upstream  bool
	unpushed  map[string]bool // paths touched by commits not on the upstream
	scope     []string        // pathspecs limiting status and ls-files, empty for all
	statusErr bool
}

// loadRepo reads the repository's state. scope limits it to the scanned
// folders when the repository is larger than them.
func (g *gitRunner) loadRepo(ctx context.Context, top string, scope []string) *repoInfo {
	ri := &repoInfo{top: top, tracked: map[string]bool{}, changed: map[string]string{}, ignored: map[string]bool{}, unpushed: map[string]bool{}, scope: scope}
	spec := pathspecs(scope)

	out, code, err := g.run(ctx, top, append([]string{"status", "--porcelain=v1", "-z", "--ignored", "--untracked-files=all"}, spec...)...)
	if err != nil || code != 0 {
		ri.statusErr = true
		return ri
	}
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		e := fields[i]
		if len(e) < 4 {
			continue
		}
		xy, p := e[:2], e[3:]
		switch xy {
		case "??":
		case "!!":
			ri.ignored[p] = true
		default:
			ri.changed[p] = xy
			if strings.ContainsAny(xy, "RC") {
				i++ // the next field is the path it was renamed or copied from
			}
		}
	}

	out, code, err = g.run(ctx, top, append([]string{"ls-files", "-z"}, spec...)...)
	if err != nil || code != 0 {
		ri.statusErr = true
		return ri
	}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			ri.tracked[p] = true
		}
	}

	if _, code, err := g.run(ctx, top, "rev-parse", "--verify", "--quiet", "@{upstream}"); err != nil || code != 0 {
		return ri
	}
	out, code, err = g.run(ctx, top, append([]string{"log", "--format=", "--name-only", "-z", "--no-renames", "@{upstream}..HEAD"}, spec...)...)
	if err != nil || code != 0 {
		return ri
	}
	ri.upstream = true
	for _, p := range strings.Split(string(out), "\x00") {
		if p = strings.Trim(p, "\n"); p != "" {
			ri.unpushed[p] = true
		}
	}
	return ri
}

// pathspecs turns repository relative folders into literal pathspecs.
func pathspecs(scope []string) []string {
	if len(scope) == 0 {
		return nil
	}
	out := []string{"--"}
	for _, s := range scope {
		out = append(out, ":(literal)"+s)
	}
	return out
}

// state is the GitState of a file at rel, relative to the repository.
func (ri *repoInfo) state(rel string) GitState {
	gs := GitState{Repo: ri.top}
	switch {
	case ri.tracked[rel]:
		xy, changed := ri.changed[rel]
		gs.State = StateClean
		if changed {
			gs.State = StateModified
		}
		// A file only added to the index has no commit to be pushed.
		if ri.upstream && !(changed && xy[0] == 'A') {
			pushed := !ri.unpushed[rel]
			gs.Pushed = &pushed
		}
	case ri.ignored[rel]:
		gs.State = StateIgnored
	default:
		gs.State = StateUntracked
	}
	return gs
}
