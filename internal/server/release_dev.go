//go:build dev

package server

// ReleaseBase may be set at link time in a dev build:
// -ldflags "-X github.com/smrit-ai/cli/internal/server.ReleaseBase=https://localhost:8443/releases/latest/download"
// Unset, a dev build upgrades from the real releases.
var ReleaseBase = "https://github.com/smrit-ai/cli/releases/latest/download"
