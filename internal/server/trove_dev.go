//go:build dev

package server

// TroveOrigin is set at link time in a dev build:
// -ldflags "-X github.com/smrit-ai/cli/internal/server.TroveOrigin=http://localhost:8001"
var TroveOrigin string

// Dev reports a dev build.
const Dev = true
