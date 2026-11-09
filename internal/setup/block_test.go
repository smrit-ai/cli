package setup

import (
	"strings"
	"testing"
)

// servedSkill is the shape of the served onboarding skill: prose that names
// the markers inline, then the fenced block, then more prose.
const servedSkill = "# Setting up\n\nIf the file has `<!-- smrit:start -->`, replace through `<!-- smrit:end -->`.\n\n" +
	"```markdown\n<!-- smrit:start -->\n## Smrit\nSmrit is memory.\n\nCall `start` once.\n<!-- smrit:end -->\n```\n\nIn the project file, add one paragraph.\n"

const wantBlock = "<!-- smrit:start -->\n## Smrit\nSmrit is memory.\n\nCall `start` once.\n<!-- smrit:project -->\n<!-- smrit:end -->"

func TestExtractBlockAddsProjectLine(t *testing.T) {
	got, err := ExtractBlock(servedSkill)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantBlock {
		t.Fatalf("block:\n%s", got)
	}
}

func TestExtractBlockKeepsServedProjectLine(t *testing.T) {
	skill := strings.Replace(servedSkill, "once.\n<!-- smrit:end -->", "once.\n<!-- smrit:project -->\n<!-- smrit:end -->", 1)
	got, err := ExtractBlock(skill)
	if err != nil {
		t.Fatal(err)
	}
	if got != wantBlock {
		t.Fatalf("block:\n%s", got)
	}
}

func TestExtractBlockRefusesASkillWithoutOne(t *testing.T) {
	for _, s := range []string{"# Nothing here\n", "```markdown\n<!-- smrit:start -->\nno end\n```\n", "<!-- smrit:start -->\nx\n<!-- smrit:end -->\n"} {
		if _, err := ExtractBlock(s); err == nil {
			t.Fatalf("expected a refusal for %q", s)
		}
	}
}

func TestBlockReplacedKeepsProjectParagraph(t *testing.T) {
	old := "# My rules\n\nBe brief.\n\n<!-- smrit:start -->\n## Smrit\nAn older wording.\n<!-- smrit:project -->\nThis project lives in the engineering space.\n\nIts index is the Task list.\n<!-- smrit:end -->\n\n## After\nKeep me.\n"
	got, err := ApplyBlock(old, wantBlock)
	if err != nil {
		t.Fatal(err)
	}
	want := "# My rules\n\nBe brief.\n\n<!-- smrit:start -->\n## Smrit\nSmrit is memory.\n\nCall `start` once.\n<!-- smrit:project -->\nThis project lives in the engineering space.\n\nIts index is the Task list.\n<!-- smrit:end -->\n\n## After\nKeep me.\n"
	if got != want {
		t.Fatalf("got:\n%s", got)
	}
	again, err := ApplyBlock(got, wantBlock)
	if err != nil || again != got {
		t.Fatalf("a second run changed the file: %v\n%s", err, again)
	}
}

func TestBlockWrittenByHandKeepsWhatFollowsTheServedText(t *testing.T) {
	old := "<!-- smrit:start -->\n## Smrit\nSmrit is memory.\n\nCall `start` once.\nThis project lives in the web space.\n<!-- smrit:end -->\n"
	got, err := ApplyBlock(old, wantBlock)
	if err != nil {
		t.Fatal(err)
	}
	want := "<!-- smrit:start -->\n## Smrit\nSmrit is memory.\n\nCall `start` once.\n<!-- smrit:project -->\nThis project lives in the web space.\n<!-- smrit:end -->\n"
	if got != want {
		t.Fatalf("got:\n%s", got)
	}
}

func TestBlockWrittenByHandInOtherWordsLosesNothing(t *testing.T) {
	old := "<!-- smrit:start -->\nWords nobody serves.\nMore of them.\n<!-- smrit:end -->\n"
	got, err := ApplyBlock(old, wantBlock)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "<!-- smrit:project -->\nWords nobody serves.\nMore of them.\n<!-- smrit:end -->") {
		t.Fatalf("hand written text was lost:\n%s", got)
	}
}

func TestBlockAppendedWhenAbsent(t *testing.T) {
	cases := map[string]string{
		"":                         wantBlock + "\n",
		"# Mine\n\nrules\n":        "# Mine\n\nrules\n\n" + wantBlock + "\n",
		"# Mine\nno final newline": "# Mine\nno final newline\n\n" + wantBlock + "\n",
		"# Mine\n\nrules\n\n\n":    "# Mine\n\nrules\n\n\n" + wantBlock + "\n",
	}
	for old, want := range cases {
		got, err := ApplyBlock(old, wantBlock)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("from %q got:\n%q", old, got)
		}
	}
}

func TestBlockWithoutEndMarkerRefused(t *testing.T) {
	if _, err := ApplyBlock("<!-- smrit:start -->\nhalf a block\n", wantBlock); err == nil {
		t.Fatal("a start marker without an end must be refused, not guessed at")
	}
}

func TestBlockWrittenByHandKeepsEveryLineOfItsOwn(t *testing.T) {
	old := "<!-- smrit:start -->\n## Smrit\nMY CUSTOM RULE\nSmrit is memory.\n\nAnother of mine.\nCall `start` once.\n<!-- smrit:end -->\n"
	got, err := ApplyBlock(old, wantBlock)
	if err != nil {
		t.Fatal(err)
	}
	want := "<!-- smrit:start -->\n## Smrit\nSmrit is memory.\n\nCall `start` once.\n<!-- smrit:project -->\nMY CUSTOM RULE\n\nAnother of mine.\n<!-- smrit:end -->\n"
	if got != want {
		t.Fatalf("got:\n%s", got)
	}
}

func TestMalformedBlocksRefused(t *testing.T) {
	for _, old := range []string{
		"<!-- smrit:start -->\nA\n<!-- smrit:start -->\nB\n<!-- smrit:end -->\n",
		"<!-- smrit:end -->\n<!-- smrit:start -->\nA\n<!-- smrit:end -->\n",
		"<!-- smrit:start -->\nA\n<!-- smrit:end -->\n<!-- smrit:start -->\nB\n<!-- smrit:end -->\n",
		"<!-- smrit:start -->\nA\n<!-- smrit:end -->\n<!-- smrit:end -->\n",
		"<!-- smrit:start -->\n<!-- smrit:project -->\nA\n<!-- smrit:project -->\n<!-- smrit:end -->\n",
		"A\n<!-- smrit:end -->\n",
	} {
		if got, err := ApplyBlock(old, wantBlock); err == nil {
			t.Fatalf("expected a refusal for %q, got %q", old, got)
		}
	}
}

func TestBlockKeepsCRLF(t *testing.T) {
	crlf := func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }
	old := crlf("# Rules\n<!-- smrit:start -->\nOld.\n<!-- smrit:project -->\nMine.\n<!-- smrit:end -->\nAfter.\n")
	got, err := ApplyBlock(old, wantBlock)
	if err != nil {
		t.Fatal(err)
	}
	want := crlf("# Rules\n" + strings.TrimSuffix(wantBlock, EndMarker) + "Mine.\n<!-- smrit:end -->\nAfter.\n")
	if got != want {
		t.Fatalf("got %q", got)
	}
	got, _ = ApplyBlock(crlf("# Rules\n"), wantBlock)
	if got != crlf("# Rules\n\n"+wantBlock+"\n") {
		t.Fatalf("appended %q", got)
	}
}

func TestExtractBlockStrict(t *testing.T) {
	fence := func(body string) string { return "# S\n\n```markdown\n" + body + "\n```\n" }
	for name, skill := range map[string]string{
		"two starts":    fence("<!-- smrit:start -->\nA\n<!-- smrit:end -->") + fence("<!-- smrit:start -->\nB\n<!-- smrit:end -->"),
		"two ends":      fence("<!-- smrit:start -->\nA\n<!-- smrit:end -->\n<!-- smrit:end -->"),
		"two projects":  fence("<!-- smrit:start -->\nA\n<!-- smrit:project -->\n<!-- smrit:project -->\n<!-- smrit:end -->"),
		"too big":       fence("<!-- smrit:start -->\n" + strings.Repeat("x", 17<<10) + "\n<!-- smrit:end -->"),
		"project after": fence("<!-- smrit:start -->\nA\n<!-- smrit:end -->\n<!-- smrit:project -->"),
	} {
		if _, err := ExtractBlock(skill); err == nil {
			t.Fatalf("%s: expected a refusal", name)
		}
	}
}
