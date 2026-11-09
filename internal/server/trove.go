//go:build !dev

package server

// TroveOrigin is where object bytes go, compiled in like Address. An upload
// URL Smrit hands back is used only when its scheme and host are these.
const TroveOrigin = "https://trove.smrit.ai"

// Dev reports a dev build, the only one that may send object bytes over
// plain http, and only to an http TroveOrigin set at link time.
const Dev = false
