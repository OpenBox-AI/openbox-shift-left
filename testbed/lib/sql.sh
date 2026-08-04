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
