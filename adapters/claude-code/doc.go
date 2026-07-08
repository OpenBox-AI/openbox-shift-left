// Package claudecode is the OpenBox Claude Code adapter (STORY-SL-4) — the first
// realization of the generic Provider Adapter Contract (architecture §1b) for a
// developer-runtime coding tool.
//
// It maps Claude Code's native hook payloads (SessionStart / UserPromptSubmit /
// PreToolUse / PostToolUse / SessionEnd) onto the normalized developer event
// contract (STORY-SL-1, contracts/dev-event) and emits them through the shared
// AIP-signed transport (STORY-SL-3, client/). It adds NO governance pipeline:
// the developer runtime is onboarded onto the existing one, exactly as the
// Temporal SDK's create_openbox_worker() onboards an agent runtime.
//
// Provider Adapter Contract (SPI, §1b) as realized here:
//
//   - register()     — done out-of-band by `openbox dev init` (STORY-SL-2), which
//     mints the obx_ key + did:aip DID + Ed25519 seed and installs this plugin.
//     The adapter consumes that identity from the OS secret store (see creds.go).
//   - emit(event)    — Observe(): map a hook payload → SL-1 DevEvent → client.Emit
//     (via a local spool so the tool-call hot path never does network I/O).
//   - apply(verdict) — Phase-1 observe is a NO-OP (D7/INV-3): every verdict is
//     treated as allow; the adapter NEVER denies or blocks a tool call.
//   - capabilities() — see capabilities.go (Capabilities()).
//
// Design invariants (SL-4 story + inherited from SL-1/SL-3):
//
//   - INV-2 metadata-only by default: the adapter never copies prompt text, tool
//     command strings, file contents, or tool output into the event — not into
//     `content`, not into `metadata`, not into `tool.name`. (This is also the
//     SL3-SEC-3 requirement that gates each adapter's security review: the raw
//     free-form metadata map and tool.name must not smuggle content past the
//     client's content stripper.) File paths and tool identifiers are structural
//     metadata, not content, and are carried.
//   - INV-3 observe-only + fail-open: nothing this adapter does can block, delay
//     beyond a small local-I/O budget, or fail a Claude Code tool call. The hook
//     binary always exits 0 with empty stdout (see cmd/openbox-cc-hook).
//
// The single Go engine (this package + client/) is shared by every provider
// adapter; adding Codex (SL-7) or Cursor (SL-8) is a new mapper + package, no
// change here (architecture §1b, "adding a provider = one adapter").
package claudecode
