//go:build dev

// Package server holds the address of the Smrit server this binary talks to.
package server

// Address is set at link time in a dev build:
// -ldflags "-X github.com/smrit-ai/cli/internal/server.Address=http://localhost:8000"
var Address string
