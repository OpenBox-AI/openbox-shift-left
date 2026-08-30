# Phase 02 — `~/.openbox/` layout, dotenv codec, migration

## Context links

- Parent: [plan.md](plan.md) · Depends on: phase 1 (decision record authorizes the posture)
- Blocks: phase 3 (rewire needs the codec + paths)
- Parallel-safe with: phase 4
- Prior art to reuse: `cli/internal/secret/file.go:80-111` (atomic temp+rename,
  0600 file under 0700 dir) — proven, copy the pattern rather than reinvent

## Overview

- **Date:** 2026-08-12
- **Description:** Establish `~/.openbox/` as the single neutral config dir on all
  three OSes, add a dotenv read/write codec, and migrate `dev.json` +
  `approver.json` from `os.UserConfigDir()`.
- **Priority:** P1
- **Implementation status:** implemented 2026-08-13
- **Review status:** self-reviewed; awaiting code-reviewer

## Key Insights

- `os.UserHomeDir()` + `.openbox` is one path on every OS. `~/.aws`, `~/.kube`
  and `~/.docker` all do exactly this **including on Windows**
  (`C:\Users\<you>\.openbox\`), so the convention is well-trodden.
- Deliberately **not** `os.UserConfigDir()` (what the repo uses today):
  `~/Library/Application Support/openbox` vs `~/.config/openbox` vs
  `%AppData%\openbox` — three paths is the thing being eliminated.
- **No build tags anywhere in this phase.** Dotenv + `os.UserHomeDir()` are
  platform-independent. The superseded DPAPI plan's `_windows.go`/stub split is
  void.
- The codec lives in **`devconfig`**, not `cli/`, because the runtime read side
  (`devconfig.ResolveCredentials`) must use it and `devconfig` cannot import
  `cli/`. Five modules already depend on `devconfig`, so this adds no new edge.
- **CRLF matters.** A Windows user editing `.env` in Notepad produces `\r\n`; a
  naive parser leaves `\r` on every value, and a `\r` inside a base64 seed fails
  signing with a baffling error. Strip explicitly, and test it.
- Migration must be **non-destructive**: read old path, write new, leave the old
  file. A deleted old file makes rollback lossy for no benefit.

## Requirements

1. `devconfig.Home()` → `$OPENBOX_HOME` if set, else `os.UserHomeDir()+"/.openbox"`.
   Creates with `0700` on first write, never on read.
2. `devconfig.EnvFilePath()`, `DevConfigPath()`, `ApproverConfigPath()` derived
   from `Home()`. `OPENBOX_CONFIG` (existing, `devconfig.go:60`) keeps overriding
   the dev.json path.
3. Dotenv **reader**: `#` comments, blank lines, optional `export ` prefix,
   `KEY=value`, single/double-quoted values, surrounding whitespace, CRLF.
   **Duplicate key ⇒ error**, not last-wins-silently.
4. Dotenv **writer**: atomic (temp + rename), `0600`, `0700` parent, stable key
   order, single-quoted values, and a header comment stating that this file is the
   sole copy of once-shown credentials and must not be committed.
5. The keys `auth` writes are **secrets only** (D2): `OPENBOX_API_KEY`,
   `OPENBOX_AGENT_PRIVATE_KEY`, and `OPENBOX_CONTROL_TOKEN` on approver installs.
   The codec stays a general `map[string]string` — this is a caller-side
   restriction, not a whitelist in the writer.
6. Writer preserves unknown keys and comments on rewrite where practical; at
   minimum it must not silently drop keys it did not author. This matters more
   under D2, not less: a user who hand-adds `OPENBOX_AGENT_DID` to `.env` to
   override a coordinate has authored a key the writer must keep, even though
   `auth` will never write it.
7. Migration: if the new `dev.json`/`approver.json` is absent and the old
   `os.UserConfigDir()` one exists, read it, write the new one, log one line
   naming both paths. Idempotent. Old file untouched. Note the asymmetry with D1:
   *config* migrates automatically, *keychain credentials* do not migrate at all.
8. No secret is ever logged, and no value appears in an error message.

## Architecture

New files in `adapters/common/devconfig/`:

| File | Contents |
|---|---|
| `paths.go` | `Home()`, `EnvFilePath()`, `DevConfigPath()`, `ApproverConfigPath()` |
| `envfile.go` | `ParseEnvFile(path) (map[string]string, error)`, `WriteEnvFile(path, map) error` |
| `migrate.go` | `MigrateLegacyConfig() (migrated []string, err error)` |

`ParseEnvFile` returns a plain map; precedence logic belongs to phase 3, not here.
Keep this phase a pure codec + path layer so it is unit-testable with no env or
filesystem globals beyond `OPENBOX_HOME` pointing at `t.TempDir()`.

## Related code files

| Path | Why |
|---|---|
| `cli/internal/secret/file.go:80-111` | atomic-write pattern to copy |
| `adapters/common/devconfig/devconfig.go:60` | `EnvConfigPath` = `OPENBOX_CONFIG`, must keep working |
| `adapters/common/devconfig/devconfig.go:~150` | `DevConfig.SecretFile` field — becomes vestigial, phase 3 removes |
| `adapters/common/devconfig/write.go:53` | `WriteConfig` — unchanged, but now writes to the new path |

## Implementation Steps

1. `paths.go` with `Home()` and the three path helpers. `Home()` never creates a
   directory; a separate `ensureHome()` does, called only from write paths.
2. `envfile.go` reader. Table-driven tests first: comments, blanks, `export `,
   both quote styles, whitespace, CRLF, duplicate-key error, missing file ⇒ empty
   map + nil error, unreadable ⇒ error.
3. `envfile.go` writer, reusing file.go's temp+rename+chmod sequence. Test:
   round-trip, 0600 perms, atomicity under a concurrent reader, header present.
4. `migrate.go`. Tests: new-absent+old-present ⇒ migrated; both present ⇒ no-op;
   neither ⇒ no-op; old unreadable ⇒ error surfaced, not swallowed.
5. Wire `WriteConfig`/`Load` call sites to the new `DevConfigPath()` — path change
   only, no format change.
6. `go test ./...` in devconfig; then `go build ./...` for all 11 modules to prove
   nothing else broke.

## Todo list

- [x] `paths.go` + `OPENBOX_HOME` override, 0700 on write only
- [x] dotenv reader with the full edge-case table incl. CRLF + duplicate-key error
- [x] dotenv writer, atomic, 0600, header comment, preserves unknown keys
- [x] `migrate.go` non-destructive + idempotent, one log line
- [x] dev.json/approver.json read+write use the new paths
- [x] devconfig tests green; all 11 modules build

## Success Criteria

- `OPENBOX_HOME=$(mktemp -d) go test ./...` green in devconfig with no writes
  outside the temp dir.
- A `.env` authored with CRLF parses to values with no trailing `\r`.
- A duplicate key produces a clear error naming the key and line number.
- Migration leaves the legacy file in place and is a no-op on second run.
- `0600` on the written file (asserted on unix; skipped with a reason on Windows,
  where it is a documented no-op).

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| Hand-rolled dotenv mis-parses a real value (base64 `=`, `+`, `/`) | M×H | round-trip test on a real 44-char seed fails | **Adjust:** the parser splits on the FIRST `=` only; add the real-seed case to the table permanently. |
| CRLF leaks into a value and breaks signing later | M×H | signature verification fails with an opaque error in phase 5/7 | **Adjust:** strip in the parser AND assert in phase 3's resolution tests, so the failure cannot reach signing. |
| `os.UserHomeDir()` empty in a container/CI | L×M | `Home()` errors on a runner with no HOME | **Accepted, mitigated:** require `OPENBOX_HOME` in that case and say so in the error text. |
| Migration runs concurrently from two processes | L×L | torn dev.json | **Accepted:** writer is atomic (temp+rename), so worst case is one of two identical writes winning. |
| Preserving unknown keys proves fiddly | M×L | writer drops a hand-added key | **Adjust:** if full comment preservation is not cheap, preserve *keys* only and document that comments may be rewritten. Do not silently drop keys. |

## Security Considerations

- The writer sets `0600` before writing content (chmod on the temp file, as
  file.go does), so the secret never exists at a wider mode even briefly.
- Parse errors must name the key and line, never the value.
- The header comment is a security control, not decoration: it is where a human
  discovers this file is the only copy and must not be committed.
- `Home()` under `OPENBOX_HOME` must not be used to write outside the user's own
  tree without their intent — reject a path that is not absolute.

## Next steps

Phase 3 consumes `ParseEnvFile` to make `~/.openbox/.env` a credential source and
deletes the keychain.
