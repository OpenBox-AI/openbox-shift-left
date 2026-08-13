package hookflow

// EnvStaleDir named the stale-marker directory the session-start freshness
// check wrote to. Both the check and the markers are gone with the local policy
// bundle (ADR-0017); the constant survives only so a test harness that still
// isolates the path keeps compiling, and any file left at it is inert.
//
// Deliberately not re-pointed at anything: there is no bundle path to resolve
// any more, so ResolveBundlePath was deleted rather than made to return a
// plausible-looking value nothing reads.
const EnvStaleDir = "OPENBOX_STALE_DIR"
