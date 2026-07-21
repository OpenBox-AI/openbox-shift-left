module github.com/openbox-ai/openbox-shift-left/decision

go 1.23

require github.com/openbox-ai/openbox-shift-left/client v0.0.0

// Sibling module in this multi-module repo; no published version yet, so consume
// it from source (ADR-0003: sidecar depends on client, never the reverse).
replace github.com/openbox-ai/openbox-shift-left/client => ../client
