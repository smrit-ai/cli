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
