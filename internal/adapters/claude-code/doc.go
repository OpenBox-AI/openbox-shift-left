// Package claudecode is the OpenBox Claude Code adapter; the first realization
// of the generic Provider Adapter Contract for a developer-runtime coding
// tool.
//   - Register(); done out-of-band by `openbox init`, which mints the obx_ key
//   - did:aip DID + Ed25519 seed and installs this plugin.
//   - Emit(event); Observe(): map a hook payload → DevEvent → client.Emit (via
//     a local spool so the tool-call hot path never does network I/O).
//   - Apply(verdict); observe mode is a no-op (INV-3): every verdict is
//     treated as allow unless enforce mode is on (enforce.go).
package claudecode
