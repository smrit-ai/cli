package plan

import (
	"path"
	"regexp"
	"strings"
)

// MaxSlug is the longest slug.
const MaxSlug = 200

var slugOK = regexp.MustCompile(`^[-a-zA-Z0-9_]{1,200}$`)

// DefaultSlug is the path without its extension, with separators and any
// character outside [-a-zA-Z0-9_] turned to a hyphen, lowercased, at most 200
// characters.
func DefaultSlug(p string) string {
	if ext := path.Ext(p); ext != "" && ext != p {
		p = strings.TrimSuffix(p, ext)
	}
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.ToLower(b.String())
	if len(s) > MaxSlug {
		s = s[:MaxSlug]
	}
	return s
}

// Stem is the base name of a path without its extension.
func Stem(p string) string {
	b := path.Base(p)
	if ext := path.Ext(b); ext != "" && ext != b {
		b = strings.TrimSuffix(b, ext)
	}
	return b
}
