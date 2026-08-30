// Package git is the provider-independent write side of session->commit
// attribution. The durable, authoritative binding is resolved server-side at
// push against the real pushed SHA; never a pre-push SHA; by the git action.
//   - INV-1: the trailer carries only the opaque session id, never a secret
//     (the obx_ key / Ed25519 seed).
//   - INV-6 (write side): multiple distinct sessions => multiple trailer lines
//     (genuine fan-in, mirroring Co-Authored-By); idempotent under re-
//     fire/`--amend` (identical id never duplicated).
package git
