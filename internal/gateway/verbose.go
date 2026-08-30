package gateway

// WithVerbose turns on per-call logging. Observe-only: it changes what is
// printed, never what is forwarded, captured or evaluated.
func (g *Gateway) WithVerbose(logf func(format string, args ...any)) *Gateway {
	g.logf = logf
	return g
}

func (g *Gateway) verbose() bool { return g.logf != nil }

func (g *Gateway) vlog(format string, args ...any) {
	if g.logf != nil {
		g.logf(format, args...)
	}
}
