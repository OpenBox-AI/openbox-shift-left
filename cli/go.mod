module github.com/openbox-ai/openbox-shift-left/cli

go 1.23

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

require github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig v0.0.0 // indirect

// E6-S5: the local decision sidecar the `openbox sidecar serve` subcommand runs
// (ADR-0003 — one binary, WIRE-2; cli imports sidecar, never the reverse).
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
