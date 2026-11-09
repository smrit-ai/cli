package plan

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/smrit-ai/cli/internal/scan"
)

// Space is one space as MCP spaces lists it.
type Space struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
}

// Entry is a plan entry that passed every check, with what the scan found
// and the defaults filled in.
type Entry struct {
	Index int // position in the plan
	PlanEntry
	// Space is the space's slug, as resolved.
	Space string
	// Abs is the file's absolute path; Size and SHA256 are the rescan's.
	Abs    string
	Size   int64
	SHA256 string
}

// Refusal is a plan entry that cannot be imported, with one sentence.
type Refusal struct {
	Index  int
	Path   string
	Kind   string
	Space  string
	Reason string
}

// Validate checks every entry against res, a scan of the plan's root made
// now, never an earlier one. Nothing here is overridable. Entries and
// refusals come back in plan order within each list.
func Validate(ctx context.Context, p Plan, res scan.Result, spaces []Space, objectsAvailable bool) ([]Entry, []Refusal) {
	files := map[string]scan.File{}
	skipped := map[string]string{}
	for _, f := range res.Files {
		files[f.Path] = f
	}
	for _, s := range res.Skipped {
		skipped[s.Path] = s.Reason
	}
	bySlug := map[string]int{}
	byPath := map[string]int{}
	for _, e := range p.Entries {
		byPath[e.Path]++
		if e.Kind == scan.KindDocument {
			if s := effectiveSlug(e); s != "" {
				bySlug[s]++
			}
		}
	}

	var ok []Entry
	var bad []Refusal
	for i, pe := range p.Entries {
		if ctx.Err() != nil {
			break
		}
		refuse := func(format string, a ...any) {
			bad = append(bad, Refusal{Index: i, Path: pe.Path, Kind: pe.Kind, Space: pe.Space, Reason: fmt.Sprintf(format, a...)})
		}
		e := Entry{Index: i, PlanEntry: pe}
		if reason := pathProblem(pe.Path); reason != "" {
			refuse("%s", reason)
			continue
		}
		if byPath[pe.Path] > 1 {
			refuse("The plan lists this path more than once.")
			continue
		}
		f, found := files[pe.Path]
		if !found {
			if skipped[pe.Path] == scan.SkipSymlink {
				refuse("This file is a symlink, and smrit never follows one.")
			} else if res.Truncated {
				refuse("The folder has more than %d files and smrit reads only the first %d, so this file was cut off. Plan a smaller folder.", scan.DefaultMaxFiles, scan.DefaultMaxFiles)
			} else {
				refuse("This file is not in the folder the plan names, or it cannot be read.")
			}
			continue
		}
		if has(f.Flags, scan.FlagSecret) {
			refuse("This file now looks like it holds a secret, so it is never imported.")
			continue
		}
		if has(f.Flags, scan.FlagSensitiveName) {
			refuse("This file's name marks it as sensitive, so it is never imported.")
			continue
		}
		e.Abs = filepath.Join(f.Root, filepath.FromSlash(f.Path))
		e.Size, e.SHA256 = f.Size, f.SHA256

		switch pe.Kind {
		case scan.KindDocument, scan.KindWorkbook:
			if f.Kind != pe.Kind {
				refuse("A %s cannot be made from this kind of file.", pe.Kind)
				continue
			}
			if has(f.Flags, scan.FlagNotUTF8) {
				refuse("This file is not UTF-8 text.")
				continue
			}
			if pe.Kind == scan.KindDocument && f.Size > scan.MaxDocumentBytes {
				refuse("This document is over 512 KiB.")
				continue
			}
			sp, reason := findSpace(spaces, pe.Space)
			if reason != "" {
				refuse("%s", reason)
				continue
			}
			e.Space = sp.Slug
			if pe.Kind == scan.KindDocument {
				if pe.Type == "" {
					refuse("A document needs a type.")
					continue
				}
				if pe.Slug != "" && !slugOK.MatchString(pe.Slug) {
					refuse("The slug may hold only letters, digits, hyphens and underscores, up to 200.")
					continue
				}
				e.Slug = effectiveSlug(pe)
				if bySlug[e.Slug] > 1 {
					refuse("Another entry in the plan has the slug %s.", e.Slug)
					continue
				}
				if e.Title == "" {
					e.Title = f.Heading
				}
				if e.Title == "" {
					e.Title = Stem(pe.Path)
				}
			} else if pe.Title == "" {
				refuse("A workbook needs a title.")
				continue
			}
		case scan.KindObject:
			if f.Kind != scan.KindObject {
				refuse("An object cannot be made from this kind of file.")
				continue
			}
			if !objectsAvailable {
				refuse("This organization does not offer objects.")
				continue
			}
			if pe.Scope != "organization" && pe.Scope != "document" {
				refuse("An object needs a scope, organization or document.")
				continue
			}
			if e.Name == "" {
				e.Name = path.Base(pe.Path)
			}
		default:
			refuse("The kind %q is not document, workbook or object.", pe.Kind)
			continue
		}
		ok = append(ok, e)
	}
	ok, bad = refuseTitleClashes(ok, bad)
	return ok, bad
}

// maxBaseSlug is how much of a slug Smrit compares when it refuses a
// document under a name its space already has.
const maxBaseSlug = 193

// refuseTitleClashes refuses each workbook whose describing document Smrit
// would refuse as a duplicate: its base slug, derived from its title as
// Smrit derives it, is another workbook's or a plan document's in the same
// space (content/services.py, _refuse_duplicate). Titles alone, or the same
// title in two spaces, are no clash. Only an ASCII title is compared, the
// one case where TitleSlug is Smrit's slug exactly: a title outside ASCII
// is left to Smrit, whose duplicate refusal reports the workbook exists and
// writes nothing for it. A document's slug is ASCII by the plan's own rule.
func refuseTitleClashes(ok []Entry, bad []Refusal) ([]Entry, []Refusal) {
	type key struct{ space, slug string }
	books := map[key]int{}
	docs := map[key]bool{}
	for _, e := range ok {
		switch e.Kind {
		case scan.KindWorkbook:
			if isASCII(e.Title) {
				books[key{e.Space, TitleSlug(e.Title)}]++
			}
		case scan.KindDocument:
			docs[key{e.Space, baseSlug(e.Slug)}] = true
		}
	}
	var kept []Entry
	clashed := false
	for _, e := range ok {
		reason := ""
		if e.Kind == scan.KindWorkbook && isASCII(e.Title) {
			switch k := (key{e.Space, TitleSlug(e.Title)}); {
			case docs[k]:
				reason = "A document in the plan has the name Smrit would store this workbook's document under in this space."
			case books[k] > 1:
				reason = "Another workbook in the plan has a title Smrit would store under the same name in this space."
			}
		}
		if reason == "" {
			kept = append(kept, e)
			continue
		}
		clashed = true
		bad = append(bad, Refusal{Index: e.Index, Path: e.Path, Kind: e.Kind, Space: e.PlanEntry.Space, Reason: reason})
	}
	if clashed {
		slices.SortStableFunc(bad, func(a, b Refusal) int { return a.Index - b.Index })
	}
	return kept, bad
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// TitleSlug is the slug Smrit derives from a title when it is given none,
// as Django's slugify does for ASCII: letters, digits, underscores and
// hyphens kept and lowercased, each run of spaces and hyphens one hyphen,
// hyphens and underscores trimmed from the ends, cut to 193. It is exact
// only for an ASCII title: Smrit first folds an accented letter to its base
// letter (NFKD), which this drops instead, so it is never used to refuse a
// title outside ASCII.
func TitleSlug(title string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			if gap && b.Len() > 0 {
				b.WriteByte('-')
			}
			gap = false
			b.WriteRune(r)
		case r == '-', r == ' ', r == '\t', r == '\n', r == '\r', r == '\v', r == '\f':
			gap = true
		}
	}
	s := strings.Trim(b.String(), "-_")
	s = strings.TrimRight(baseSlug(s), "-")
	if s == "" {
		return "document"
	}
	return s
}

func baseSlug(s string) string {
	s = strings.ToLower(s)
	if len(s) > maxBaseSlug {
		s = s[:maxBaseSlug]
	}
	return s
}

func effectiveSlug(e PlanEntry) string {
	if e.Slug != "" {
		return strings.ToLower(e.Slug)
	}
	return DefaultSlug(e.Path)
}

func has(flags []string, f string) bool {
	for _, x := range flags {
		if x == f {
			return true
		}
	}
	return false
}

// pathProblem is a sentence when p is not a clean path below the root.
func pathProblem(p string) string {
	const outside = "This path is outside the plan's folder."
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return outside
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "This path holds a control character."
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return outside
		}
	}
	return ""
}

func findSpace(spaces []Space, key string) (Space, string) {
	if key == "" {
		return Space{}, "The entry names no space."
	}
	for _, s := range spaces {
		if s.Slug == key || s.ID == key {
			if s.Visibility == "public" {
				return Space{}, "The Smrit CLI does not import into a public space. Publish from the web app."
			}
			return s, ""
		}
	}
	return Space{}, "No space you can see is named " + quote(key) + "."
}

func quote(s string) string {
	if len(s) > 60 {
		s = s[:60]
	}
	return fmt.Sprintf("%q", s)
}
