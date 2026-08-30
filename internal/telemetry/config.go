// Package telemetry receives the coding tool's own OpenTelemetry export over a
// loopback OTLP endpoint and hands each record to an emitter. It exists
// because the gateway lane cannot see the traffic that matters most. What it
// claims is weaker, and the difference must survive into every reader: the
// transport and gateway lanes observe the bytes in path, while this one is the
// governed tool reporting ITS OWN calls.
package telemetry

import (
	"context"
	"fmt"
	"net"
	"time"
)

// DefaultAddr is this receiver's loopback endpoint.
const DefaultAddr = "127.0.0.1:8789"

const resolveTimeout = 2 * time.Second

// MaxRequestBodyBytes bounds one OTLP request.
const MaxRequestBodyBytes = 8 * 1024 * 1024

// Config is the receiver's listen configuration.
type Config struct {
	// Addr is the loopback host:port the OTLP endpoints are served on.
	Addr string
}

// Validate fills defaults and rejects a configuration that would break the one
// invariant this receiver rests on: it is reachable from this machine only.
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

// requireLoopback rejects any host that is not, or does not resolve to,
// loopback.
func requireLoopback(host string) error {
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
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("telemetry: listen host %q resolves to %s, which is not loopback", host, addr)
		}
	}
	return nil
}
