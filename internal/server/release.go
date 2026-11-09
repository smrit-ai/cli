//go:build !dev

package server

// ReleaseBase is where smrit upgrade downloads a release from, compiled in
// like Address. It is GitHub's address for the latest release's assets.
const ReleaseBase = "https://github.com/smrit-ai/cli/releases/latest/download"
