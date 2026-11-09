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

// resolveRoots checks each root and returns its real path. A root that is
// the whole disk, the home folder, or a folder holding the home folder is
// refused, and so is a root that is itself a symbolic link: a document read
// by an agent could ask it to scan ~/.ssh, and a scan of the home folder
// would reach it.
func resolveRoots(roots []string, home string) ([]string, error) {
	if len(roots) == 0 {
		return nil, usage("Name at least one folder to scan, as in smrit scan ./docs.")
	}
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return nil, refused("smrit scan could not find your home folder, so it cannot check that a folder is not it.")
		}
		home = h
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		realHome = filepath.Clean(home)
	}
	var out []string
	for _, given := range roots {
		abs, err := filepath.Abs(filepath.Clean(given))
		if err != nil {
			return nil, usage("smrit scan cannot read the path %s.", given)
		}
		st, err := os.Lstat(abs)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, usage("There is no folder at %s.", given)
		}
		if err != nil {
			return nil, usage("smrit scan cannot read %s.", given)
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return nil, refused("smrit scan does not follow the symbolic link %s, so name the folder it points to.", given)
		}
		if !st.IsDir() {
			return nil, usage("%s is a file, and smrit scan takes folders.", given)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, usage("smrit scan cannot read %s.", given)
		}
		if real == string(filepath.Separator) {
			return nil, refused("smrit scan will not scan the whole disk, so name the folder that holds your documents.")
		}
		if contains(real, realHome) {
			return nil, refused("smrit scan will not scan %s because it is or holds your home folder, so name a folder inside it.", given)
		}
		out = append(out, real)
	}
	for i := range out {
		for j := range out {
			if i != j && contains(out[i], out[j]) {
				return nil, usage("The folders %s and %s overlap, so scan only the outer one.", roots[i], roots[j])
			}
		}
	}
	return out, nil
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

// walk lists the regular files under root, never following a symbolic link
// and never entering a skipped folder. It stops after limit files in all,
// counting those already in files, and reports whether it had to.
func walk(ctx context.Context, root string, limit int, files *[]walked, skipped *[]Skipped) (truncated bool, err error) {
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
			return nil
		case !t.IsRegular():
			*skipped = append(*skipped, Skipped{Root: root, Path: rel, Reason: SkipNotRegular})
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
