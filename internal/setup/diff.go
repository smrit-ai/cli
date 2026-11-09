package setup

import (
	"fmt"
	"strings"
)

const (
	diffContext = 3
	// maxDiffCells bounds the comparison table; past it the whole file is
	// shown as removed and added, which is still exact.
	maxDiffCells = 16 << 20
)

// splitLines splits text into lines that keep their newline, so a missing
// final newline is a difference the diff can show.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.SplitAfter(s, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

type diffOp struct {
	kind byte // ' ', '-', '+'
	line string
}

// diffLines is a longest common subsequence diff, enough for the size of a
// settings or instruction file.
func diffLines(a, b []string) []diffOp {
	n, m := len(a), len(b)
	if (n+1)*(m+1) > maxDiffCells {
		var ops []diffOp
		for _, l := range a {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range b {
			ops = append(ops, diffOp{'+', l})
		}
		return ops
	}
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

func hunkRange(start, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", start)
	}
	if count == 1 {
		return fmt.Sprintf("%d", start+1)
	}
	return fmt.Sprintf("%d,%d", start+1, count)
}

// unifiedDiff is the unified diff from oldText to newText, with three lines
// of context, or "" when they are the same.
func unifiedDiff(oldName, newName, oldText, newText string) string {
	if oldText == newText {
		return ""
	}
	ops := diffLines(splitLines(oldText), splitLines(newText))
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s\n", oldName, newName)
	// Each op's position in the old and new files.
	oldAt := make([]int, len(ops)+1)
	newAt := make([]int, len(ops)+1)
	for k, op := range ops {
		oldAt[k+1], newAt[k+1] = oldAt[k], newAt[k]
		if op.kind != '+' {
			oldAt[k+1]++
		}
		if op.kind != '-' {
			newAt[k+1]++
		}
	}
	k := 0
	for k < len(ops) {
		for k < len(ops) && ops[k].kind == ' ' {
			k++
		}
		if k == len(ops) {
			break
		}
		from := max(0, k-diffContext)
		to := k
		// Extend while the next change is close enough to share context.
		for {
			for to < len(ops) && ops[to].kind != ' ' {
				to++
			}
			same := 0
			for to+same < len(ops) && ops[to+same].kind == ' ' {
				same++
			}
			if to+same == len(ops) || same > 2*diffContext {
				to = min(len(ops), to+min(same, diffContext))
				break
			}
			to += same
		}
		fmt.Fprintf(&b, "@@ -%s +%s @@\n",
			hunkRange(oldAt[from], oldAt[to]-oldAt[from]), hunkRange(newAt[from], newAt[to]-newAt[from]))
		for _, op := range ops[from:to] {
			b.WriteByte(op.kind)
			b.WriteString(op.line)
			if !strings.HasSuffix(op.line, "\n") {
				b.WriteString("\n\\ No newline at end of file\n")
			}
		}
		k = to
	}
	return b.String()
}
