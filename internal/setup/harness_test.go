package setup

import (
	"path/filepath"
	"testing"
)

func TestConfigFoldersFallBackToHome(t *testing.T) {
	env := map[string]string{}
	c := &Config{Home: "/home/p", Getenv: func(k string) string { return env[k] }}
	if claudeDir(c) != filepath.Join("/home/p", ".claude") || codexHome(c) != filepath.Join("/home/p", ".codex") {
		t.Fatal(claudeDir(c), codexHome(c))
	}
	env["CLAUDE_CONFIG_DIR"], env["CODEX_HOME"] = "relative", "also/relative"
	if claudeDir(c) != filepath.Join("/home/p", ".claude") || codexHome(c) != filepath.Join("/home/p", ".codex") {
		t.Fatal("a relative folder is ignored")
	}
	env["CLAUDE_CONFIG_DIR"], env["CODEX_HOME"] = "/c", "/x"
	if claudeDir(c) != "/c" || codexHome(c) != "/x" {
		t.Fatal(claudeDir(c), codexHome(c))
	}
}
