package scan

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// minUsedPattern is the shortest path searched for. A path of one to three
// characters (a, ab, x.c) appears inside so much unrelated text that every
// such file would be marked used, so it is not searched at all.
const minUsedPattern = 4

// gitPathLists are never searched: a path named in .gitignore or
// .gitattributes is one git ignores or treats specially, not one anything
// reads.
var gitPathLists = []string{":(exclude,glob)**/.gitignore", ":(exclude,glob)**/.gitattributes"}

// usedFile is one scanned file in a repository, by both of its names.
// importable is a document, workbook or object: what an import moves.
type usedFile struct {
	repoRel, rootRel string
	importable       bool
}

// findUsed returns the indexes of files whose repository relative or root
// relative path appears, as a literal string, in a tracked file of the same
// repository that is not itself a scanned document, workbook or object. A
// scanned file of any other kind, a config, a script or code, is searched
// even inside the root: a mkdocs.yml at the repository top still names the
// pages beside it. Any file can be marked used, whatever its kind. It makes
// one git grep call for the repository. ok is false when git grep failed.
func (g *gitRunner) findUsed(ctx context.Context, ri *repoInfo, files []usedFile) (used map[int]bool, ok bool) {
	used = map[int]bool{}
	byPattern := map[string][]int{}
	for i, f := range files {
		for _, p := range []string{f.repoRel, f.rootRel} {
			if len(p) < minUsedPattern || strings.ContainsAny(p, "\n\r") {
				continue
			}
			if l := byPattern[p]; len(l) == 0 || l[len(l)-1] != i {
				byPattern[p] = append(byPattern[p], i)
			}
		}
	}
	if len(byPattern) == 0 {
		return used, true
	}
	excluded := map[string]bool{}
	for _, f := range files {
		if f.importable {
			excluded[f.repoRel] = true
		}
	}
	excludes, anyLeft := excludeSpecs(ri.tracked, excluded, ri.scope)
	if !anyLeft {
		return used, true // every tracked file is excluded; nothing else can name one
	}

	patterns := make([]string, 0, len(byPattern))
	for p := range byPattern {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	dir, err := os.MkdirTemp("", "smrit-scan-")
	if err != nil {
		return used, false
	}
	defer os.RemoveAll(dir)
	pf := filepath.Join(dir, "patterns")
	if err := os.WriteFile(pf, []byte(strings.Join(patterns, "\n")+"\n"), 0o600); err != nil {
		return used, false
	}
	args := []string{"grep", "-I", "-o", "-h", "-F", "-f", pf, "--"}
	args = append(args, gitPathLists...)
	args = append(args, excludes...)
	out, code, err := g.run(ctx, ri.top, args...)
	if err != nil || (code != 0 && code != 1) {
		return used, false
	}
	found := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if line != "" {
			found[line] = true
		}
	}
	// git grep -o prints the leftmost longest match, so a path inside a
	// longer one that also matched (guide.md inside docs/guide.md) is not
	// printed by itself. It still appears as a literal string, so each match
	// also counts every pattern it contains.
	for p := range containedPatterns(found, patterns) {
		for _, i := range byPattern[p] {
			used[i] = true
		}
	}
	return used, true
}

// containedPatterns returns every pattern that is a substring of a found
// match. Patterns are indexed by their last four bytes, so each match is
// checked only against patterns that could fit in it.
func containedPatterns(found map[string]bool, patterns []string) map[string]bool {
	byTail := map[string][]string{}
	for _, p := range patterns {
		t := p[len(p)-minUsedPattern:]
		byTail[t] = append(byTail[t], p)
	}
	out := map[string]bool{}
	for m := range found {
		for i := minUsedPattern; i <= len(m); i++ {
			for _, p := range byTail[m[i-minUsedPattern:i]] {
				if !out[p] && len(p) <= i && m[i-len(p):i] == p {
					out[p] = true
				}
			}
		}
	}
	return out
}

// excludeSpecs returns pathspecs excluding every tracked file in scanned,
// the set to leave out of the search. A folder whose tracked files are all
// in the set is excluded as one pathspec, so the argument list stays short.
// anyLeft is false when no tracked file is left to search.
//
// When scope is set, tracked holds only the files under those folders, so
// only a folder inside the scope can be known to be fully in the set, and
// there may always be tracked files left outside it.
func excludeSpecs(tracked, scanned map[string]bool, scope []string) (specs []string, anyLeft bool) {
	inScope := func(d string) bool {
		if len(scope) == 0 {
			return true
		}
		for _, s := range scope {
			if d == s || strings.HasPrefix(d, s+"/") {
				return true
			}
		}
		return false
	}
	anyLeft = len(scope) > 0
	// full[d] is false once any tracked file under folder d was not scanned.
	full := map[string]bool{}
	for t := range tracked {
		in := scanned[t]
		if !in {
			anyLeft = true
		}
		for d := path.Dir(t); ; d = path.Dir(d) {
			if v, seen := full[d]; !seen {
				full[d] = in
			} else if v && !in {
				full[d] = false
			}
			if d == "." {
				break
			}
		}
	}
	if !anyLeft {
		return nil, false
	}
	seen := map[string]bool{}
	for t := range tracked {
		if !scanned[t] {
			continue
		}
		// The highest folder below the top whose tracked files were all scanned.
		target := t
		for d := path.Dir(t); d != "."; d = path.Dir(d) {
			if full[d] && inScope(d) {
				target = d
			}
		}
		if !seen[target] {
			seen[target] = true
			specs = append(specs, ":(exclude,literal)"+target)
		}
	}
	sort.Strings(specs)
	return specs, true
}
