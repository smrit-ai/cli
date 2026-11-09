//go:build dev

package server

// Address is set at link time in a dev build:
// -ldflags "-X github.com/smrit-ai/cli/internal/server.Address=http://localhost:8000"
var Address string
