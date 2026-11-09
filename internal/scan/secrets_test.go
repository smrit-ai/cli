package scan

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The cases come from the plugin's Python module (scripts/secret_cases.py),
// so this proves the Go port agrees with it on every one, including the
// boundary cases its lookarounds exist for.
func TestSecretCasesMatchThePlugin(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "secret_cases.json"))
	if err != nil {
		t.Fatalf("Cannot read the cases: %v. Regenerate them with: make secret-cases", err)
	}
	var cases []struct {
		Text   string `json:"text"`
		Secret bool   `json:"secret"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 100 {
		t.Fatalf("only %d cases", len(cases))
	}
	var pos, neg int
	for _, c := range cases {
		if c.Secret {
			pos++
		} else {
			neg++
		}
		if got := ContainsSecret(c.Text); got != c.Secret {
			show := c.Text
			if len(show) > 120 {
				show = show[:120] + "..."
			}
			t.Errorf("ContainsSecret(%q) = %v, the plugin says %v", show, got, c.Secret)
		}
	}
	if pos == 0 || neg == 0 {
		t.Fatalf("cases need both kinds: %d secrets, %d not", pos, neg)
	}
}

// A file larger than one window is scanned in overlapping windows, so a
// secret across a window edge is still found.
func TestSecretAcrossWindowEdge(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 36)
	for split := 1; split < len(secret); split += 7 {
		text := strings.Repeat("x ", 50)[:100-split] + secret + strings.Repeat(" y", 100)
		w := newSecretWindow(100, 64)
		w.Write([]byte(text[:60]))
		w.Write([]byte(text[60:]))
		if !w.Close() {
			t.Fatalf("split %d: secret across the window edge was missed", split)
		}
	}
	w := newSecretWindow(100, 64)
	w.Write([]byte(strings.Repeat("plain words ", 100)))
	if w.Close() {
		t.Fatal("plain text flagged")
	}
}

func TestSecretPatternsNamesWithoutQuoting(t *testing.T) {
	got := SecretPatterns("key AKIA" + "ABCDEFGHIJKLMNOP and bearer " + "abcdefghijklmnopqrstuvwx")
	if len(got) != 2 || got[0] != "aws-access-key-id" || got[1] != "bearer-token" {
		t.Fatalf("%v", got)
	}
	if len(SecretPatterns("nothing here")) != 0 {
		t.Fatal("matched clean text")
	}
}
