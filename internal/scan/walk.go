package scan

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// skipDirs are folder names never walked into, besides any starting with a dot.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "dist": true, "build": true, "target": true,
	"__pycache__": true, "venv": true, ".venv": true,
}

// resolveRoots checks each root and returns its real path. Refused, each
// with one sentence: a root that is itself a symbolic link; the whole disk;
// the home folder or any folder above it; and any root inside a dot folder,
// such as ~/.ssh or ~/.config/x. A document an agent reads could ask it to
// scan any of those.
//
// The home check compares folders, not names: on macOS one folder has many
// names (/Users/pat, /users/pat on a case insensitive disk,
// /System/Volumes/Data/Users/pat through a firmlink), and EvalSymlinks
// changes none of them. So the root is compared by identity (homeGuard),
// and the walk checks every folder it enters the same way.
func resolveRoots(roots []string, home string) ([]string, *homeGuard, error) {
	if len(roots) == 0 {
		return nil, nil, usage("Name at least one folder to scan, as in smrit scan ./docs.")
	}
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return nil, nil, refused("smrit scan could not find your home folder, so it cannot check that a folder is not it.")
		}
		home = h
	}
	guard, err := newHomeGuard(home)
	if err != nil {
		return nil, nil, err
	}
	realHome := guard.path
	diskRoot, _ := os.Stat(string(filepath.Separator))

	var out []string
	var infos []os.FileInfo
	for _, given := range roots {
		abs, err := filepath.Abs(filepath.Clean(given))
		if err != nil {
			return nil, nil, usage("smrit scan cannot read the path %s.", given)
		}
		st, err := os.Lstat(abs)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, usage("There is no folder at %s.", given)
		}
		if err != nil {
			return nil, nil, usage("smrit scan cannot read %s.", given)
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return nil, nil, refused("smrit scan does not follow the symbolic link %s, so name the folder it points to.", given)
		}
		if !st.IsDir() {
			return nil, nil, usage("%s is a file, and smrit scan takes folders.", given)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, nil, usage("smrit scan cannot read %s.", given)
		}
		if real == string(filepath.Separator) || (diskRoot != nil && os.SameFile(st, diskRoot)) {
			return nil, nil, refused("smrit scan will not scan the whole disk, so name the folder that holds your documents.")
		}
		if contains(real, realHome) || guard.holds(abs, st) {
			return nil, nil, refused("smrit scan will not scan %s because it is or holds your home folder, so name a folder inside it.", given)
		}
		for _, part := range strings.Split(real, string(filepath.Separator)) {
			if strings.HasPrefix(part, ".") {
				return nil, nil, refused("smrit scan will not scan %s because hidden folders such as %s hold credentials and settings, not documents.", given, part)
			}
		}
		out = append(out, real)
		infos = append(infos, st)
	}
	for i := range out {
		for j := range out {
			if i != j && holds(infos[i], out[j]) {
				return nil, nil, usage("The folders %s and %s overlap, so scan only the outer one.", roots[i], roots[j])
			}
		}
	}
	return out, guard, nil
}

// holds reports whether the folder a is b or a folder above it, comparing
// folders rather than names.
func holds(a os.FileInfo, b string) bool {
	for d := b; ; d = filepath.Dir(d) {
		if st, err := os.Stat(d); err == nil && os.SameFile(a, st) {
			return true
		}
		if d == filepath.Dir(d) {
			return false
		}
	}
}

// contains reports whether folder a is b or holds b.
func contains(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// walked is one regular file the walk found.
type walked struct {
	root, abs, rel string
}

// homeGuard recognises the home folder by identity, under any name.
type homeGuard struct {
	path  string        // the home folder's real path
	home  os.FileInfo   // nil when it cannot be read
	parts []string      // path's components
	above []os.FileInfo // home and every folder above it, up to /
}

func newHomeGuard(home string) (*homeGuard, error) {
	p, err := filepath.EvalSymlinks(home)
	if err != nil {
		p = filepath.Clean(home)
	}
	g := &homeGuard{path: p}
	g.home, _ = os.Stat(p)
	for d := p; ; d = filepath.Dir(d) {
		if st, err := os.Stat(d); err == nil {
			g.above = append(g.above, st)
		}
		if d == filepath.Dir(d) {
			break
		}
	}
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		if part != "" {
			g.parts = append(g.parts, part)
		}
	}
	return g, nil
}

// holds reports whether dir (whose Lstat is info) is the home folder, a
// folder above it, or a folder holding it under another name: a volume
// root such as /System/Volumes/Data holds /Users/pat as Users/pat, which
// no walk up from the home folder ever passes. That last case is found by
// looking for the end of the home folder's path inside dir.
func (g *homeGuard) holds(dir string, info os.FileInfo) bool {
	for _, a := range g.above {
		if os.SameFile(info, a) {
			return true
		}
	}
	if g.home == nil {
		return false
	}
	for k := range g.parts {
		st, err := os.Stat(filepath.Join(dir, filepath.Join(g.parts[k:]...)))
		if err == nil && os.SameFile(st, g.home) {
			return true
		}
	}
	return false
}

// walk lists the regular files under root, never following a symbolic link
// and never entering a skipped folder. It stops after limit files in all,
// counting those already in files, and reports whether it had to. It is
// refused when it enters a folder that is or holds the home folder by
// identity, which catches any alias the root checks miss (a mount point, a
// firmlink); nothing has been read by then, since files are read after the
// walk.
func walk(ctx context.Context, root string, limit int, files *[]walked, skipped *[]Skipped, leftovers *[]Leftover, guard *homeGuard) (truncated bool, err error) {
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if werr != nil {
			if p == root {
				return werr
			}
			*skipped = append(*skipped, Skipped{Root: root, Path: rel, Reason: SkipUnreadable})
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if p == root {
			return nil
		}
		name := d.Name()
		if name == ".git" {
			// A repository's own folder, or the .git file of a worktree or
			// submodule: never content, never reported.
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		switch t := d.Type(); {
		case t&fs.ModeSymlink != 0:
			*skipped = append(*skipped, Skipped{Root: root, Path: rel, Reason: SkipSymlink})
			return nil
		case d.IsDir():
			if strings.HasPrefix(name, ".") || skipDirs[name] {
				*skipped = append(*skipped, Skipped{Root: root, Path: rel, Reason: SkipList})
				return filepath.SkipDir
			}
			if info, err := d.Info(); err == nil && guard != nil && guard.holds(p, info) {
				return refused("smrit scan stopped at %s because that folder is or holds your home folder, so name a folder that does not.", p)
			}
			return nil
		case !t.IsRegular():
			*skipped = append(*skipped, Skipped{Root: root, Path: rel, Reason: SkipNotRegular})
			return nil
		case IsRetireTemp(name):
			// Never read and never a file: a leftover is listed, not imported.
			*leftovers = append(*leftovers, Leftover{Root: root, Path: rel, Folder: filepath.Dir(p), Note: LeftoverNote})
			return nil
		}
		if len(*files) >= limit {
			truncated = true
			return filepath.SkipAll
		}
		*files = append(*files, walked{root: root, abs: p, rel: rel})
		return nil
	})
	return truncated, err
}
