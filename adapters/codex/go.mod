module github.com/openbox-ai/openbox-shift-left/adapters/codex

go 1.23

require (
	// STORY-SL7-A (OD-SL7-SHARE): shared dev.json/credential resolution.
	github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig v0.0.0
	// STORY-SL-5: the commit-trailer hook install the SessionStart path shares.
	github.com/openbox-ai/openbox-shift-left/adapters/common/git v0.0.0
	// STORY-SL-3: the AIP-signed transport + E7 hook-wire builders.
	github.com/openbox-ai/openbox-shift-left/client v0.0.0
	// test-only: assert emitted events conform to the SL-1 contract schema.
	github.com/openbox-ai/openbox-shift-left/contracts/dev-event/conformance v0.0.0
	// STORY-SL7-B: the shared in-process decision engine (consumed, not modified).
	github.com/openbox-ai/openbox-shift-left/decision v0.0.0
	// SL4-WIRE-1: the shared install-time SPI (Installer/CredentialRef).
	github.com/openbox-ai/openbox-shift-left/provider v0.0.0
)

// Sibling modules in this multi-module repo; no published version yet, so
// consume them from source.
replace github.com/openbox-ai/openbox-shift-left/adapters/common/git => ../common/git

replace github.com/openbox-ai/openbox-shift-left/adapters/common/devconfig => ../common/devconfig

replace github.com/openbox-ai/openbox-shift-left/client => ../../client

replace github.com/openbox-ai/openbox-shift-left/decision => ../../decision

replace github.com/openbox-ai/openbox-shift-left/contracts/dev-event/conformance => ../../contracts/dev-event/conformance

replace github.com/openbox-ai/openbox-shift-left/provider => ../../provider
