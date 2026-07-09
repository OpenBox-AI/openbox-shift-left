module github.com/openbox-ai/openbox-shift-left/cli

go 1.23

require (
	// SL4-WIRE-1: the CLI registers the real Claude Code installer and depends
	// on the shared install-time SPI both it and the adapter implement.
	github.com/openbox-ai/openbox-shift-left/adapters/claude-code v0.0.0
	github.com/openbox-ai/openbox-shift-left/provider v0.0.0
)

require (
	github.com/openbox-ai/openbox-shift-left/adapters/common/git v0.0.0 // indirect
	github.com/openbox-ai/openbox-shift-left/client v0.0.0 // indirect
)

// Sibling modules in this multi-module repo; no published version yet, so
// consume them from source. The claude-code adapter transitively pulls in
// client, common/git, and the dev-event conformance module.
replace github.com/openbox-ai/openbox-shift-left/provider => ../provider

replace github.com/openbox-ai/openbox-shift-left/adapters/claude-code => ../adapters/claude-code

replace github.com/openbox-ai/openbox-shift-left/adapters/common/git => ../adapters/common/git

replace github.com/openbox-ai/openbox-shift-left/client => ../client

replace github.com/openbox-ai/openbox-shift-left/contracts/dev-event/conformance => ../contracts/dev-event/conformance
