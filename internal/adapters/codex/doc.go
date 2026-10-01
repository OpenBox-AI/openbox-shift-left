// Package codex is the OpenAI Codex CLI realization of the Provider Adapter
// Contract; the observe leg. Observe-only, fail-open; it never blocks, denies,
// or slows a Codex tool call (INV-3; the enforce leg is in enforce.go).
package codex
