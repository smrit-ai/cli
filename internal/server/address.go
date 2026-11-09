//go:build !dev

// Package server holds the address of the Smrit server this binary talks to.
package server

// Address is compiled in. Nothing in a release build changes it.
const Address = "https://mcp.smrit.ai"
