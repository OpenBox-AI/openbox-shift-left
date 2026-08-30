// Package claudecode is the OpenBox Claude Code adapter — the first
// realization of the generic Provider Adapter Contract for a
// developer-runtime coding tool.
//
// It maps Claude Code's native hook payloads (SessionStart /
// UserPromptSubmit / PreToolUse / PostToolUse / SessionEnd) onto the
// normalized developer event contract (api/dev-event.schema.json) and emits them
// through the shared AIP-signed transport (client/). It adds no governance
// pipeline: the developer runtime is onboarded onto the existing one,
// exactly as the base SDK's create_openbox_worker() onboards an agent
// runtime.
//
// Provider Adapter Contract (SPI) as realized here:
//
//   - register()     — done out-of-band by `openbox init`, which mints
//     the obx_ key + did:aip DID + Ed25519 seed and installs this plugin.
//     The adapter consumes that identity from the OS secret store (see
//     creds.go).
//   - emit(event)    — Observe(): map a hook payload → DevEvent →
//     client.Emit (via a local spool so the tool-call hot path never does
//     network I/O).
//   - apply(verdict) — observe mode is a no-op (INV-3): every verdict is
//     treated as allow unless enforce mode is on (enforce.go).
//   - capabilities() — see capabilities.go (Capabilities()).
//
// Design invariants:
//
//   - INV-2 metadata-only by default: the adapter never copies prompt text,
//     tool command strings, file contents, or tool output into the event —
//     not into `content`, not into `metadata`, not into `tool.name` (the
//     raw free-form metadata map and tool.name must not smuggle content
//     past the client's content stripper). File paths and tool identifiers
//     are structural metadata, not content, and are carried.
//   - INV-3 observe-only + fail-open: nothing this adapter does can block,
//     delay beyond a small local-I/O budget, or fail a Claude Code tool
//     call. The hook path always exits 0 with empty stdout (the shared
//     engine is claudecode.RunHook, invoked as `openbox hook claude-code
//     <event>`).
//
// The single Go engine (this package + client/) is shared by every provider
// adapter; adding a new tool is a new mapper + package, no change here.
package claudecode
