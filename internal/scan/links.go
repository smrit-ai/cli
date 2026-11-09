package scan

import (
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Links are inline Markdown links, [text](target), and images,
// ![alt](target), outside fenced code blocks and inline code spans.
// Reference style links ([text][ref] with a [ref]: target line elsewhere)
// are not handled: they are ignored, not reported. Autolinks (<https://...>)
// and raw HTML are not links here either.

// fenceOpen returns the fence a line opens (its run of backticks or
// tildes), or "" when it opens none. line has its indentation removed; any
// indentation is accepted, so a fence inside a list item counts too.
func fenceOpen(line string) string {
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(line) && line[n] == c {
			n++
		}
		if n >= 3 {
			if c == '`' && strings.IndexByte(line[n:], '`') >= 0 {
				return "" // a backtick fence's info string has no backtick
			}
			return line[:n]
		}
	}
	return ""
}

// isFenceClose reports whether line closes fence: the same character, at
// least as many, and nothing else.
func isFenceClose(line, fence string) bool {
	n := 0
	for n < len(line) && line[n] == fence[0] {
		n++
	}
	return n >= len(fence) && strings.TrimSpace(line[n:]) == ""
}

// fencedRanges are the byte ranges of body inside fenced code blocks,
// fence lines included. A fence never closed runs to the end.
func fencedRanges(body string) [][2]int {
	var out [][2]int
	fence, start := "", 0
	for off := 0; off < len(body); {
		end := strings.IndexByte(body[off:], '\n')
		next := len(body)
		if end >= 0 {
			next = off + end + 1
		}
		line := strings.TrimLeft(strings.TrimRight(body[off:next], "\r\n"), " \t")
		if fence == "" {
			if f := fenceOpen(line); f != "" {
				fence, start = f, off
			}
		} else if isFenceClose(line, fence) {
			out = append(out, [2]int{start, next})
			fence = ""
		}
		off = next
	}
	if fence != "" {
		out = append(out, [2]int{start, len(body)})
	}
	return out
}

// linkScanner walks body, skipping fences and code spans.
type linkScanner struct {
	body   string
	fences [][2]int
}

// fenceAt returns the end of the fence holding i, or -1.
func (s *linkScanner) fenceAt(i int) int {
	for _, f := range s.fences {
		if i >= f[0] && i < f[1] {
			return f[1]
		}
	}
	return -1
}

// codeSpanEnd returns the index just past the code span opening at i (a run
// of backticks), or, when the run is never closed, just past the run, with
// ok false.
func (s *linkScanner) codeSpanEnd(i int) (int, bool) {
	b := s.body
	n := 0
	for i+n < len(b) && b[i+n] == '`' {
		n++
	}
	for j := i + n; j < len(b); {
		if b[j] != '`' {
			if s.fenceAt(j) >= 0 {
				break
			}
			j++
			continue
		}
		m := 0
		for j+m < len(b) && b[j+m] == '`' {
			m++
		}
		if m == n {
			return j + m, true
		}
		j += m
	}
	return i + n, false
}

// rawLink is one link as written, before it is resolved.
type rawLink struct {
	text, target string
	image        bool
}

// scan finds the links in body[from:to].
func (s *linkScanner) scan(from, to int) []rawLink {
	b := s.body
	var out []rawLink
	for i := from; i < to; {
		if end := s.fenceAt(i); end >= 0 {
			i = end
			continue
		}
		switch c := b[i]; {
		case c == '\\':
			i += 2
		case c == '`':
			i, _ = s.codeSpanEnd(i)
		case c == '[' || (c == '!' && i+1 < to && b[i+1] == '['):
			image := c == '!'
			open := i
			if image {
				open++
			}
			l, next, ok := s.link(open, to)
			if !ok {
				i = open + 1
				continue
			}
			l.image = image
			out = append(out, l)
			if !image {
				// An image inside a link's text, as in a badge, is a link too.
				out = append(out, s.scan(open+1, open+1+len(l.text))...)
			}
			i = next
		default:
			i++
		}
	}
	for k := range out {
		out[k].text = strings.TrimSpace(out[k].text)
	}
	return out
}

// link parses [text](target "title") with the [ at open. It returns the
// link and the index just past it.
func (s *linkScanner) link(open, to int) (rawLink, int, bool) {
	b := s.body
	depth := 0
	i := open
	closeAt := -1
	for i < to {
		if s.fenceAt(i) >= 0 {
			return rawLink{}, 0, false
		}
		switch b[i] {
		case '\\':
			i += 2
			continue
		case '`':
			if end, ok := s.codeSpanEnd(i); ok {
				i = end
				continue
			}
		case '[':
			depth++
		case ']':
			depth--
		}
		if depth == 0 {
			closeAt = i
			break
		}
		i++
	}
	if closeAt < 0 || closeAt+1 >= to || b[closeAt+1] != '(' {
		return rawLink{}, 0, false
	}
	text := b[open+1 : closeAt]
	i = closeAt + 2
	i = skipSpace(b, i, to)
	var target string
	if i < to && b[i] == '<' {
		end := strings.IndexAny(b[i+1:to], "<>\n")
		if end < 0 || b[i+1+end] != '>' {
			return rawLink{}, 0, false
		}
		target = b[i+1 : i+1+end]
		i = i + 1 + end + 1
	} else {
		start, parens := i, 0
		for i < to {
			c := b[i]
			if c == '\\' && i+1 < to {
				i += 2
				continue
			}
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c < 0x20 {
				break
			}
			if c == '(' {
				parens++
			} else if c == ')' {
				if parens == 0 {
					break
				}
				parens--
			}
			i++
		}
		target = b[start:i]
	}
	i = skipSpace(b, i, to)
	if i < to && (b[i] == '"' || b[i] == '\'' || b[i] == '(') {
		closer := b[i]
		if closer == '(' {
			closer = ')'
		}
		end := strings.IndexByte(b[i+1:to], closer)
		if end < 0 {
			return rawLink{}, 0, false
		}
		i = skipSpace(b, i+1+end+1, to)
	}
	if i >= to || b[i] != ')' {
		return rawLink{}, 0, false
	}
	return rawLink{text: text, target: unescape(target)}, i + 1, true
}

// skipSpace skips spaces and tabs and at most one line ending.
func skipSpace(b string, i, to int) int {
	newline := false
	for i < to {
		switch b[i] {
		case ' ', '\t':
		case '\r':
		case '\n':
			if newline {
				return i
			}
			newline = true
		default:
			return i
		}
		i++
	}
	return i
}

// unescape removes the backslash before ASCII punctuation, as Markdown does.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", s[i+1]) >= 0 {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

var schemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]{1,31}:`)

// parseLinks returns the links in a Markdown body, resolved against rel,
// the file's root relative path.
func parseLinks(body, rel string) []Link {
	s := &linkScanner{body: body, fences: fencedRanges(body)}
	raw := s.scan(0, len(body))
	links := make([]Link, 0, len(raw))
	for _, r := range raw {
		links = append(links, resolveLink(r, rel))
	}
	return links
}

func resolveLink(r rawLink, rel string) Link {
	l := Link{Text: r.text, Target: r.target, Image: r.image}
	p := r.target
	if i := strings.IndexByte(p, '#'); i >= 0 {
		l.Anchor = p[i:]
		p = p[:i]
	}
	if schemeRE.MatchString(r.target) || strings.HasPrefix(r.target, "//") {
		l.External = true
		return l
	}
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	// A bare #anchor points into the same file and has no path to resolve.
	if p == "" {
		return l
	}
	if u, err := url.PathUnescape(p); err == nil {
		p = u
	}
	var joined string
	if strings.HasPrefix(p, "/") {
		// A leading slash means the top of the site, taken as the root.
		joined = path.Clean(strings.TrimLeft(p, "/"))
	} else {
		joined = path.Join(path.Dir(rel), p)
	}
	if joined == ".." || strings.HasPrefix(joined, "../") || joined == "" {
		return l
	}
	l.Resolved = joined
	return l
}
