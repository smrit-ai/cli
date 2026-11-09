// Package historyscan looks for credential-shaped text in every file version
// ever committed to a git repository, using the same patterns the smrit
// command uses to refuse a file. It is a repository check run by CI and is not
// part of the smrit binary.
//
// A report names the commit, the path and the pattern, and never the text that
// matched. Known fake values in test fixtures are allowed by a list of path,
// pattern and blob id lines, each covering one version of one file; anything
// else, a later version of a listed file included, is a finding.
package historyscan

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/smrit-ai/cli/internal/scan"
)

// Allow is one permitted hit: a file path, the pattern allowed in it, and
// the one version of the file, by blob id, the permission covers.
type Allow struct{ Path, Pattern, Blob string }

// Hit is one blob that matched one pattern.
type Hit struct {
	Commit  string
	Blob    string
	Path    string
	Pattern string
	Allowed bool
}

// Result is what a scan found.
type Result struct {
	Commits int
	Blobs   int
	Hits    []Hit
}

// Findings are the hits no allowlist entry covers.
func (r Result) Findings() []Hit {
	var out []Hit
	for _, h := range r.Hits {
		if !h.Allowed {
			out = append(out, h)
		}
	}
	return out
}

// blobID is a full git object id, SHA-1 or SHA-256, in lower case.
var blobID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// ParseAllowlist reads lines of "path pattern blob". Blank lines and lines
// starting with # are ignored. A line covers that one blob only, so a key
// committed later into an allowlisted file is still a finding.
func ParseAllowlist(text string) ([]Allow, error) {
	var out []Allow
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 {
			return nil, fmt.Errorf("allowlist line %d: want \"path pattern blob\"", i+1)
		}
		if !blobID.MatchString(f[2]) {
			return nil, fmt.Errorf("allowlist line %d: %q is not a blob id", i+1, f[2])
		}
		out = append(out, Allow{f[0], f[1], f[2]})
	}
	return out, nil
}

func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Scan reads every blob reachable from any ref of the repository in dir.
func Scan(dir string, allow []Allow) (Result, error) {
	var res Result
	commits, err := git(dir, "rev-list", "--all")
	if err != nil {
		return res, err
	}
	res.Commits = len(strings.Fields(string(commits)))

	// Every path each blob has had, with the first commit that held it there,
	// oldest first. rev-list --objects names a blob under one path only, so two
	// files with identical content would otherwise be reported as one.
	log, err := git(dir, "log", "--all", "--reverse", "--no-renames", "-m", "--root", "-r", "--raw", "--no-abbrev", "--format=commit %H")
	if err != nil {
		return res, err
	}
	type where struct{ commit, path string }
	places := map[string][]where{}
	cur := ""
	for _, line := range strings.Split(string(log), "\n") {
		if strings.HasPrefix(line, "commit ") {
			cur = strings.TrimPrefix(line, "commit ")
			continue
		}
		if !strings.HasPrefix(line, ":") {
			continue
		}
		meta, path, ok := strings.Cut(line, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) < 5 {
			continue
		}
		dup := false
		for _, w := range places[f[3]] {
			dup = dup || w.path == path
		}
		if !dup {
			places[f[3]] = append(places[f[3]], where{cur, path})
		}
	}

	objs, err := git(dir, "rev-list", "--all", "--objects")
	if err != nil {
		return res, err
	}
	type blob struct{ sha, path string }
	var blobs []blob
	for _, line := range strings.Split(string(objs), "\n") {
		sha, path, ok := strings.Cut(line, " ")
		if ok && path != "" {
			blobs = append(blobs, blob{sha, path})
		}
	}

	cmd := exec.Command("git", "cat-file", "--batch")
	cmd.Dir = dir
	in, _ := cmd.StdinPipe()
	outp, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		return res, err
	}
	go func() {
		for _, b := range blobs {
			fmt.Fprintln(in, b.sha)
		}
		in.Close()
	}()
	rd := bufio.NewReaderSize(outp, 1<<20)
	seen := map[string]bool{}
	for _, b := range blobs {
		header, err := rd.ReadString('\n')
		if err != nil {
			return res, fmt.Errorf("reading git cat-file: %v", err)
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			continue // a missing object, or a tree reached by path
		}
		size, _ := strconv.Atoi(f[2])
		data := make([]byte, size+1)
		if _, err := io.ReadFull(rd, data); err != nil {
			return res, err
		}
		if f[1] != "blob" {
			continue
		}
		res.Blobs++
		spots := places[b.sha]
		if len(spots) == 0 {
			spots = []where{{"", b.path}}
		}
		for _, name := range scan.SecretPatterns(string(data[:size])) {
			for _, w := range spots {
				key := b.sha + " " + w.path + " " + name
				if seen[key] {
					continue
				}
				seen[key] = true
				res.Hits = append(res.Hits, Hit{Commit: w.commit, Blob: b.sha, Path: w.path, Pattern: name, Allowed: allowed(allow, b.sha, w.path, name)})
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		return res, err
	}
	sort.SliceStable(res.Hits, func(i, j int) bool {
		a, b := res.Hits[i], res.Hits[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Pattern < b.Pattern
	})
	return res, nil
}

func allowed(list []Allow, blob, path, pattern string) bool {
	for _, a := range list {
		if a.Blob == blob && a.Path == path && a.Pattern == pattern {
			return true
		}
	}
	return false
}

// Lines are the allowlist lines that would allow every hit, sorted, one per
// path, pattern and blob.
func Lines(r Result) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range r.Hits {
		l := h.Path + " " + h.Pattern + " " + h.Blob
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}
