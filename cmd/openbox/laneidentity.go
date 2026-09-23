package main

import (
	"github.com/openbox-ai/openbox-shift-left/internal/adapters/common/devconfig"
	"github.com/openbox-ai/openbox-shift-left/internal/client"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// providerIdentity is what a lane daemon pre-resolves ONCE at startup for one
// governed tool: its developer DID and the client.Client that SIGNS on its
// behalf. Resolved through devconfig's *For accessors, never through
// devconfig.BindProvider on the record path -- a lane daemon serves every
// configured tool for its whole life, and flipping the process-global bound
// provider per record would let two concurrent records resolve each other's
// identity store.
//
// The client is what actually decides who signs an egressed record, not the
// DID alone: a Codex record sent by the CC client would egress as the CC
// agent no matter what DID it carries. Named separately from
// cmd/openbox/initlane.go's laneIdentity (unit-file identity: label, systemd
// name, unit path), which this is not.
type providerIdentity struct {
	DID    string
	Client *client.Client
}

// resolveProviderIdentities pre-resolves one identity+client per
// provider.Supported() with a usable identity store. A provider with no
// store yet (no credentials, never `init`-ed) is skipped and reported through
// warn, not treated as a startup failure: a machine that has only run
// `openbox init --provider claude-code` legitimately has nothing for Codex to
// resolve, and the daemon still governs the tool(s) it can.
func resolveProviderIdentities(warn func(format string, args ...any)) map[string]providerIdentity {
	out := map[string]providerIdentity{}
	for _, tool := range provider.Supported() {
		creds, err := devconfig.ResolveCredentialsFor(tool)
		if err != nil {
			if warn != nil {
				warn("openbox: %s has no usable identity yet (%v); its lane records will not be sent", tool, err)
			}
			continue
		}
		c, err := client.New(client.Config{
			BaseURL:               creds.BaseURL,
			APIKey:                creds.APIKey,
			DID:                   creds.DID,
			PrivateKeyB64:         creds.PrivateKeyB64,
			ContentCaptureEnabled: creds.ContentCaptureEnabled,
			// Without this, client.Emit's own diagnostic lines (a delivery
			// failure's detail, a dropped-unbuildable-event reason) fell back to
			// nopLogger and were silently discarded for both lane daemons' send
			// path -- the caller-level "delivery failed: %v" still printed, just
			// without the detail Emit itself would have added. warnFn is the
			// adapter's own Printf-shaped adapters/claude-code and adapters/codex
			// pass their NewClient(logger); this is the fourth site, brought to
			// parity with the other three.
			Logger: warnFn(warn),
		})
		if err != nil {
			if warn != nil {
				warn("openbox: %s's identity could not build a client (%v); its lane records will not be sent", tool, err)
			}
			continue
		}
		out[tool] = providerIdentity{DID: creds.DID, Client: c}
	}
	return out
}

// warnFn adapts a Printf-shaped warn closure to client.Logger, so a nil warn
// (a caller that wants silence, e.g. a test) becomes a nil client.Logger --
// client.New already treats that as "default discards" -- rather than a
// non-nil interface value wrapping a nil func, which would panic on first
// use instead.
type warnLogger func(format string, args ...any)

func (f warnLogger) Printf(format string, args ...any) { f(format, args...) }

func warnFn(warn func(format string, args ...any)) client.Logger {
	if warn == nil {
		return nil
	}
	return warnLogger(warn)
}
