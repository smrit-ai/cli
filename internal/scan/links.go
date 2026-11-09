package scan

import (
	"math/bits"
	"net/url"
	"path"
	"regexp"
	"sort"
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
func fencedRanges(body string, work *int) [][2]int {
	var out [][2]int
	fence, start := "", 0
	for off := 0; off < len(body); {
		end := strings.IndexByte(body[off:], '\n')
		next := len(body)
		if end >= 0 {
			next = off + end + 1
		}
		*work += next - off
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

// linkScanner finds the links in a body. It works on one stretch between
// fences at a time, so no position it visits is inside a fence. Code spans
// and bracket pairs are found first, in one pass each, so the link pass
// only looks them up: the parse is linear in the body's size, whatever the
// body holds. work counts every step of every loop, so a test can check
// that without timing anything.
type linkScanner struct {
	body string
	// codeEnd maps the start of a backtick run to the end of its code span,
	// or to the end of the run when it opens none.
	codeEnd map[int]int
	// closeOf maps a [ to its matching ].
	closeOf map[int]int
	// at lists the positions of each byte that can end or cut a title.
	at   map[byte][]int
	work int
}

// rawLink is one link as written, before it is resolved.
type rawLink struct {
	text, target string
	image        bool
	// start and end are where the target sits in the body, as written:
	// inside the angle brackets when it has them.
	start, end int
}

// maxDestinationParens bounds the nesting of parentheses in a link
// destination, as CommonMark allows, so a destination scan never runs on
// through the rest of a long line.
const maxDestinationParens = 32

// prepare finds the code spans and bracket pairs in body[from:to].
func (s *linkScanner) prepare(from, to int) {
	b := s.body
	type run struct{ start, end int }
	var runs []run
	for i := from; i < to; {
		s.work++
		switch b[i] {
		case '\\':
			i += 2
		case '`':
			j := i
			for j < to && b[j] == '`' {
				s.work++
				j++
			}
			runs = append(runs, run{i, j})
			i = j
		default:
			i++
		}
	}
	// A run opens a code span closed by the next run of the same length.
	next := make([]int, len(runs))
	last := map[int]int{}
	for k := len(runs) - 1; k >= 0; k-- {
		s.work++
		n := runs[k].end - runs[k].start
		next[k] = -1
		if j, ok := last[n]; ok {
			next[k] = j
		}
		last[n] = k
	}
	for k := 0; k < len(runs); {
		s.work++
		if j := next[k]; j >= 0 {
			s.codeEnd[runs[k].start] = runs[j].end
			k = j + 1
		} else {
			s.codeEnd[runs[k].start] = runs[k].end
			k++
		}
	}
	var stack []int
	for i := from; i < to; {
		s.work++
		switch b[i] {
		case '\\':
			i += 2
			continue
		case '`':
			if e, ok := s.codeEnd[i]; ok {
				i = e
				continue
			}
		case '[':
			stack = append(stack, i)
		case ']':
			if len(stack) > 0 {
				s.closeOf[stack[len(stack)-1]] = i
				stack = stack[:len(stack)-1]
			}
		}
		i++
	}
}

// scan finds the links in body[from:to]. Inside a link's text only images
// count (a badge), since a link cannot hold another link.
func (s *linkScanner) scan(from, to int, imagesOnly bool) []rawLink {
	b := s.body
	var out []rawLink
	for i := from; i < to; {
		s.work++
		switch c := b[i]; {
		case c == '\\':
			i += 2
		case c == '`':
			if e, ok := s.codeEnd[i]; ok && e > i {
				i = e
			} else {
				i++
			}
		case c == '!' && i+1 < to && b[i+1] == '[':
			l, next, ok := s.link(i+1, to)
			if !ok {
				i += 2
				continue
			}
			l.image = true
			out = append(out, l)
			i = next
		case c == '[' && !imagesOnly:
			l, next, ok := s.link(i, to)
			if !ok {
				i++
				continue
			}
			out = append(out, l)
			out = append(out, s.scan(i+1, i+1+len(l.text), true)...)
			i = next
		default:
			i++
		}
	}
	return out
}

// link parses [text](target "title") with the [ at open. It returns the
// link and the index just past it.
func (s *linkScanner) link(open, to int) (rawLink, int, bool) {
	b := s.body
	closeAt, ok := s.closeOf[open]
	if !ok || closeAt+1 >= to || b[closeAt+1] != '(' {
		return rawLink{}, 0, false
	}
	text := b[open+1 : closeAt]
	i := s.skipSpace(closeAt+2, to)
	var target string
	var tStart, tEnd int
	if i < to && b[i] == '<' {
		j := i + 1
		for j < to && b[j] != '<' && b[j] != '>' && b[j] != '\n' {
			s.work++
			j++
		}
		if j >= to || b[j] != '>' {
			return rawLink{}, 0, false
		}
		target = b[i+1 : j]
		tStart, tEnd = i+1, j
		i = j + 1
	} else {
		start, parens := i, 0
		for i < to {
			s.work++
			c := b[i]
			if c == '\\' && i+1 < to {
				i += 2
				continue
			}
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c < 0x20 {
				break
			}
			if c == '(' {
				if parens++; parens > maxDestinationParens {
					return rawLink{}, 0, false
				}
			} else if c == ')' {
				if parens == 0 {
					break
				}
				parens--
			}
			i++
		}
		target = b[start:i]
		tStart, tEnd = start, i
	}
	i = s.skipSpace(i, to)
	if i < to && (b[i] == '"' || b[i] == '\'' || b[i] == '(') {
		closer := b[i]
		if closer == '(' {
			closer = ')'
		}
		// A title ends on its own line and before the next [. Without that,
		// every link start in a long line would reach for the same far
		// closer and skip the same run after it again.
		end := s.nextAt(closer, i+1)
		if end < 0 || end >= to {
			return rawLink{}, 0, false
		}
		if nl := s.nextAt('\n', i+1); nl >= 0 && nl < end {
			return rawLink{}, 0, false
		}
		if br := s.nextAt('[', i+1); br >= 0 && br < end {
			return rawLink{}, 0, false
		}
		i = s.skipSpace(end+1, to)
	}
	if i >= to || b[i] != ')' {
		return rawLink{}, 0, false
	}
	return rawLink{text: text, target: unescape(target), start: tStart, end: tEnd}, i + 1, true
}

// nextAt is the first position at or after from holding c, or -1, found by
// binary search in the positions listed once for the whole body.
func (s *linkScanner) nextAt(c byte, from int) int {
	list := s.at[c]
	s.work += bits.Len(uint(len(list))) + 1
	k := sort.SearchInts(list, from)
	if k == len(list) {
		return -1
	}
	return list[k]
}

// skipSpace skips spaces and tabs and at most one line ending.
func (s *linkScanner) skipSpace(i, to int) int {
	b := s.body
	newline := false
	for i < to {
		s.work++
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
	l, _ := parseLinksCounting(body, rel)
	return l
}

// LinkSpan is a link with the byte range its target occupies in the body,
// as written, so a caller can replace the target in place.
type LinkSpan struct {
	Link
	Start, End int
}

// LinkSpans returns the links parseLinks finds in body, in the same order,
// each with where its target sits. A fenced block or a code span holds none,
// so a rewrite by these ranges never touches code. A link inside an image's
// text comes after the link holding it, and no two spans overlap.
func LinkSpans(body, rel string) []LinkSpan {
	raw, _ := rawLinks(body)
	out := make([]LinkSpan, 0, len(raw))
	for _, r := range raw {
		out = append(out, LinkSpan{Link: resolveLink(r, rel), Start: r.start, End: r.end})
	}
	return out
}

// parseLinksCounting is parseLinks, also returning how many steps it took.
func parseLinksCounting(body, rel string) ([]Link, int) {
	raw, work := rawLinks(body)
	links := make([]Link, 0, len(raw))
	for _, r := range raw {
		links = append(links, resolveLink(r, rel))
	}
	return links, work
}

// rawLinks finds the links in body as written, and how many steps it took.
func rawLinks(body string) ([]rawLink, int) {
	s := &linkScanner{body: body, codeEnd: map[int]int{}, closeOf: map[int]int{}, at: map[byte][]int{}}
	for i := 0; i < len(body); i++ {
		s.work++
		if c := body[i]; c == '"' || c == '\'' || c == ')' || c == '\n' || c == '[' {
			s.at[c] = append(s.at[c], i)
		}
	}
	var raw []rawLink
	from := 0
	for _, f := range append(fencedRanges(body, &s.work), [2]int{len(body), len(body)}) {
		if f[0] > from {
			s.prepare(from, f[0])
			raw = append(raw, s.scan(from, f[0], false)...)
		}
		from = f[1]
	}
	return raw, s.work
}

func resolveLink(r rawLink, rel string) Link {
	l := Link{Text: capText(strings.TrimSpace(r.text)), Target: r.target, Image: r.image}
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
