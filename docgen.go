//go:generate go run ./tools/docgen

package main

// This file anchors `go generate ./...` so the reference Markdown
// pages under docs/reference/ are regenerated from the canonical Go
// declarations in config.go and metrics.go. CI re-runs the generator
// on every push and fails the build when the committed output is
// stale; see tools/docgen for the implementation.
