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

// ErrNoBlock means the served skill has no always on block to write.
var ErrNoBlock = errors.New("The onboarding skill Smrit served has no always on block, so setup has nothing to write. Try again later, or upgrade smrit.")

func isMarker(line, marker string) bool { return strings.TrimSpace(line) == marker }

// ExtractBlock returns the always on block from the served onboarding
// skill: the fenced markdown block holding the start and end markers, from
// the start marker through the end marker. When the served block has no
// project line, one is put on its own line just before the end marker.
func ExtractBlock(skill string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(skill, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```markdown" {
			continue
		}
		start, end := -1, -1
		j := i + 1
		for ; j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), "```"); j++ {
			switch {
			case start < 0 && isMarker(lines[j], StartMarker):
				start = j
			case start >= 0 && end < 0 && isMarker(lines[j], EndMarker):
				end = j
			}
		}
		if start >= 0 && end > start {
			block := append([]string(nil), lines[start:end+1]...)
			for k := range block {
				block[k] = strings.TrimRight(block[k], " \t")
			}
			hasProject := false
			for _, l := range block {
				if isMarker(l, ProjectMarker) {
					hasProject = true
				}
			}
			if !hasProject {
				block = append(block[:len(block)-1], ProjectMarker, EndMarker)
			}
			return strings.Join(block, "\n"), nil
		}
		i = j
	}
	return "", ErrNoBlock
}

// ApplyBlock writes block, which ExtractBlock returned, into the text of an
// instruction file. Where the file has a block already, the served part,
// from the start marker through the project line, is replaced and whatever
// follows the project line up to the end marker is kept exactly. A block
// written by hand, without a project line, keeps as its project paragraph
// everything after the last line it shares with the served text. Without a
// block, the block is appended after a blank line.
func ApplyBlock(text, block string) (string, error) {
	served := strings.Split(block, "\n")
	p := indexMarker(served, ProjectMarker, 0)
	if p < 0 || !isMarker(served[0], StartMarker) || !isMarker(served[len(served)-1], EndMarker) {
		return "", ErrNoBlock
	}
	head := served[:p+1]

	lines := strings.Split(text, "\n")
	start := indexMarker(lines, StartMarker, 0)
	if start < 0 {
		if text == "" {
			return block + "\n", nil
		}
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if !strings.HasSuffix(text, "\n\n") {
			text += "\n"
		}
		return text + block + "\n", nil
	}
	end := indexMarker(lines, EndMarker, start+1)
	if end < 0 {
		return "", fmt.Errorf("it has %s without %s after it", StartMarker, EndMarker)
	}
	var keep []string
	if pl := indexMarker(lines[:end], ProjectMarker, start+1); pl >= 0 {
		keep = lines[pl+1 : end]
	} else {
		keep = lines[start+1 : end]
		known := map[string]bool{}
		for _, l := range served[1:p] {
			if strings.TrimSpace(l) != "" {
				known[strings.TrimSpace(l)] = true
			}
		}
		for k := end - 1; k > start; k-- {
			if known[strings.TrimSpace(lines[k])] {
				keep = lines[k+1 : end]
				break
			}
		}
	}
	out := append([]string(nil), lines[:start]...)
	out = append(out, head...)
	out = append(out, keep...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n"), nil
}

func indexMarker(lines []string, marker string, from int) int {
	for i := from; i < len(lines); i++ {
		if isMarker(lines[i], marker) {
			return i
		}
	}
	return -1
}
