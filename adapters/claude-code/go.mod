module github.com/openbox-ai/openbox-shift-left/adapters/claude-code

go 1.23

require (
	github.com/openbox-ai/openbox-shift-left/client v0.0.0
	// test-only: assert emitted events conform to the SL-1 contract schema.
	github.com/openbox-ai/openbox-shift-left/contracts/dev-event/conformance v0.0.0
)

// Sibling modules in this multi-module repo; no published version yet, so
// consume them from source.
replace github.com/openbox-ai/openbox-shift-left/client => ../../client

replace github.com/openbox-ai/openbox-shift-left/contracts/dev-event/conformance => ../../contracts/dev-event/conformance
