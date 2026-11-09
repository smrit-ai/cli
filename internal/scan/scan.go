// Package scan lists a docs folder for an agent to plan an import from: each
// file's size, hash, heading, kind, git state, links and flags. It never
// reports a file's body. The flags are safety rails the later steps rely on:
// a file flagged secret or sensitive_name is never imported, and one flagged
// keep or used is never retired.
package scan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// DefaultMaxFiles is how many files one scan lists before it stops and sets
// Truncated.
const DefaultMaxFiles = 5000

// Kinds.
const (
	KindDocument = "document"
	KindWorkbook = "workbook"
	KindObject   = "object"
	KindOther    = "other"
)

// Git states.
const (
	StateClean     = "clean"
	StateModified  = "modified"
	StateUntracked = "untracked"
	StateIgnored   = "ignored"
	StateOutside   = "outside"
)

// Flags, in the order a file's Flags lists them.
const (
	FlagSecret        = "secret"
	FlagSensitiveName = "sensitive_name"
	FlagUsed          = "used"
	FlagKeep          = "keep"
	FlagNotUTF8       = "not_utf8"
	FlagTooLarge      = "too_large"
)

// Reasons a path is skipped.
const (
	SkipSymlink    = "symlink"
	SkipList       = "skip_list"
	SkipNotRegular = "not_regular"
	SkipUnreadable = "unreadable"
)

// MaxDocumentBytes is the largest document Smrit imports; a larger one is
// flagged too_large.
const MaxDocumentBytes = 512 * 1024

// Options adjusts a scan. The zero value is the real behaviour.
type Options struct {
	// MaxFiles is the file limit; 0 means DefaultMaxFiles.
	MaxFiles int
	// Git is the git binary; empty means git on PATH.
	Git string
	// Home is the home folder no root may be or contain; empty means the
	// user's home folder.
	Home string
}

// Root is one folder that was scanned, as its real path.
type Root struct {
	Path string `json:"path"`
}

// Result is everything a scan found.
type Result struct {
	Roots     []Root     `json:"roots"`
	Files     []File     `json:"files"`
	Skipped   []Skipped  `json:"skipped"`
	Leftovers []Leftover `json:"leftovers"`
	Truncated bool       `json:"truncated"`
	// Notes are sentences about the scan as a whole, such as git missing.
	Notes []string `json:"notes"`
}

// File is one regular file under a root.
type File struct {
	// Root is the real path of the root the file was found under; Path is
	// relative to it, with forward slashes.
	Root    string   `json:"root"`
	Path    string   `json:"path"`
	Size    int64    `json:"size"`
	SHA256  string   `json:"sha256"`
	Heading string   `json:"heading"`
	Kind    string   `json:"kind"`
	Git     GitState `json:"git"`
	Links   []Link   `json:"links"`
	Flags   []string `json:"flags"`
}

// GitState is where a file stands in its repository.
type GitState struct {
	// State is clean, modified, untracked, ignored or outside.
	State string `json:"state"`
	// Repo is the repository's top folder, empty when outside.
	Repo string `json:"repo"`
	// Pushed is whether the last commit touching the file is on the
	// branch's upstream; null without an upstream or without a commit.
	Pushed *bool `json:"pushed"`
	// Unknown, for a file listed as outside, says why it may in fact be in
	// a repository: one whose config names programs, one git could not
	// read, or git missing. Empty means no repository holds it. It is not
	// in the scan's JSON; retire reads it so it never treats such a file
	// as outside every repository.
	Unknown string `json:"-"`
}

// Why a file listed as outside may be in a repository after all.
const (
	UnknownNamesPrograms = "names_programs"
	UnknownUnreadable    = "unreadable"
	UnknownNoGit         = "no_git"
)

// Link is one inline Markdown link or image.
type Link struct {
	Text   string `json:"text"`
	Target string `json:"target"`
	Image  bool   `json:"image"`
	// Resolved is the target as a root relative path when it stays inside
	// the root, and empty otherwise. It is lexical: the file may not exist.
	Resolved string `json:"resolved"`
	// Anchor is the #... part of the target, empty when there is none.
	Anchor   string `json:"anchor"`
	External bool   `json:"external"`
}

// Skipped is a path the scan did not list, and why.
type Skipped struct {
	Root   string `json:"root"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// RetireTempPrefix and RetireTempSuffix frame the name retire gives a file
// it moves aside before removing it.
const (
	RetireTempPrefix = ".smrit-retire-"
	RetireTempSuffix = ".tmp"
)

// LeftoverNote is what a leftover says to a person.
const LeftoverNote = "This file holds a file smrit retire was moving aside when it stopped, so look inside and give it back its name or delete it yourself."

// Leftover is a file retire moved aside and never finished with, left by a
// crash between the move and the unlink. It holds a person's file under a
// temporary name, so it is never listed as a file: nothing imports, zips or
// deletes it.
type Leftover struct {
	Root string `json:"root"`
	// Path is relative to Root, with forward slashes; Folder is the real
	// path of the folder holding it.
	Path   string `json:"path"`
	Folder string `json:"folder"`
	Note   string `json:"note"`
}

// IsRetireTemp reports whether name is a name retire gives a file it moves
// aside.
func IsRetireTemp(name string) bool {
	return len(name) > len(RetireTempPrefix)+len(RetireTempSuffix) &&
		strings.HasPrefix(name, RetireTempPrefix) && strings.HasSuffix(name, RetireTempSuffix)
}

// RefusedError is a root the scan will not read at all. Its message is one
// sentence for a person.
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return e.Msg }

// UsageError is a command line that cannot be scanned as given.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

func refused(format string, a ...any) error { return &RefusedError{fmt.Sprintf(format, a...)} }
func usage(format string, a ...any) error   { return &UsageError{fmt.Sprintf(format, a...)} }

// CheckRoots reports whether Run would read the roots at all, as the same
// RefusedError or UsageError Run gives, without reading any file.
func CheckRoots(roots []string, opts Options) error {
	_, _, err := resolveRoots(roots, opts.Home)
	return err
}

// Run scans the roots and returns what it found. It reads every file but
// reports only facts about it, never its body.
func Run(ctx context.Context, roots []string, opts Options) (Result, error) {
	res := Result{Roots: []Root{}, Files: []File{}, Skipped: []Skipped{}, Leftovers: []Leftover{}, Notes: []string{}}
	real, guard, err := resolveRoots(roots, opts.Home)
	if err != nil {
		return res, err
	}
	limit := opts.MaxFiles
	if limit <= 0 {
		limit = DefaultMaxFiles
	}
	var found []walked
	for _, r := range real {
		res.Roots = append(res.Roots, Root{Path: r})
		if res.Truncated {
			continue
		}
		t, err := walk(ctx, r, limit, &found, &res.Skipped, &res.Leftovers, guard)
		if err != nil {
			return res, err
		}
		res.Truncated = res.Truncated || t
	}

	g := newGit(opts.Git)
	if g == nil {
		res.Notes = append(res.Notes, noGitNote)
	}
	tops, unknown, flagged := repoTops(ctx, g, found)
	for _, dir := range flagged {
		res.Notes = append(res.Notes, fmt.Sprintf("The repository holding %s names programs in its own git config, so smrit scan ran no git command there and lists its files as outside.", dir))
	}

	// One status and one ls-files per repository. A repository found at or
	// inside a root is read whole; one above a root only under the roots, so
	// a home folder kept in git is never read in full.
	type need struct {
		whole bool
		scope []string
	}
	needs := map[string]*need{}
	var order []string
	for i, w := range found {
		top := tops[i]
		if top == "" {
			continue
		}
		n := needs[top]
		if n == nil {
			n = &need{}
			needs[top] = n
			order = append(order, top)
		}
		if top != w.root && contains(top, w.root) {
			rel, _ := filepath.Rel(top, w.root)
			if rel = filepath.ToSlash(rel); !slices.Contains(n.scope, rel) {
				n.scope = append(n.scope, rel)
			}
		} else {
			n.whole = true
		}
	}
	repos := map[string]*repoInfo{}
	for _, top := range order {
		scope := needs[top].scope
		if needs[top].whole {
			scope = nil
		}
		ri := g.loadRepo(ctx, top, scope)
		if ri.statusErr {
			res.Notes = append(res.Notes, fmt.Sprintf("git could not read the repository at %s, so its files are listed as outside.", top))
			continue
		}
		repos[top] = ri
	}

	for i, w := range found {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		f := File{Root: w.root, Path: w.rel, Links: []Link{}, Flags: []string{}, Git: GitState{State: StateOutside}}
		fx, err := readFacts(w.abs, isMarkdown(w.rel), textByName(w.rel))
		if err != nil {
			reason := SkipUnreadable
			switch {
			case errors.Is(err, syscall.ELOOP):
				reason = SkipSymlink // swapped for a link after the walk
			case errors.Is(err, errNotRegular):
				reason = SkipNotRegular
			}
			res.Skipped = append(res.Skipped, Skipped{Root: w.root, Path: w.rel, Reason: reason})
			continue
		}
		f.Size, f.SHA256 = fx.size, fx.sha256
		f.Kind = kindOf(w.rel, fx.binary)
		if ri := repos[tops[i]]; ri != nil {
			rel, _ := filepath.Rel(ri.top, w.abs)
			f.Git = ri.state(filepath.ToSlash(rel))
		}
		switch {
		case g == nil:
			f.Git.Unknown = UnknownNoGit
		case unknown[i] != "":
			f.Git.Unknown = unknown[i]
		case tops[i] != "" && repos[tops[i]] == nil:
			f.Git.Unknown = UnknownUnreadable
		}
		text := f.Kind == KindDocument || f.Kind == KindWorkbook
		if fx.secret {
			f.Flags = append(f.Flags, FlagSecret)
		}
		if sensitiveName(path.Base(w.rel)) {
			f.Flags = append(f.Flags, FlagSensitiveName)
		}
		if keepFile(filepath.ToSlash(w.abs)) {
			f.Flags = append(f.Flags, FlagKeep)
		}
		if text && fx.notUTF8 {
			f.Flags = append(f.Flags, FlagNotUTF8)
		}
		if f.Kind == KindDocument && f.Size > MaxDocumentBytes {
			f.Flags = append(f.Flags, FlagTooLarge)
		}
		// A file holding a secret reports no heading and no links: either
		// could carry the secret itself into the agent's context.
		if !fx.secret {
			if f.Kind == KindDocument {
				f.Heading = headingOf(fx.head, fx.size > int64(len(fx.head)))
			}
			if fx.body != nil {
				f.Links = parseLinks(string(fx.body), w.rel)
			}
		}
		res.Files = append(res.Files, f)
	}

	markUsed(ctx, g, &res, repos)
	for i := range res.Files {
		sortFlags(res.Files[i].Flags)
	}
	return res, nil
}

// repoTops finds the repository of each walked file: git's answer for each
// root, and for a folder below a root holding its own .git (a nested
// repository or a submodule), git's answer there. Other folders inherit
// their parent's. "" is outside.
//
// Before git is asked anything else about a folder, its repository's own
// config is read (namesPrograms, which runs nothing). A folder whose
// repository names programs is outside, with everything under it, and is
// returned in flagged; no other git command ever runs there.
//
// unknown says, for each file, why an outside file may be in a repository
// after all: its repository names programs, or a .git git would not open.
func repoTops(ctx context.Context, g *gitRunner, found []walked) (tops, unknown, flagged []string) {
	tops = make([]string, len(found))
	unknown = make([]string, len(found))
	if g == nil {
		return tops, unknown, nil
	}
	type answer struct{ top, unknown string }
	memo := map[string]answer{}
	var of func(dir, root string) answer
	of = func(dir, root string) answer {
		if a, ok := memo[dir]; ok {
			return a
		}
		var a answer
		_, err := os.Lstat(filepath.Join(dir, ".git"))
		switch {
		case dir != root && err != nil:
			a = of(filepath.Dir(dir), root)
		case g.namesPrograms(ctx, dir):
			flagged = append(flagged, dir)
			a.unknown = UnknownNamesPrograms
		default:
			a.top = g.toplevel(ctx, dir)
			if a.top == "" && err == nil {
				a.unknown = UnknownUnreadable
			}
		}
		memo[dir] = a
		return a
	}
	for i, w := range found {
		a := of(filepath.Dir(w.abs), w.root)
		tops[i], unknown[i] = a.top, a.unknown
	}
	return tops, unknown, flagged
}

// markUsed sets the used flag, one git grep per repository.
func markUsed(ctx context.Context, g *gitRunner, res *Result, repos map[string]*repoInfo) {
	if g == nil {
		return
	}
	byRepo := map[string][]int{}
	var order []string
	for i, f := range res.Files {
		if f.Git.Repo == "" {
			continue
		}
		if _, ok := byRepo[f.Git.Repo]; !ok {
			order = append(order, f.Git.Repo)
		}
		byRepo[f.Git.Repo] = append(byRepo[f.Git.Repo], i)
	}
	for _, top := range order {
		idx := byRepo[top]
		files := make([]usedFile, len(idx))
		for k, i := range idx {
			f := res.Files[i]
			rel, _ := filepath.Rel(top, filepath.Join(f.Root, filepath.FromSlash(f.Path)))
			files[k] = usedFile{repoRel: filepath.ToSlash(rel), rootRel: f.Path}
		}
		used, ok := g.findUsed(ctx, repos[top], files)
		if !ok {
			res.Notes = append(res.Notes, fmt.Sprintf("git grep failed in %s, so none of its files is marked used.", top))
			continue
		}
		for k := range used {
			res.Files[idx[k]].Flags = append(res.Files[idx[k]].Flags, FlagUsed)
		}
	}
}

var flagOrder = map[string]int{FlagSecret: 0, FlagSensitiveName: 1, FlagUsed: 2, FlagKeep: 3, FlagNotUTF8: 4, FlagTooLarge: 5}

func sortFlags(flags []string) {
	slices.SortFunc(flags, func(a, b string) int { return flagOrder[a] - flagOrder[b] })
}
