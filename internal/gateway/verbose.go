package gateway

// verbose.go is the relay's optional running commentary.
//
// It exists for one question that is otherwise very hard to answer from the
// outside: IS ANYTHING ACTUALLY REACHING THIS PROCESS? A gateway that is
// listening, healthy and completely bypassed looks identical to one doing its
// job — the daemon prints its startup banner either way, and the difference only
// shows up much later as an absence in stored data. That ambiguity has already
// cost real debugging time.
//
// The seam is a FUNCTION rather than a logger, so this package acquires no
// logging dependency and no writer of its own — the CLI supplies both, the same
// way it supplies the Emitter. Nil means silent, which is the default and is
// byte-identical to having no instrumentation at all.
//
// WHAT MAY BE LOGGED IS DELIBERATELY NARROW. The developer's live credential
// transits every request through here, and a debug flag that writes it to a
// terminal — or to a launchd log file that outlives the session — would be a
// credential leak wearing a helpful face. So the rules are:
//
//   - method, path (never the query: it can carry content or a token, which is
//     exactly why the capture drops it too), status, duration, and byte counts;
//   - NEVER a header name-value pair, never a body, never the credential or its
//     fingerprint.
//
// TestVerboseNeverLogsCredentialsOrBodies is the control, and it drives real
// traffic rather than inspecting the format strings.

// WithVerbose turns on per-call logging. Observe-only: it changes what is
// PRINTED, never what is forwarded, captured or evaluated.
func (g *Gateway) WithVerbose(logf func(format string, args ...any)) *Gateway {
	g.logf = logf
	return g
}

// verbose reports whether commentary is on. Callers use it to skip work that
// only feeds a log line — a clock read per request is cheap, but the non-verbose
// path should stay exactly what it was.
func (g *Gateway) verbose() bool { return g.logf != nil }

func (g *Gateway) vlog(format string, args ...any) {
	if g.logf != nil {
		g.logf(format, args...)
	}
}
