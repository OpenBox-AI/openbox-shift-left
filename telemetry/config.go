// Package telemetry receives the coding tool's own OpenTelemetry export over a
// loopback OTLP endpoint and hands each record to an emitter.
//
// It exists because the gateway lane cannot see the traffic that matters most.
// `openbox init --gateway` points a client at a local relay via
// ANTHROPIC_BASE_URL, and measurement (2026-08-27) showed the terminal CLI
// follows that variable while the desktop app does not — so on a machine where
// the developer works in the desktop app the model-call tier is inert while every
// local check reports healthy. This lane rides the `env` block of
// ~/.claude/settings.json instead, which reaches the embedded engine on both.
//
// What it claims is weaker, and the difference must survive into every reader:
// the transport and gateway lanes observe the bytes in path, while this one is
// the governed tool REPORTING ITS OWN CALLS. A tool that wants to hide a call
// simply does not export it. It is adopted because it is the only lane covering
// the desktop app and subscription OAuth today, and because partial evidence that
// names its own limit beats a gap (ADR-0022 §1). OD4 is the compensating control:
// telemetry silence on an otherwise-active session is a FINDING, not an absence.
package telemetry

import (
	"context"
	"fmt"
	"net"
	"time"
)

// DefaultAddr is this receiver's loopback endpoint.
//
// Port 8789 is deliberate on both halves. It is loopback-only for the reason
// stated in Validate, and it is 8789 rather than the OTLP default 4318 because
// the default is what any other collector on the machine will already have taken
// — binding it would either fail or, worse, succeed after some other tool's
// collector died, silently swallowing exports meant for that tool. It sits beside
// the gateway's 8788 so the two openbox listeners read as a pair.
const DefaultAddr = "127.0.0.1:8789"

// resolveTimeout bounds the hostname lookup in requireLoopback.
//
// The same bound the gateway applies, for the same reason: DefaultResolver with a
// background context waits as long as the OS resolver does, which on an
// unreachable DNS path stalls daemon startup with no output at all.
const resolveTimeout = 2 * time.Second

// MaxRequestBodyBytes bounds one OTLP request.
//
// Set EXPLICITLY rather than inherited. The library's default is 20MiB, which is
// a sensible ceiling for a collector aggregating a fleet and a poor one for a
// per-developer loopback daemon: this listener is unauthenticated by construction
// (anything running as the developer can post to it), so the default is a local
// memory-exhaustion budget handed to any process on the machine.
//
// 8MiB is chosen against the measured shape of the traffic, not guessed: model
// request bodies in the evidence run averaged 290KB and peaked at 566KB, so this
// is an order of magnitude of headroom over the largest record this lane expects
// while staying far below what a single request could use to hurt the host.
const MaxRequestBodyBytes = 8 * 1024 * 1024

// Config is the receiver's listen configuration.
type Config struct {
	// Addr is the loopback host:port the OTLP endpoints are served on.
	Addr string
}

// Validate fills defaults and rejects a configuration that would break the one
// invariant this receiver rests on: it is reachable from this machine only.
//
// An OTLP receiver reachable off-host is an unauthenticated content firehose —
// prompts, tool inputs and outputs, and full model request bodies all arrive
// here. There is no authentication to add that would make an off-host bind
// acceptable, so the bind itself is the control and it is checked rather than
// documented.
func (c *Config) Validate() error {
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}

	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return fmt.Errorf("telemetry: listen address %q is not host:port: %w", c.Addr, err)
	}
	if port == "" {
		return fmt.Errorf("telemetry: listen address %q names no port", c.Addr)
	}
	return requireLoopback(host)
}

// requireLoopback rejects any host that is not, or does not resolve to, loopback.
//
// Ported from gateway/config.go deliberately rather than shared: the two modules
// are independent by design (this one links the collector tree, which must not
// reach the credential-scanned gateway module), and a copied 30-line check is a
// far smaller cost than a shared package that would join their dependency graphs.
// If a third lane needs it, that is the point to extract it.
func requireLoopback(host string) error {
	// The empty host, the wildcards, and "*" all mean "every interface" — the
	// exact opposite of the invariant, and the easiest thing to type by accident.
	if host == "" || host == "0.0.0.0" || host == "::" || host == "*" {
		return fmt.Errorf("telemetry: listen host %q binds every interface; loopback is required", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("telemetry: listen host %q is not loopback", host)
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), resolveTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return fmt.Errorf("telemetry: listen host %q is not an IP and does not resolve: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("telemetry: listen host %q resolved to nothing", host)
	}
	// EVERY address must be loopback, not merely one of them. A name that
	// resolves to both 127.0.0.1 and a routable address would otherwise pass on
	// the first entry while the listener binds the second.
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("telemetry: listen host %q resolves to %s, which is not loopback", host, addr)
		}
	}
	return nil
}
