package scan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"syscall"
	"unicode/utf8"
)

var objectExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".svg": true,
	".bmp": true, ".tif": true, ".tiff": true, ".ico": true, ".heic": true, ".heif": true,
	".avif": true, ".pdf": true,
}

// kindOf is the kind by extension; binary says whether the content looked
// like anything but text, which makes an unknown extension an object.
func kindOf(name string, binary bool) string {
	switch ext := strings.ToLower(path.Ext(name)); {
	case ext == ".md" || ext == ".markdown" || ext == ".txt":
		return KindDocument
	case ext == ".csv":
		return KindWorkbook
	case objectExt[ext] || binary:
		return KindObject
	}
	return KindOther
}

// textByName reports a name whose kind is text whatever its bytes.
func textByName(name string) bool {
	k := kindOf(name, false)
	return k == KindDocument || k == KindWorkbook
}

func isMarkdown(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".md" || ext == ".markdown"
}

// sensitiveName reports a base name that usually holds a credential. It is
// matched without regard to case, so Credentials.json counts.
func sensitiveName(base string) bool {
	b := strings.ToLower(base)
	switch b {
	case ".npmrc", ".pypirc", ".netrc":
		return true
	}
	for _, p := range []string{".env", "id_rsa", "id_ed25519", "kubeconfig"} {
		if strings.HasPrefix(b, p) {
			return true
		}
	}
	for _, s := range []string{".pem", ".key", ".p12", ".pfx"} {
		if strings.HasSuffix(b, s) {
			return true
		}
	}
	return strings.Contains(b, "credentials") || strings.Contains(b, "secret")
}

// keepFile reports a file that is never retired: read mes, licences and the
// instruction files agents read in place. abs is the file's absolute path,
// slash separated, so .github/copilot-instructions.md is found even when
// the root is the .github folder itself.
func keepFile(abs string) bool {
	base := strings.ToUpper(path.Base(abs))
	for _, p := range []string{"README", "LICENSE", "LICENCE", "COPYING"} {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	switch base {
	case "AGENTS.MD", "CLAUDE.MD", "GEMINI.MD", ".CURSORRULES":
		return true
	}
	return strings.HasSuffix(strings.ToLower(abs), "/.github/copilot-instructions.md")
}

// sniffBytes is how much of the start of a file decides whether it is
// binary, the same amount git looks at.
const sniffBytes = 8000

// headingBytes is how much of a document is read for its heading.
const headingBytes = 64 << 10

// facts is what reading one file found.
type facts struct {
	size    int64
	sha256  string
	binary  bool
	notUTF8 bool
	secret  bool
	head    []byte // at most headingBytes
	body    []byte // the whole file, kept only for a Markdown file within MaxDocumentBytes
}

var errNotRegular = errors.New("not a regular file")

// readFacts reads the file once, streaming: its hash, whether it is text,
// whether it is valid UTF-8, and whether any window holds a secret. A file
// is scanned for secrets when its start has no NUL byte, or always when
// textByName says its name makes it a document or workbook.
//
// O_NOFOLLOW covers only the last component of the path: a file swapped
// for a symbolic link after the walk is refused, but a parent folder
// swapped for one is followed. That needs a local process racing the scan
// with write access to the folder, which is out of scope. O_NONBLOCK means
// a FIFO swapped in after the walk cannot block the open; the walk already
// skips every file that is not regular.
func readFacts(abs string, keepBody, textByName bool) (facts, error) {
	var fx facts
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fx, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fx, err
	}
	if !st.Mode().IsRegular() {
		return fx, errNotRegular
	}
	keepBody = keepBody && st.Size() <= MaxDocumentBytes

	h := sha256.New()
	first := make([]byte, headingBytes)
	n, err := io.ReadFull(f, first)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return fx, err
	}
	first = first[:n]
	fx.head = first
	fx.binary = bytes.IndexByte(first[:min(n, sniffBytes)], 0) >= 0
	var window *secretWindow
	if !fx.binary || textByName {
		window = newSecretWindow(secretWindowSize, secretWindowOverlap)
	}
	var utf utf8Stream
	var body []byte
	take := func(p []byte) {
		h.Write(p)
		fx.size += int64(len(p))
		utf.Write(p)
		if window != nil {
			window.Write(p)
		}
		if keepBody {
			body = append(body, p...)
		}
	}
	take(first)
	buf := make([]byte, 256<<10)
	for {
		m, err := f.Read(buf)
		if m > 0 {
			take(buf[:m])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fx, err
		}
	}
	fx.sha256 = hex.EncodeToString(h.Sum(nil))
	fx.notUTF8 = !utf.Valid()
	if window != nil {
		fx.secret = window.Close()
	}
	if keepBody && fx.size <= MaxDocumentBytes {
		fx.body = body
	}
	return fx, nil
}

// utf8Stream checks UTF-8 validity across reads, carrying a rune split
// between two reads into the next.
type utf8Stream struct {
	carry []byte
	bad   bool
}

func (u *utf8Stream) Write(p []byte) {
	if u.bad {
		return
	}
	data := append(u.carry, p...)
	cut := len(data)
	// A rune is at most 4 bytes, so an incomplete one starts in the last 3.
	for i := len(data) - 1; i >= 0 && i >= len(data)-3; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				cut = i
			}
			break
		}
	}
	if !utf8.Valid(data[:cut]) {
		u.bad = true
		return
	}
	u.carry = append([]byte(nil), data[cut:]...)
}

func (u *utf8Stream) Valid() bool { return !u.bad && len(u.carry) == 0 }

// maxTextRunes caps a heading and a link's text: both are untrusted text
// from the file, going into an agent's context.
const maxTextRunes = 200

// capText cuts s to maxTextRunes characters, the last an ellipsis.
func capText(s string) string {
	if utf8.RuneCountInString(s) <= maxTextRunes {
		return s
	}
	n := 0
	for i := range s {
		if n == maxTextRunes-1 {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

// headingOf is the front matter title, else the first ATX heading of any
// level, one to six, read from the start of a document.
func headingOf(head []byte, truncated bool) string {
	text := string(head)
	if truncated {
		// The last line may be cut short; leave it out.
		if i := strings.LastIndexByte(text, '\n'); i >= 0 {
			text = text[:i]
		}
	}
	text = strings.TrimPrefix(text, "\uFEFF")
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	start := 0
	if len(lines) > 0 && strings.TrimRight(lines[0], " \t") == "---" {
		for i := 1; i < len(lines); i++ {
			l := strings.TrimRight(lines[i], " \t")
			if l == "---" || l == "..." {
				start = i + 1
				for _, fm := range lines[1:i] {
					if v, ok := strings.CutPrefix(fm, "title:"); ok {
						if t := unquote(strings.TrimSpace(v)); t != "" {
							return capText(t)
						}
					}
				}
				break
			}
		}
	}
	var fence string
	for _, l := range lines[start:] {
		trimmed := strings.TrimLeft(l, " \t")
		if fence != "" {
			if isFenceClose(trimmed, fence) {
				fence = ""
			}
			continue
		}
		if f := fenceOpen(trimmed); f != "" {
			fence = f
			continue
		}
		if len(l)-len(strings.TrimLeft(l, " ")) > 3 {
			continue
		}
		level := 0
		for level < len(trimmed) && trimmed[level] == '#' {
			level++
		}
		if rest := trimmed[level:]; level >= 1 && level <= 6 && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
			rest = strings.TrimSpace(rest)
			// A closing sequence of #s after a space is not part of it.
			if i := strings.LastIndexAny(rest, " \t"); i >= 0 && strings.Trim(rest[i+1:], "#") == "" {
				rest = strings.TrimSpace(rest[:i])
			} else if strings.Trim(rest, "#") == "" {
				rest = ""
			}
			if rest != "" {
				return capText(rest)
			}
		}
	}
	return ""
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}
