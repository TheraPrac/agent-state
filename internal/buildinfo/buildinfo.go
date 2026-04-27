// Package buildinfo exposes compile-time identity for the st binary so
// the runtime can report which build is running and detect drift between
// per-agent binaries (I-404 follow-up).
//
// The vars are populated via -ldflags at build time — both `make build`
// and `make install` set them, since `install` depends on `build`.
// Consumers import this package and read the exported strings; nothing
// else writes to them. When the binary is built without the make
// wrapper (e.g., raw `go build`, or `go test` linking the main package),
// the defaults below apply and drift detection treats the binary as
// "unstamped" — visible in status output as `<unstamped>` so the
// operator knows to rebuild via the wrapper.
package buildinfo

// Version is the human-readable release tag, bumped manually with
// notable changes. Independent of Commit.
var Version = "0.6.0"

// Commit is the git SHA of the as repo at build time. "unknown" when
// the binary was built without the make wrapper. Reported by
// `st version` and recorded in agent registration so `st status` can
// detect drift between per-agent binaries.
var Commit = "unknown"

// Dirty is "1" when the working tree had uncommitted changes at build
// time, "0" when clean. Distinguishes a reproducible release-style
// build from a dev-loop build that may not match what's pushed.
var Dirty = "0"

// Built is the human-readable timestamp of `make install`, captured
// at build time. Useful when debugging "why is this agent running an
// older binary?" without forcing a `git log` lookup.
var Built = "unknown"
