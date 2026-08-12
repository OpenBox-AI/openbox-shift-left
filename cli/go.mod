module github.com/openbox-ai/openbox-shift-left/cli

go 1.23.0


require (
	// SL4-WIRE-1: the CLI registers the real Claude Code installer and depends
	// on the shared install-time SPI both it and the adapter implement.
	github.com/openbox-ai/openbox-shift-left/adapters/claude-code v0.0.0
	// STORY-SL7-A: the real Codex installer + hook engine.
	github.com/openbox-ai/openbox-shift-left/adapters/codex v0.0.0
	github.com/openbox-ai/openbox-shift-left/provider v0.0.0
)

require github.com/openbox-ai/openbox-shift-left/adapters/common/git v0.0.0

require github.com/openbox-ai/openbox-shift-left/client v0.0.0

require golang.org/x/sys v0.35.0 // indirect

// The in-process decision engine the enforce hook evaluates against (ADR-0006
// retired the socket sidecar and its `sidecar serve` subcommand; cli imports
// decision, never the reverse).
require github.com/openbox-ai/openbox-shift-left/decision v0.0.0

// Sibling modules in this multi-module repo; no published version yet, so
// consume them from source. The claude-code adapter transitively pulls in
// client, common/git, and the dev-event conformance module.
replace github.com/openbox-ai/openbox-shift-left/provider => ../provider

replace github.com/openbox-ai/openbox-shift-left/adapters/claude-code => ../adapters/claude-code

replace github.com/openbox-ai/openbox-shift-left/adapters/codex => ../adapters/codex

replace github.com/openbox-ai/openbox-shift-left/adapters/common/git => ../adapters/common/git

replace github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig => ../adapters/common/devconfig

replace github.com/openbox-ai/openbox-shift-left/client => ../client

replace github.com/openbox-ai/openbox-shift-left/contracts/dev-event/conformance => ../contracts/dev-event/conformance

replace github.com/openbox-ai/openbox-shift-left/decision => ../decision

require (
	github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig v0.0.0
	github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow v0.0.0
	// This repo's ONLY external dependency (ADR-0015): masked credential input
	// and TTY detection that works on native Windows, where the stdlib mode
	// check misjudges a console handle (golang/go#23123).
	//
	// PINNED, and not to the latest: x/term v0.35.0+ declares `go 1.24.0` and
	// v0.45.0 wants `go 1.25.0`, so upgrading raises this repo's language floor
	// across all eleven modules and go.work — a toolchain decision arriving
	// disguised as a dependency bump. `go mod tidy` and `go get -u` will both
	// happily do it; don't let them. v0.34.0 is the newest release still
	// declaring `go 1.23.0`.
	golang.org/x/term v0.34.0
)

replace github.com/openbox-ai/openbox-shift-left/adapters/common/hookflow => ../adapters/common/hookflow
