// Package git is the provider-independent write side of session->commit
// attribution. It binds a git commit to the OpenBox session(s) that produced
// it by stamping an `OpenBox-Session:` commit-message trailer. The durable,
// authoritative binding is resolved server-side at push against the real
// pushed SHA; never a pre-push SHA; by the git action. Because git hooks are
// local and never travel, this write side is best-effort: its only job is to
// put the opaque session id into data that lives inside the commit object so
// the git action can resolve it later.
package git
