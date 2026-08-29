// Its OWN module, deliberately.
//
// This tool fabricates provider responses. Nothing in the product may ever import
// it, and a separate module is the only form of that rule Go enforces on its own —
// no allowlist to maintain, no tripwire to keep honest. It also has no
// dependencies, so it costs the workspace nothing.
module github.com/openbox-ai/openbox-shift-left/probes/refusal-injector

go 1.27.0
