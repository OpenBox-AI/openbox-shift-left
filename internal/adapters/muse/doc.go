// Package muse is the adapter for Meta's Muse Code CLI (`muse`): its native
// hook shape, its mapper, its output contract and its installer, on the
// shared hookflow engine. Muse's hook stdin is Claude Code-shaped, but its
// runtime discards an answer it does not accept and treats that as allow,
// so the output contract here is closed per event and never renders
// `continue` or `allow` where Muse rejects them.
package muse
