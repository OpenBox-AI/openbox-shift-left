// Package decision is the OpenBox developer-runtime local policy decision
// engine; what the enforce-mode PreToolUse hook asks for a governance decision
// before a tool call runs. You cannot block a developer's Bash/Edit/Read on a
// second-plus round-trip. You cannot block a developer's Bash/Edit/Read on a
// second-plus round-trip. So the enforcement decision must be made locally,
// from a policy bundle synced out-of-band.
//   - Protocol.go; the DecisionRequest / DecisionResponse contract.
package decision
