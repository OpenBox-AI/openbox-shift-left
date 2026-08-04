#!/usr/bin/env bash
# testbed/lib/session.sh — drive a REAL Claude Code session, non-interactively.
#
# `claude -p` is not a mock of a session: the same hook entries fire, the same
# spool drains, the same POST /evaluate is signed and sent, the same OPA bundle
# decides. The only thing replaced is the person typing (plan §2).
#
# Every run happens inside $TB_PROJECT, the one directory `openbox init
# --local-hooks` governs.

# tb_session runs one prompt and prints the session id — which is core's
# `run_id`, so it is the join key for every later assertion.
#
#   sid=$(tb_session "Read README.md" "Read")
#
# Tools must be allow-listed: a headless session cannot answer a permission
# prompt, so an un-listed tool is refused and the assertion that wanted its
# span fails for the wrong reason.
tb_session() { # <prompt> [allowed-tools] [extra claude args…]
	local prompt="$1" tools="${2:-}"
	shift 2 2>/dev/null || shift $#
	local args=(-p "$prompt" --output-format json --model "$TB_MODEL" --no-session-persistence)
	[ -n "$tools" ] && args+=(--allowedTools "$tools")
	if [ -n "${TB_MCP_CONFIG:-}" ]; then
		args+=(--mcp-config "$TB_MCP_CONFIG" --strict-mcp-config)
	fi
	local out
	out="$(cd "$TB_PROJECT" && claude "${args[@]}" "$@" 2>"$TB_STATE/last-session.err")" || {
		printf ''
		return 0
	}
	printf '%s' "$out" >"$TB_STATE/last-session.json"
	tb_json "$out" session_id
}

# tb_session_bg starts a session and returns immediately, leaving its pid in
# TB_SESSION_PID.
#
# Needed because `claude -p` does not exit while an async rewake watcher is
# still waiting on an undecided approval: the watcher's window is 45 minutes.
# Interactively that is invisible (the developer is still in the session);
# headlessly it means any scenario that leaves a request pending must be driven
# from outside the session, and released before it can be joined.
tb_session_bg() { # <prompt> [allowed-tools]
	tb_session "$@" >"$TB_STATE/last-session.id" 2>&1 &
	TB_SESSION_PID=$!
}

# tb_session_wait joins a backgrounded session, killing it if it outlives the
# deadline. Returns 1 on timeout, which is itself a finding.
tb_session_wait() { # <seconds>
	local i=0
	while [ "$i" -lt "$1" ]; do
		kill -0 "$TB_SESSION_PID" 2>/dev/null || {
			wait "$TB_SESSION_PID" 2>/dev/null
			return 0
		}
		i=$((i + 1))
		sleep 1
	done
	kill "$TB_SESSION_PID" 2>/dev/null
	wait "$TB_SESSION_PID" 2>/dev/null
	return 1
}

# tb_session_text is the model's answer from the last tb_session run — used
# where the assertion is about what reached the model (a denial reason, a
# rewake notice) rather than about a database row.
tb_session_text() {
	tb_json "$(cat "$TB_STATE/last-session.json" 2>/dev/null)" result
}

# tb_session_uuid resolves a run_id to the sessions.id UUID that governance
# rows carry.
tb_session_uuid() { # <run-id>
	tb_val "select id from sessions where run_id='$1' limit 1;"
}

# tb_wait_for polls a command until it prints the wanted value, up to N
# seconds. Core's workers are asynchronous — asserting immediately after a
# session ends is a race, and a sleep long enough to be safe is a slow suite.
tb_wait_for() { # <want> <seconds> <command…>
	local want="$1" limit="$2"
	shift 2
	local i=0
	while [ "$i" -lt "$limit" ]; do
		[ "$("$@")" = "$want" ] && return 0
		i=$((i + 1))
		sleep 1
	done
	return 1
}
