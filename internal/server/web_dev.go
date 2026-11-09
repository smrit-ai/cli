//go:build dev

package server

// WebOrigin is set at link time in a dev build:
// -ldflags "-X github.com/smrit-ai/cli/internal/server.WebOrigin=http://localhost:3000"
var WebOrigin string
