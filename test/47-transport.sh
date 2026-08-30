#!/usr/bin/env bash
# 47-transport.sh — the in-path :proxy: transport relay, against a real stack and
# a real session.
#
# ── WHAT THIS PHASE IS FOR ────────────────────────────────────────────────────
#
# Phase 13 replays a REAL recorded model call across the CONNECT path without a
# socket: goproxy's hijack, a real TLS handshake against the project CA, the real
# gateway relay, a real spool file. That covers byte-identity in both directions,
# per-chunk SSE delivery, the capture and the redaction. Everything below is what
# replay cannot reach:
#
#   * a real LISTENER, a real dialer, a real TLS session to a real provider;
#   * that the CA this installs is actually TRUSTED by the running Claude Code;
#   * that :proxy: turns reach governance_events;
#   * that `--remove-all` returns the machine to its baseline — the OD2 half that
#     no unit test can observe, because it is a property of the system, not of a
#     process.
#
# ── THE FAILURE THIS FILE EXISTS FOR ──────────────────────────────────────────
#
# An in-path relay fails DANGEROUSLY. A buffered response stalls a real session
# under Claude Code's 180s watchdog. A mangled header removes a capability with no
# error. And the self-loop — a daemon that inherits the HTTPS_PROXY pointing at
# itself — recurses until the machine runs out of sockets. transport.New clears
# the six proxy variables in its constructor for exactly that reason, and this is
# the only place that clearing is exercised against a real environment.
#
# DORMANT: written, never run. No stack has been reachable from this branch.
set -uo pipefail

TB_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$TB_DIR/env.sh"
. "$TB_DIR/lib/assert.sh"
. "$TB_DIR/lib/sql.sh"
. "$TB_DIR/lib/session.sh"

[ -d "$TB_PROJECT/.claude" ] || tb_fatal "project not governed — run 10-onboard.sh first"
TB_AGENT="${TB_AGENT:-$(tb_state_get agent_id)}"
[ -n "$TB_AGENT" ] || tb_fatal "no agent id in test state — run 10-onboard.sh first"

PX_ADDR="${TB_TRANSPORT_ADDR:-127.0.0.1:8790}"
SETTINGS="${HOME}/.claude/settings.json"
CA_DIR="${HOME}/.openbox"

# ── 47.0  install through the REAL command ────────────────────────────────────
tb_step "47.0  install the transport lane via \`openbox init --transport\`"
if ! "$OPENBOX_BIN" init --provider claude-code --transport --transport-addr "$PX_ADDR" \
	>"$TB_STATE/px-init.log" 2>&1; then
	tb_bad "openbox init --transport failed"
	tb_note "$(tail -5 "$TB_STATE/px-init.log")"
	tb_finish
	exit 1
fi
tb_ok "transport lane installed"

if nc -z "${PX_ADDR%%:*}" "${PX_ADDR##*:}" 2>/dev/null; then
	tb_ok "relay listening at $PX_ADDR"
else
	tb_bad "the relay is not listening at $PX_ADDR after a successful init"
	tb_note "init writes HTTPS_PROXY only after proving the listener is up. A set var"
	tb_note "over a dead port fails EVERY model call on this machine — loopback fails"
	tb_note "closed — while init reports success."
fi

# ── 47.1  the CA exists and is name-constrained ───────────────────────────────
# The constraint is what survives the trust boundary ADR-0015 already concedes:
# the key is readable by anything running as the developer, so a leaked key must
# not be able to mint a certificate for anything but the intercepted host.
tb_step "47.1  the project CA is name-constrained to the provider host"
CA_PEM="$CA_DIR/transport-ca.pem"
if [ -f "$CA_PEM" ]; then
	if openssl x509 -in "$CA_PEM" -noout -text 2>/dev/null | grep -qi "X509v3 Name Constraints"; then
		tb_ok "CA carries name constraints"
	else
		tb_bad "the CA has NO name constraints"
		tb_note "a leaked key could then mint a usable certificate for any host on this"
		tb_note "machine; the constraint is applied at GENERATION and cannot be added later."
	fi
else
	tb_bad "no CA at $CA_PEM"
fi

# ── 47.2  the self-loop is not armed ──────────────────────────────────────────
# A daemon that inherits the HTTPS_PROXY activation wrote dials ITSELF: CONNECT →
# hijack → serve → Do() → HTTPS_PROXY → CONNECT, until sockets run out. Cleared in
# the constructor because net/http caches the environment behind a sync.Once, so a
# later clear does nothing at all.
tb_step "47.2  the relay did not inherit a proxy pointing at itself"
if "$OPENBOX_BIN" doctor 2>&1 | grep -qi "proxy env cleared"; then
	tb_ok "doctor reports the inherited proxy environment cleared"
else
	tb_note "doctor did not report the cleared environment — inspect its transport block"
	tb_note "before trusting a green run below; a self-looping relay exhausts sockets"
	tb_note "rather than erroring, so the symptom is a hung session, not a failure."
fi

# ── 47.3  a real session's model calls traverse the relay ─────────────────────
tb_step "47.3  a real session produces :proxy: turns in governance_events"
before=$(tb_count "governance_events where agent_id='$TB_AGENT' and activity_id like '%:proxy:%'")
tb_session "Say the single word: transport." "none"
sid=$(tb_session_uuid "$(tb_state_get last_run)")
after=$(tb_wait_for "gt:$before" 60 tb_count "governance_events where agent_id='$TB_AGENT' and activity_id like '%:proxy:%'")
if [ "${after:-0}" -gt "${before:-0}" ]; then
	tb_ok "stored $((after - before)) :proxy: turn(s)"
else
	tb_bad "no :proxy: turn reached governance_events"
	tb_note "NEEDS A STACK. Byte-identity and per-chunk SSE across the CONNECT path"
	tb_note "are proven without one (cli/cmd/openbox/transportreplay_test.go)."
	tb_note "What this adds is a real socket, a real dialer, and a CA that the"
	tb_note "running Claude Code actually trusts."
fi

# ── 47.4  the response body is captured, not just the request ─────────────────
# The in-memory replay proves a body traverses the lane; only a live provider can
# show what a REAL response body does to the capture — including the one shape
# that defeats it, a content-encoded body, which is MARKED rather than decoded.
tb_step "47.4  the captured span carries a response body or its encoding marker"
n=$(tb_count "spans where session_id='$sid' and semantic_type='llm_completion'")
if [ "${n:-0}" -gt 0 ]; then
	tb_ok "$n llm_completion span(s) from the relay"
else
	tb_bad "no llm_completion span for session $sid"
fi

# ── 47.5  one command out ─────────────────────────────────────────────────────
# OD2's second half, and the only case here that is about the SYSTEM rather than a
# process. A removal that leaves HTTPS_PROXY behind points every model call on the
# machine at a dead port; one that leaves the CA behind leaves a trust anchor the
# developer never sees again.
tb_step "47.5  \`openbox init --remove-all\` returns the machine to baseline"
"$OPENBOX_BIN" init --provider claude-code --remove-all >"$TB_STATE/px-remove.log" 2>&1
residue=0
grep -q "HTTPS_PROXY" "$SETTINGS" 2>/dev/null && { tb_bad "HTTPS_PROXY survives removal"; residue=1; }
[ -f "$CA_PEM" ] && { tb_bad "the CA survives removal at $CA_PEM"; residue=1; }
nc -z "${PX_ADDR%%:*}" "${PX_ADDR##*:}" 2>/dev/null && { tb_bad "the relay is still listening after removal"; residue=1; }
[ "$residue" -eq 0 ] && tb_ok "settings, CA and unit all removed"

tb_finish
