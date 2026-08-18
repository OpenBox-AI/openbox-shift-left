# Security Scan Report

**Project:** openbox-shift-left (Go workspace, 11 modules) · **Scanned:** 2026-08-18
**Scope:** current branch `main` incl. uncommitted changes (prompt gate + HALT session exit) · **Mode:** --full

## Summary

| Category | Critical | High | Medium | Low/Info |
|---|---|---|---|---|
| Secrets | 0 | 0 | 0 | 0 |
| Dependencies | 0 | 0 | 0 | 1 note |
| Code patterns | 0 | 0 | 0 | 2 |
| .env exposure | 0 | 0 | 0 | 0 |

No critical, high or medium findings. Working-tree changes introduce no new risk surface
beyond what their ADRs document.

## Secrets — clean

- Token/key regex sweep (AWS, GitHub, Slack, Stripe/OpenAI-style, JWT, PEM) over tracked +
  untracked non-ignored files: **zero hits in non-test code**; no credential-shaped
  assignments; no connection strings with embedded credentials.
- All pattern hits are deliberate fixtures, verified dummy: the secret DETECTOR's own test
  corpus (`decision/secrets_test.go`, fuzz corpus, conformance C-cases, testbed §B) uses
  AWS's canonical documentation example key (`AKIA…LE`); `testPrivateKeyB64`
  (usage_test.go:23) is the sequential-byte seed 0x00–0x1f, not key material;
  posture_test.go:105 embeds the literal PEM *header string* to test leak scanning.
- `.env` never tracked; `.gitignore` covers `.env`, `.env.*` (allowing `.env.example`),
  `.claude/`, `.fab7/`.

## Dependencies — minimal surface

- Third-party across all 11 modules: **exactly one** — `golang.org/x/term v0.34.0` (cli,
  pinned deliberately per CLAUDE.md; no known CVEs for this package/version). Everything
  else is stdlib.
- **Note:** `govulncheck` is not installed, so no automated stdlib/toolchain advisory scan
  ran. Recommendation: add `govulncheck ./...` to CI — near-zero noise at one dependency,
  and it covers Go stdlib advisories (crypto, net/http) this repo does rely on.

## Code patterns — 2 low/info

1. **LOW — install.sh checksum verification is best-effort** (install.sh:149–161).
   Mismatch hard-fails (`die`) ✓, but a missing `checksums.txt` or absent sha256 tool warns
   and proceeds on the TLS-authenticated download. Since checksums.txt ships from the same
   origin as the tarball, it provides integrity, not authenticity — an attacker who can
   swap the asset can drop the checksums file. Standard `curl | bash` trade; options:
   sign releases (cosign/minisign) and verify in install.sh, or state the limit in the
   installer docs the way this repo documents other limits.
2. **INFO — testbed SQL uses shell string interpolation** (`testbed/lib/*.sh`, e.g.
   session.sh:78): session ids/agent ids interpolated into psql queries. Local,
   developer-run harness against a local stack with self-generated values — not a
   production injection surface. No change needed; noted for completeness.

Checked and clean:
- `exec.Command` sites are all fixed-argv (git ops, self re-exec for flush, approver host,
  provider `--version` probe via LookPath + 2s timeout). Untrusted hook-payload data never
  reaches a shell; the payload `cwd` flows to `git -C <cwd>` as an argv *value* (no shell,
  and `-C` consumes the next token, so a dash-prefixed path cannot become a flag).
- No `InsecureSkipVerify`, no `math/rand` in non-test code, no `unsafe`, no
  `pull_request_target`, no CI secret echoing (release.yml uses standard `GITHUB_TOKEN`).
- New session-halt latch: path traversal confined (sanitize + raw-id hash suffix,
  unit-pinned by `TestSessionHaltPathIsConfinedToHaltDir`); files 0600, dirs 0700 —
  consistent with every other sink.
- Client egress: `checkBaseURL` restricts http to loopback; redact-before-attach ordering
  pinned by conformance (C18/C26).

## Documented-by-design (not findings)

Listed so they are not re-reported as discoveries: plaintext `~/.openbox/.env` 0600 with no
at-rest protection on Windows (ADR-0015 — outside the repo); enforcement fail-open default
(`fail_closed:false`, ADR-0016/0017); prompt text egresses unredacted under content capture
(known limit, restated in ADR-0020-adjacent docs); the halt latch is developer-deletable
client state, not tamper-proof enforcement (ADR-0020).

## Recommendations

1. Add `govulncheck` to CI (one dep + stdlib advisories; near-zero noise).
2. Decide the installer authenticity story: release signing, or an explicit documented
   limit — this repo's own standard ("prefer an honest limit over a confident sentence")
   suggests at least the doc line.

## Unresolved questions

1. None blocking. Both recommendations are OD-class (owner priority calls), not defects.
