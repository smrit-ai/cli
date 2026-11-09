package scan

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

// Repo is a repository git may run in: its own config was read first and
// names no programs. It is for commands after the scan, such as retire,
// which must ask git about a path through the same hardened runner.
type Repo struct {
	// Top is the repository's top folder, as its real path.
	Top string
	g   *gitRunner
}

// OpenRepo returns the repository holding dir. When that repository's own
// config names programs it returns nil and distrusted true, and no git
// command but git config ran. It returns nil and false when git is missing
// or dir is in no repository git will open.
func OpenRepo(ctx context.Context, dir string, opts Options) (repo *Repo, distrusted bool) {
	g := newGit(opts.Git)
	if g == nil {
		return nil, false
	}
	if g.namesPrograms(ctx, dir) {
		return nil, true
	}
	top := g.toplevel(ctx, dir)
	if top == "" {
		return nil, false
	}
	return &Repo{Top: top, g: g}, false
}

var errGit = errors.New("git could not answer")

// Ignored reports whether git ignores rel, a path relative to the top with
// forward slashes. check-ignore takes no literal pathspecs, so the path is
// given as ./rel, where a leading colon cannot start pathspec magic.
func (r *Repo) Ignored(ctx context.Context, rel string) (bool, error) {
	_, code, err := r.g.run(ctx, r.Top, "check-ignore", "-q", "--", "./"+rel)
	switch {
	case err != nil:
		return false, err
	case code == 0:
		return true, nil
	case code == 1:
		return false, nil
	}
	return false, errGit
}

// Tracked reports whether the index holds rel, read literally.
func (r *Repo) Tracked(ctx context.Context, rel string) (bool, error) {
	out, code, err := r.g.run(ctx, r.Top, "--literal-pathspecs", "ls-files", "-z", "--", rel)
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, errGit
	}
	for _, p := range strings.Split(string(out), "\x00") {
		if p == rel {
			return true, nil
		}
	}
	return false, nil
}

// GitPath is git rev-parse --git-path name, as an absolute path: where the
// repository keeps name, such as info/exclude, in a worktree as well.
func (r *Repo) GitPath(ctx context.Context, name string) (string, error) {
	out, code, err := r.g.run(ctx, r.Top, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	p := strings.TrimRight(string(out), "\n")
	if code != 0 || p == "" || strings.ContainsAny(p, "\n\x00") {
		return "", errGit
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.Top, p)
	}
	return filepath.Clean(p), nil
}
