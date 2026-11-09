package setup

import (
	"strings"
	"testing"
)

func TestUnifiedDiff(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n"
	new := "a\nb\nc\nd\nE\nf\ng\nh\ni\nj\nk\n"
	got := unifiedDiff("/x/f", "/x/f", old, new)
	want := "--- /x/f\n+++ /x/f\n@@ -2,9 +2,10 @@\n b\n c\n d\n-e\n+E\n f\n g\n h\n i\n j\n+k\n"
	if got != want {
		t.Fatalf("got:\n%s", got)
	}
}

func TestUnifiedDiffNewFile(t *testing.T) {
	got := unifiedDiff("/dev/null", "/x/f", "", "one\ntwo\n")
	want := "--- /dev/null\n+++ /x/f\n@@ -0,0 +1,2 @@\n+one\n+two\n"
	if got != want {
		t.Fatalf("got:\n%s", got)
	}
}

func TestUnifiedDiffSeparateHunks(t *testing.T) {
	var a, b []string
	for i := 0; i < 30; i++ {
		a = append(a, "l")
		b = append(b, "l")
	}
	a[2], b[2] = "x", "y"
	a[25], b[25] = "p", "q"
	got := unifiedDiff("f", "f", strings.Join(a, "\n")+"\n", strings.Join(b, "\n")+"\n")
	if strings.Count(got, "@@ -") != 2 {
		t.Fatalf("want two hunks:\n%s", got)
	}
}

func TestUnifiedDiffMissingFinalNewline(t *testing.T) {
	got := unifiedDiff("f", "f", "a", "a\nb\n")
	if !strings.Contains(got, "\\ No newline at end of file") {
		t.Fatalf("got:\n%s", got)
	}
}
