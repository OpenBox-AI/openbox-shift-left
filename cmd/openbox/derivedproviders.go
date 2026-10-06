package main

import (
	"github.com/openbox-ai/openbox-shift-left/internal/cli/activation"
	"github.com/openbox-ai/openbox-shift-left/internal/cli/providers"
	"github.com/openbox-ai/openbox-shift-left/internal/provider"
)

// systemPACSupportedFn is whether this OS has a system PAC activation at all.
// A seam so a test can stand a macOS machine up on any host, and a Linux or
// Windows one on macOS.
var systemPACSupportedFn = activation.SystemPACSupported

// derivedTransportProviders is the set of installed providers whose model
// calls can reach the transport relay on this machine. One answer feeds both
// halves of an install, so they cannot drift: it is written into the relay's
// unit as --providers (what the relay intercepts and serves in its PAC) and
// into the system-PAC activation record (what the election reads).
//
//   - claude-code routes through the relay by the env block in its
//     settings.json, on every OS;
//   - codex routes through it by the system PAC, which exists on macOS only;
//   - muse has no proxy lane: nothing has shown it follows the PAC or trusts
//     the relay's CA.
//
// installing is the provider the running `init` is for, which counts as
// installed before anything it writes exists. Order is stable so a repeated
// install renders the same unit.
func derivedTransportProviders(home string, installing provider.Name) []string {
	set := []string{}
	if installing == provider.ClaudeCode || laneRouted(home, activation.LaneTransport) {
		set = append(set, string(provider.ClaudeCode))
	}
	if systemPACSupportedFn() && (installing == provider.Codex || codexInstalled()) {
		set = append(set, string(provider.Codex))
	}
	return set
}

// codexInstalled reports whether OpenBox has installed anything for Codex on
// this machine: its owned [otel] block or its hook registration.
func codexInstalled() bool {
	if providers.HasOwnedCodexOtel(providers.CodexConfigTOMLPath()) {
		return true
	}
	_, present := codexHooksPresent()
	return present
}

// hasProvider reports whether set names p.
func hasProvider(set []string, p provider.Name) bool {
	for _, s := range set {
		if s == string(p) {
			return true
		}
	}
	return false
}

// hasTransportArm reports whether `init` for p installs the relay: Claude
// Code always, Codex only where a system PAC can route it.
func hasTransportArm(p provider.Name) bool {
	switch p {
	case provider.ClaudeCode:
		return true
	case provider.Codex:
		return systemPACSupportedFn()
	}
	return false
}
