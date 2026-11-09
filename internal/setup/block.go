package setup

import (
	"errors"
	"fmt"
	"strings"
)

// The markers of the always on block. The project line divides the served
// text, above it, from the paragraph an agent writes for one project, below.
const (
	StartMarker   = "<!-- smrit:start -->"
	ProjectMarker = "<!-- smrit:project -->"
	EndMarker     = "<!-- smrit:end -->"
)

// MaxBlock bounds the served block, which is a few paragraphs.
const MaxBlock = 16 << 10

// ErrNoBlock means the served skill has no always on block setup can trust.
var ErrNoBlock = errors.New("The onboarding skill Smrit served has no always on block setup can use, so it has nothing to write. Try again later, or upgrade smrit.")

func isMarker(line, marker string) bool { return strings.TrimSpace(line) == marker }

// markers is the index of every line that is exactly marker.
func markers(lines []string, marker string) []int {
	var at []int
	for i, l := range lines {
		if isMarker(l, marker) {
			at = append(at, i)
		}
	}
	return at
}

// ExtractBlock returns the always on block from the served onboarding
// skill: from the start marker through the end marker, inside a fenced
// markdown block. The skill must hold exactly one start and one end marker
// line, at most one project line between them, and the block must be at
// most MaxBlock bytes. When the served block has no project line, one is
// put on its own line just before the end marker.
func ExtractBlock(skill string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(skill, "\r\n", "\n"), "\n")
	starts, ends, projects := markers(lines, StartMarker), markers(lines, EndMarker), markers(lines, ProjectMarker)
	if len(starts) != 1 || len(ends) != 1 || len(projects) > 1 {
		return "", ErrNoBlock
	}
	start, end := starts[0], ends[0]
	if end < start || (len(projects) == 1 && (projects[0] < start || projects[0] > end)) {
		return "", ErrNoBlock
	}
	// The block sits in a fence opened with ```markdown and not closed
	// before the end marker.
	open := -1
	for i := start - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
			if strings.TrimSpace(lines[i]) == "```markdown" {
				open = i
			}
			break
		}
	}
	if open < 0 {
		return "", ErrNoBlock
	}
	for _, l := range lines[start:end] {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			return "", ErrNoBlock
		}
	}
	block := append([]string(nil), lines[start:end+1]...)
	for k := range block {
		block[k] = strings.TrimRight(block[k], " \t")
	}
	if len(projects) == 0 {
		block = append(block[:len(block)-1], ProjectMarker, EndMarker)
	}
	out := strings.Join(block, "\n")
	if len(out) > MaxBlock {
		return "", ErrNoBlock
	}
	return out, nil
}

// ApplyBlock writes block, which ExtractBlock returned, into the text of an
// instruction file.
//
// Where the file has a block, the served part, from the start marker
// through the project line, is replaced, and whatever follows the project
// line up to the end marker is kept exactly. A block written by hand,
// without a project line, keeps every line that is not part of the served
// text, in its order, below the project line. Without a block, the block is
// appended after a blank line. A file whose markers are not one start, then
// at most one project line, then one end, is refused, never rewritten. A
// file with CRLF line endings gets CRLF on every line written.
func ApplyBlock(text, block string) (string, error) {
	served := strings.Split(block, "\n")
	p := indexMarker(served, ProjectMarker, 0)
	if p < 0 || !isMarker(served[0], StartMarker) || !isMarker(served[len(served)-1], EndMarker) {
		return "", ErrNoBlock
	}
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	cr := strings.TrimSuffix(eol, "\n")
	head := make([]string, p+1)
	for i, l := range served[:p+1] {
		head[i] = l + cr
	}

	lines := strings.Split(text, "\n")
	starts, ends := markers(lines, StartMarker), markers(lines, EndMarker)
	if len(starts) == 0 && len(ends) == 0 {
		if text == "" {
			return strings.Join(served, eol) + eol, nil
		}
		if !strings.HasSuffix(text, "\n") {
			text += eol
		}
		if !strings.HasSuffix(text, "\n\n") && !strings.HasSuffix(text, "\n\r\n") {
			text += eol
		}
		return text + strings.Join(served, eol) + eol, nil
	}
	switch {
	case len(starts) > 1:
		return "", fmt.Errorf("it has %s more than once", StartMarker)
	case len(ends) > 1:
		return "", fmt.Errorf("it has %s more than once", EndMarker)
	case len(starts) == 0:
		return "", fmt.Errorf("it has %s without %s before it", EndMarker, StartMarker)
	case len(ends) == 0:
		return "", fmt.Errorf("it has %s without %s after it", StartMarker, EndMarker)
	case ends[0] < starts[0]:
		return "", fmt.Errorf("it has %s before %s", EndMarker, StartMarker)
	}
	start, end := starts[0], ends[0]
	projects := markers(lines[start:end], ProjectMarker)
	if len(projects) > 1 {
		return "", fmt.Errorf("its block has %s more than once", ProjectMarker)
	}
	if len(markers(lines[:start], ProjectMarker))+len(markers(lines[end:], ProjectMarker)) > 0 {
		return "", fmt.Errorf("it has %s outside the block", ProjectMarker)
	}
	var keep []string
	if len(projects) == 1 {
		keep = lines[start+projects[0]+1 : end]
	} else {
		keep = ownLines(lines[start+1:end], served[1:p], cr)
	}
	out := append([]string(nil), lines[:start]...)
	out = append(out, head...)
	out = append(out, keep...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n"), nil
}

// ownLines is every line of a hand written block that is not a line of the
// served text, in order. Blank lines are kept only between two kept lines,
// one at most; cr ends the blank lines it adds.
func ownLines(old, served []string, cr string) []string {
	known := map[string]bool{}
	for _, l := range served {
		if t := strings.TrimSpace(l); t != "" {
			known[t] = true
		}
	}
	var keep []string
	blank := false
	for _, l := range old {
		t := strings.TrimSpace(l)
		switch {
		case t == "":
			blank = len(keep) > 0
		case known[t]:
		default:
			if blank {
				keep = append(keep, cr)
			}
			keep = append(keep, l)
			blank = false
		}
	}
	return keep
}

func indexMarker(lines []string, marker string, from int) int {
	for i := from; i < len(lines); i++ {
		if isMarker(lines[i], marker) {
			return i
		}
	}
	return -1
}
