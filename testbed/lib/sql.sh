#!/usr/bin/env bash
# testbed/lib/sql.sh — read the stack's database.
#
# SQL is the fallback, not the default: assert through the backend read API
# wherever one exists (plan §3 P6). It is used for what core exposes no read
# route for at all — spans, Merkle leaves, evaluation fan-out rows.

# tb_sql runs a query and returns pipe-separated rows, no headers.
tb_sql() { # <query>
	docker exec -i "$TB_PG" psql -U postgres -d "$TB_PG_DB" -t -A -F'|' -c "$1" 2>/dev/null
}

# tb_val returns a single scalar (an id, a count) with surrounding whitespace
# stripped. Not for values that may contain spaces.
tb_val() { # <query>
	tb_sql "$1" | head -1 | tr -d '[:space:]'
}

# tb_count is the shape most assertions want.
tb_count() { # <from-and-where clause>, e.g. "spans where session_id='…'"
	tb_val "select count(*) from $1;"
}

# tb_val_strict is tb_val for assertions where an ERROR MUST NOT LOOK LIKE A ZERO.
#
# tb_sql sends stderr to /dev/null, so a query naming a column that does not exist
# returns empty — and the `${var:-0}` idiom every caller uses then turns that into
# "0". For a counting assertion phrased as "no leaked credentials found", a broken
# query is therefore indistinguishable from a clean result, and it PASSES.
#
# That is not hypothetical: 45-gateway.sh's credential-leak check — the single most
# security-critical assertion in the suite — queried `spans.request_headers`, a
# column that does not exist, and would have printed a green tick the first time
# anyone ran it.
#
# So this variant keeps stderr, and any caller must treat a non-zero exit as a
# FAILED ASSERTION rather than as a zero count.
tb_val_strict() { # <query>  -> prints value, exit non-zero on SQL error
	local out rc
	out="$(docker exec -i "$TB_PG" psql -U postgres -d "$TB_PG_DB" -t -A -F'|' -v ON_ERROR_STOP=1 -c "$1" 2>&1)"
	rc=$?
	if [ $rc -ne 0 ]; then
		printf 'SQL ERROR: %s\n' "$out" >&2
		return 1
	fi
	printf '%s' "$out" | head -1 | tr -d '[:space:]'
}

# tb_count_strict is tb_count with the same guarantee.
tb_count_strict() { # <from-and-where clause>
	tb_val_strict "select count(*) from $1;"
}
