package scan

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The secret patterns are those of Smrit's memory plugin, which is written in
// Python, ported to Go so that both refuse the same text. Go's regular
// expressions are RE2, with no lookbehind or lookahead, so each lookaround
// is a check in code around a match, with the same meaning. Three more of
// Python's rules differ from Go's and are spelled out here:
//
//   - \s in Python means any Unicode whitespace; in Go it is ASCII only.
//   - \b and \w in Python are Unicode word characters; in Go, ASCII.
//   - Python's IGNORECASE folds i with İ and ı, s with ſ and k with the
//     Kelvin sign K; Go's (?i) has no İ or ı, so no (?i) is used at all.
//
// testdata/secret_cases.json, generated from the Python module by
// scripts/secret_cases.py, holds the cases that prove the two agree.

// pyspace is the class body of Python's \s for text.
const pyspace = `\t\n\v\f\r\x1c-\x1f \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

// foldExtras are the characters IGNORECASE adds to [A-Za-z].
const foldExtras = `\x{130}\x{131}\x{17f}\x{212a}`

// ci spells a lower case literal as Python's IGNORECASE matches it.
func ci(lit string) string {
	var b strings.Builder
	for _, r := range lit {
		if r < 'a' || r > 'z' {
			b.WriteString(regexp.QuoteMeta(string(r)))
			continue
		}
		b.WriteByte('[')
		b.WriteRune(r)
		b.WriteRune(unicode.ToUpper(r))
		switch r {
		case 'i':
			b.WriteString(`\x{130}\x{131}`)
		case 's':
			b.WriteString(`\x{17f}`)
		case 'k':
			b.WriteString(`\x{212a}`)
		}
		b.WriteByte(']')
	}
	return b.String()
}

// plain holds every pattern without lookaround. They stay separate rather
// than one alternation: most start with a literal, which Go then finds with a
// fast substring search, several times quicker than one combined program.
var plain = compileAll([]string{
	`AKIA[0-9A-Z]{16}`,
	ci("aws_secret_access_key") + `[` + pyspace + `]*[:=][` + pyspace + `]*[^` + pyspace + `]{20}`,
	`gh[pousr]_[A-Za-z0-9]{36}`,
	`github_pat_[A-Za-z0-9_]{40}`,
	`glpat-[A-Za-z0-9_\-]{20}`,
	`npm_[A-Za-z0-9]{36}`,
	`sk-ant-[A-Za-z0-9_\-]{20}`,
	`[sr]k_live_[A-Za-z0-9]{16}`,
	`xox[abprs]-[A-Za-z0-9\-]{10}`,
	`xapp-[0-9]-[A-Za-z0-9\-]{10}`,
	`hooks\.slack\.com/services/`,
	`AIza[0-9A-Za-z_\-]{35}`,
	`ya29\.[A-Za-z0-9_\-]{20}`,
	`GOCSPX-[A-Za-z0-9_\-]{20}`,
	`-----BEGIN [A-Z ]*PRIVATE KEY( BLOCK)?-----`,
	`(?:` + ci("password") + `|` + ci("passwd") + `|` + ci("secret") + `|` + ci("api") + `[_ ]?` + ci("key") +
		`|` + ci("access key") + `|` + ci("client secret") + `)[` + pyspace + `]*[:=][` + pyspace + `]*[^` + pyspace + `]{8}`,
})

// plainNames and guardNames name the patterns above, in the same order, so a
// report can say which pattern matched without quoting the matching text.
var plainNames = []string{
	"aws-access-key-id", "aws-secret-access-key", "github-token", "github-pat", "gitlab-token",
	"npm-token", "anthropic-key", "stripe-live-key", "slack-token", "slack-app-token", "slack-webhook",
	"google-api-key", "google-oauth-token", "google-client-secret", "private-key", "password-assignment",
}

var guardNames = []string{
	"openai-project-key", "openai-key", "jwt", "smrit-token", "bearer-token", "url-credentials", "token-assignment",
}

// SecretPatterns returns the names of the patterns text matches, in a fixed
// order. It never returns any of the matching text.
func SecretPatterns(text string) []string {
	var out []string
	for i, re := range plain {
		if re.MatchString(text) {
			out = append(out, plainNames[i])
		}
	}
	for i, g := range guards {
		if g.find(text) {
			out = append(out, guardNames[i])
		}
	}
	return out
}

func compileAll(sources []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(sources))
	for i, s := range sources {
		out[i] = regexp.MustCompile(s)
	}
	return out
}

// guarded is a pattern whose Python form starts with a negative lookbehind
// (or \b, which before a word character is the same thing). The match must
// not be preceded by a character for which before is true, and check, when
// set, stands in for any lookahead or length limit RE2 cannot express.
//
// Every guarded pattern's first character is itself one before accepts. So
// when a match fails, every later start inside the same run of such
// characters fails the lookbehind too, and the search resumes after the run.
// That keeps the scan linear on long runs such as "eyJeyJeyJ...".
type guarded struct {
	re     *regexp.Regexp
	before func(r rune) bool
	check  func(text string, m []int) bool
}

func (g guarded) find(text string) bool {
	pos := 0
	for pos < len(text) {
		m := g.re.FindStringSubmatchIndex(text[pos:])
		if m == nil {
			return false
		}
		for i := range m {
			if m[i] >= 0 {
				m[i] += pos
			}
		}
		s := m[0]
		prev, _ := utf8.DecodeLastRuneInString(text[:s])
		if (s == 0 || !g.before(prev)) && (g.check == nil || g.check(text, m)) {
			return true
		}
		j := s
		for j < len(text) {
			r, size := utf8.DecodeRuneInString(text[j:])
			if !g.before(r) {
				break
			}
			j += size
		}
		if j == s {
			j++
		}
		pos = j
	}
	return false
}

// pyWord is Python's \w for text: letters, numbers and underscore.
func pyWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

func asciiIn(set string) func(rune) bool {
	return func(r rune) bool { return r < utf8.RuneSelf && strings.IndexByte(set, byte(r)) >= 0 }
}

const (
	lower  = "abcdefghijklmnopqrstuvwxyz"
	upper  = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digits = "0123456789"
)

// foldLetter is [A-Za-z] under Python's IGNORECASE.
func foldLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == 0x130 || r == 0x131 || r == 0x17f || r == 0x212a
}

// tokenChar is [A-Za-z0-9_\-+/=] under IGNORECASE.
func tokenChar(r rune) bool {
	return foldLetter(r) || (r >= '0' && r <= '9') || strings.ContainsRune("_-+/=", r)
}

var guards = []guarded{
	// \bsk-(?:proj|svcacct|admin)-[A-Za-z0-9_\-]{20,}
	{re: regexp.MustCompile(`sk-(?:proj|svcacct|admin)-[A-Za-z0-9_\-]{20}`), before: pyWord},
	// \bsk-[A-Za-z0-9]{32,}
	{re: regexp.MustCompile(`sk-[A-Za-z0-9]{32}`), before: pyWord},
	// (?<![A-Za-z0-9_\-])eyJ[...]{8,2048}\.eyJ[...]{8,2048}\.[...]{8,2048}
	// A segment runs up to the next dot, which is not a segment character, so
	// each group is the whole segment and its length is checked here. The
	// third segment's upper limit never decides whether there is a match.
	{
		re:     regexp.MustCompile(`eyJ([A-Za-z0-9_\-]+)\.eyJ([A-Za-z0-9_\-]+)\.[A-Za-z0-9_\-]{8}`),
		before: asciiIn(lower + upper + digits + "_-"),
		check: func(_ string, m []int) bool {
			a, b := m[3]-m[2], m[5]-m[4]
			return a >= 8 && a <= 2048 && b >= 8 && b <= 2048
		},
	},
	// (?<![A-Za-z0-9_])smrit_[A-Za-z0-9_\-]{40,}: a tool name such as
	// mcp__smrit__get_document_metadata never matches.
	{re: regexp.MustCompile(`smrit_[A-Za-z0-9_\-]{40}`), before: asciiIn(lower + upper + digits + "_")},
	// (?i)\bbearer\s+[A-Za-z0-9._~+/\-]{20,}=*
	{re: regexp.MustCompile(ci("bearer") + `[` + pyspace + `]+[A-Za-z0-9._~+/\-` + foldExtras + `]{20}`), before: pyWord},
	// (?<![a-z0-9+.\-])[a-z][a-z0-9+.\-]{0,30}://[^/\s:@]+:[^/\s@]+@
	{
		re:     regexp.MustCompile(`[a-z][a-z0-9+.\-]{0,30}://[^/` + pyspace + `:@]+:[^/` + pyspace + `@]+@`),
		before: asciiIn(lower + digits + "+.-"),
	},
	// (?i)(?<![A-Za-z0-9])token\s*[:=]\s*["']?
	//   (?=[A-Za-z0-9_\-+/=]{16})(?=[...]{0,48}\d)(?=[...]{0,48}[A-Za-z])[...]{16,256}
	{
		re:     regexp.MustCompile(ci("token") + `[` + pyspace + `]*[:=][` + pyspace + `]*["']?`),
		before: func(r rune) bool { return foldLetter(r) || (r >= '0' && r <= '9') },
		check:  func(text string, m []int) bool { return tokenValue(text[m[1]:]) },
	},
}

// tokenValue is the token rule's lookaheads at the start of value: 16 or
// more token characters, a digit among the first 49 characters (Python's \d
// is any Unicode decimal digit, so the character just after a short run
// counts too), and a letter among the first 49 token characters.
func tokenValue(value string) bool {
	n := 0
	digit, letter := false, false
	for _, r := range value {
		if n > 48 {
			break
		}
		if !tokenChar(r) {
			if unicode.IsDigit(r) {
				digit = true
			}
			break
		}
		if r >= '0' && r <= '9' {
			digit = true
		}
		if foldLetter(r) {
			letter = true
		}
		n++
	}
	return n >= 16 && digit && letter
}

// ContainsSecret reports whether text holds anything shaped like a
// credential, by the patterns above.
func ContainsSecret(text string) bool {
	for _, re := range plain {
		if re.MatchString(text) {
			return true
		}
	}
	for _, g := range guards {
		if g.find(text) {
			return true
		}
	}
	return false
}

// Files are scanned for secrets in windows, so a large file never sits in
// memory whole. Consecutive windows overlap, so a secret across an edge is
// still inside one window. A match longer than the overlap (a 64 KiB user
// name in a URL) across an edge would be missed, and a match starting at a
// window's first byte has no character before it to refuse it, so it may be
// flagged when the whole file would not be; flagging more is the safe side.
const (
	secretWindowSize    = 4 << 20
	secretWindowOverlap = 64 << 10
)

type secretWindow struct {
	buf           []byte
	size, overlap int
	fresh, found  bool
}

func newSecretWindow(size, overlap int) *secretWindow {
	return &secretWindow{size: size, overlap: overlap}
}

func (w *secretWindow) Write(p []byte) {
	for len(p) > 0 && !w.found {
		n := min(w.size-len(w.buf), len(p))
		w.buf = append(w.buf, p[:n]...)
		p = p[n:]
		w.fresh = true
		if len(w.buf) == w.size {
			w.scan()
			keep := w.buf[len(w.buf)-w.overlap:]
			w.buf = append(w.buf[:0], keep...)
			w.fresh = false
		}
	}
}

func (w *secretWindow) scan() {
	if !w.found && ContainsSecret(string(w.buf)) {
		w.found = true
	}
}

// Close scans what is left and reports whether any window held a secret.
func (w *secretWindow) Close() bool {
	if w.fresh {
		w.scan()
		w.fresh = false
	}
	return w.found
}
