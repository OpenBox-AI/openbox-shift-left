// Package gitaction is the OpenBox git action: the server-side, push-time
// resolver that binds a pushed commit to the OpenBox session(s) that produced
// it and registers a Deploy governance event provably linked to those
// sessions. That package's prepare-commit-msg hook writes an `OpenBox-
// Session:` trailer into the commit object locally; this action reads it back
// at push, against the real pushed SHA (never a pre-push SHA; git hooks are
// local and SHAs are unstable until push), dedups to a session set, and emits
// a Deploy event through the shared client (client.Emit).
package gitaction
