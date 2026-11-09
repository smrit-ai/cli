package importer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/smrit-ai/cli/internal/plan"
)

// A CSV cell is checked here as Smrit's write_records checks it
// (api/smrit/workbooks/records.py, coerce_value and the add loop), so a cell
// Smrit would refuse is refused before anything is written, never after
// some rows are in. Where Smrit is lenient in ways no CSV needs, this is
// stricter, never looser.

// cellValue is one cell: what is sent, and the JSON Smrit stores for it,
// whose length counts against the row limit.
type cellValue struct {
	wire   any
	stored string
}

// cell is one CSV cell as column c (column number i from 1) takes it. ok
// false leaves it out, which Smrit stores as no value. reason, when set,
// ends a sentence saying why the cell does not fit; it never repeats the
// cell.
func cell(s string, c plan.Column) (v cellValue, ok bool, reason string) {
	if c.Type == "text" {
		if s == "" {
			return v, false, ""
		}
		if hiddenOrControl(s) {
			return v, false, "carries a hidden or control character"
		}
		return cellValue{s, jsonString(s)}, true, ""
	}
	t := strings.TrimSpace(s)
	if t == "" {
		return v, false, ""
	}
	switch c.Type {
	case "number":
		stored, ok := storedNumberOK(t)
		if !ok {
			return v, false, "is not a number Smrit can store"
		}
		return cellValue{json.Number(t), stored}, true, ""
	case "bool":
		switch strings.ToLower(t) {
		case "true":
			return cellValue{true, "true"}, true, ""
		case "false":
			return cellValue{false, "false"}, true, ""
		}
		return v, false, "is not true or false"
	case "option":
		for i, x := range c.Values {
			if x == t {
				// Smrit stores an option's key, o1 for the first value of a
				// new column, not the value.
				return cellValue{t, jsonString(fmt.Sprintf("o%d", i+1))}, true, ""
			}
		}
		return v, false, "is not one of the column's values"
	case "date":
		d, ok := isoDate(t)
		if !ok {
			return v, false, "is not a date written as year, month and day"
		}
		return cellValue{d, jsonString(d)}, true, ""
	case "relation":
		canon, ok := relationMention(t)
		if !ok {
			return v, false, "is not one mention such as <smrit:d:ID>"
		}
		return cellValue{t, jsonString(canon)}, true, ""
	}
	return v, false, "does not fit the column"
}

// jsonString is s as Smrit's json.dumps(ensure_ascii=False) writes it, for
// the text a cell may hold.
func jsonString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// storedRowBytes is the row's stored payload size: {"c1":…,"c2":…}, keyed
// by each column's key, c1 for the first column of a new workbook.
func storedRowBytes(cols []int, vals []cellValue) int {
	n := 2
	for i, col := range cols {
		if i > 0 {
			n++
		}
		n += len(fmt.Sprintf(`"c%d":`, col)) + len(vals[i].stored)
	}
	return n
}

// hiddenOrControl is Smrit's rule for a text cell: CONTROL_CHAR_PATTERN and
// HIDDEN_CHAR_PATTERN in api/smrit/okf/validator.py. Tab, newline and a
// carriage return before a newline stay writable.
func hiddenOrControl(s string) bool {
	for i, r := range s {
		switch {
		case r == '\r':
			if i+1 >= len(s) || s[i+1] != '\n' {
				return true
			}
		case r < 0x20 && r != '\t' && r != '\n', r == 0x7f:
			return true
		case r >= 0x80 && r <= 0x9f, r >= 0x200b && r <= 0x200f, r >= 0x2028 && r <= 0x202e,
			r >= 0x2060 && r <= 0x2064, r >= 0x2066 && r <= 0x2069, r == 0xad, r == 0x180e,
			r == 0xfeff, r == 0x34f, r >= 0x115f && r <= 0x1160, r == 0x3164, r == 0xffa0,
			r >= 0xfff9 && r <= 0xfffb, r >= 0x1d173 && r <= 0x1d17a, r >= 0xfe00 && r <= 0xfe0e,
			r >= 0xe0000 && r <= 0xe007f, r >= 0xe0100 && r <= 0xe01ef:
			return true
		}
	}
	return false
}

// maxIntDigits is Python's default limit on the digits of an int it reads.
const maxIntDigits = 4300

// storedNumberOK is t as Smrit stores it, when t is a JSON number Smrit
// takes: an integer stays as written, a float is Python's repr of it, and a
// float that is not finite is refused.
func storedNumberOK(t string) (string, bool) {
	if t == "" || (t[0] != '-' && (t[0] < '0' || t[0] > '9')) || !json.Valid([]byte(t)) {
		return "", false
	}
	if !strings.ContainsAny(t, ".eE") {
		digits := strings.TrimPrefix(t, "-")
		if len(digits) > maxIntDigits {
			return "", false
		}
		if digits == "0" {
			return "0", true
		}
		return t, true
	}
	f, err := strconv.ParseFloat(t, 64)
	if math.IsInf(f, 0) || math.IsNaN(f) || (err != nil && f != 0) {
		return "", false
	}
	return pyFloatRepr(f), true
}

// storedNumber is storedNumberOK's text alone.
func storedNumber(t string) string {
	s, _ := storedNumberOK(t)
	return s
}

// pyFloatRepr is Python's repr of a float: the shortest digits, fixed
// notation with at least one decimal between 1e-4 and 1e16, exponent
// notation outside it.
func pyFloatRepr(f float64) string {
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	if decpt := exp + 1; decpt <= -4 || decpt > 16 {
		return e
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// isoDate is t as YYYY-MM-DD when Python's date.fromisoformat takes it in
// one of its documented forms: 2026-11-09, 20261109, 2026-W45, 2026W45,
// 2026-W45-1 and 2026W451.
func isoDate(t string) (string, bool) {
	digits := func(s string) (int, bool) {
		n := 0
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return 0, false
			}
			n = n*10 + int(s[i]-'0')
		}
		return n, s != ""
	}
	if len(t) < 7 {
		return "", false
	}
	year, ok := digits(t[:4])
	if !ok || year < 1 {
		return "", false
	}
	rest := t[4:]
	sep := strings.HasPrefix(rest, "-")
	if sep {
		rest = rest[1:]
	}
	var d time.Time
	if w, isWeek := strings.CutPrefix(rest, "W"); isWeek {
		day := 1
		switch {
		case len(w) == 2:
		case sep && len(w) == 4 && w[2] == '-':
			day, ok = digits(w[3:])
		case !sep && len(w) == 3:
			day, ok = digits(w[2:])
		default:
			return "", false
		}
		week, wok := digits(w[:2])
		if !ok || !wok || week < 1 || day < 1 || day > 7 {
			return "", false
		}
		// The last ISO week of the year is the week of 28 December.
		if _, last := time.Date(year, 12, 28, 0, 0, 0, 0, time.UTC).ISOWeek(); week > last {
			return "", false
		}
		jan4 := time.Date(year, 1, 4, 0, 0, 0, 0, time.UTC)
		monday := jan4.AddDate(0, 0, -((int(jan4.Weekday()) + 6) % 7))
		d = monday.AddDate(0, 0, (week-1)*7+day-1)
	} else {
		var ms, ds string
		switch {
		case sep && len(rest) == 5 && rest[2] == '-':
			ms, ds = rest[:2], rest[3:]
		case !sep && len(rest) == 4:
			ms, ds = rest[:2], rest[2:]
		default:
			return "", false
		}
		m, mok := digits(ms)
		day, dok := digits(ds)
		if !mok || !dok || m < 1 || m > 12 || day < 1 {
			return "", false
		}
		d = time.Date(year, time.Month(m), day, 0, 0, 0, 0, time.UTC)
		if d.Month() != time.Month(m) || d.Day() != day {
			return "", false
		}
	}
	if d.Year() < 1 || d.Year() > 9999 {
		return "", false
	}
	return d.Format("2006-01-02"), true
}

var (
	mentionRE = regexp.MustCompile(`^<smrit:(d|s|wb):([^>\s]+)>$`)
	seqRE     = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)
	hexRE     = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
)

const base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// maxShortID is 2**122: a short id at or past it names no id.
var maxShortID = new(big.Int).Lsh(big.NewInt(1), 122)

// relationMention is the canonical mention Smrit stores for a relation
// cell written <smrit:d:ID>, <smrit:s:ID>, <smrit:wb:ID> or
// <smrit:wb:ID#SEQ>, with ID short or a UUID. The other spellings Smrit
// reads in a document body are refused here.
func relationMention(t string) (string, bool) {
	if strings.ContainsAny(t, "?;") {
		return "", false
	}
	m := mentionRE.FindStringSubmatch(t)
	if m == nil {
		return "", false
	}
	kind, rest, seq := m[1], m[2], ""
	if kind == "wb" {
		if i := strings.IndexByte(rest, '#'); i >= 0 {
			rest, seq = rest[:i], rest[i+1:]
			if !seqRE.MatchString(seq) {
				return "", false
			}
			if n, err := strconv.ParseUint(seq, 10, 64); err != nil || n > math.MaxInt64 {
				return "", false
			}
		}
	}
	if !validID(rest) {
		return "", false
	}
	out := "<smrit:" + kind + ":" + strings.Repeat("x", 21)
	if seq != "" {
		out += "#" + seq
	}
	return out + ">", true
}

// validID is Smrit's parse_id: 21 base58 characters below 2**122, or a
// UUID of 32 hex digits, hyphens and braces aside.
func validID(s string) bool {
	if len(s) == 21 {
		n := new(big.Int)
		for i := 0; i < len(s); i++ {
			d := strings.IndexByte(base58, s[i])
			if d < 0 {
				return false
			}
			n.Mul(n, big.NewInt(58)).Add(n, big.NewInt(int64(d)))
		}
		return n.Cmp(maxShortID) < 0
	}
	h := strings.ReplaceAll(strings.ReplaceAll(s, "urn:", ""), "uuid:", "")
	h = strings.ReplaceAll(strings.Trim(h, "{}"), "-", "")
	return hexRE.MatchString(h)
}
