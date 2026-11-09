package setup

import (
	"errors"
	"strings"
	"testing"
)

func TestAddToListKeepsKeysOrderAndLiterals(t *testing.T) {
	in := `{"model":"opus","permissions":{"deny":["Bash(rm:*)"],"allow":["Read(./**)"],"ask":[]},"env":{"A":"<1>","B":"é"},"n":1.50,"big":12345678901234567890,"t":true,"z":null}`
	out, changed, err := AddToList([]byte(in), []string{"permissions", "allow"}, "mcp__smrit")
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	want := `{
  "model": "opus",
  "permissions": {
    "deny": [
      "Bash(rm:*)"
    ],
    "allow": [
      "Read(./**)",
      "mcp__smrit"
    ],
    "ask": []
  },
  "env": {
    "A": "<1>",
    "B": "é"
  },
  "n": 1.50,
  "big": 12345678901234567890,
  "t": true,
  "z": null
}
`
	if string(out) != want {
		t.Fatalf("got:\n%s", out)
	}
}

func TestAddToListNeverDuplicates(t *testing.T) {
	in := "{\"permissions\": {\"allow\": [\"mcp__smrit\"]}}"
	out, changed, err := AddToList([]byte(in), []string{"permissions", "allow"}, "mcp__smrit")
	if err != nil || changed || string(out) != in {
		t.Fatalf("%v %v %s", changed, err, out)
	}
}

func TestAddToListMakesMissingParents(t *testing.T) {
	for in, want := range map[string]string{
		"":                   "{\n  \"permissions\": {\n    \"allow\": [\n      \"r\"\n    ]\n  }\n}\n",
		"  \n":               "{\n  \"permissions\": {\n    \"allow\": [\n      \"r\"\n    ]\n  }\n}\n",
		`{"a":{}}`:           "{\n  \"a\": {},\n  \"permissions\": {\n    \"allow\": [\n      \"r\"\n    ]\n  }\n}\n",
		`{"permissions":{}}`: "{\n  \"permissions\": {\n    \"allow\": [\n      \"r\"\n    ]\n  }\n}\n",
	} {
		out, changed, err := AddToList([]byte(in), []string{"permissions", "allow"}, "r")
		if err != nil || !changed || string(out) != want {
			t.Fatalf("from %q: %v %v\n%s", in, changed, err, out)
		}
	}
}

func TestAddToListRefusesWhatItCannotEdit(t *testing.T) {
	for _, in := range []string{`{not json`, `{"a":1} trailing`, `{"a":1,}`, `[1,2]`, `"x"`, `{"permissions":[]}`, `{"permissions":{"allow":"mcp__smrit"}}`} {
		if _, _, err := AddToList([]byte(in), []string{"permissions", "allow"}, "r"); err == nil {
			t.Fatalf("expected a refusal for %s", in)
		}
	}
	_, _, err := AddToList([]byte(`{not json`), []string{"permissions", "allow"}, "r")
	if !errors.Is(err, ErrNotJSON) {
		t.Fatalf("a file that does not parse must say so: %v", err)
	}
}

func TestAddToListRefusesDuplicateKeys(t *testing.T) {
	for _, in := range []string{
		`{"permissions":{"allow":[]},"permissions":{}}`,
		`{"env":{"A":"1","A":"2"}}`,
		`{"x":[{"k":1,"k":2}]}`,
	} {
		_, _, err := AddToList([]byte(in), []string{"permissions", "allow"}, "r")
		if err == nil || !strings.Contains(err.Error(), "twice") {
			t.Fatalf("%s: %v", in, err)
		}
	}
}

func TestAddToListRefusesHalfACharacter(t *testing.T) {
	for _, in := range []string{`{"a":"\ud800"}`, `{"a":"\udc00x"}`, `{"a":"\ud800A"}`, `{"\ud83d":1}`, "{\"a\":\"\xff\"}"} {
		if _, _, err := AddToList([]byte(in), []string{"p"}, "r"); err == nil {
			t.Fatalf("%s: expected a refusal", in)
		}
	}
	out, _, err := AddToList([]byte(`{"a":"😀 \\ud800"}`), []string{"p"}, "r")
	if err != nil || !strings.Contains(string(out), "😀") || !strings.Contains(string(out), `\\ud800`) {
		t.Fatalf("a whole pair and an escaped backslash are fine: %v %s", err, out)
	}
}
