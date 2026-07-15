# STORY-E6-S9 — Tier-1 local secret/entropy detection + redact-and-continue

**Epic:** E6 (enforcement — the Tier-1 local redaction SOURCE that makes the E6-S4 `updatedInput` seam live). **Risk:** high (this is the first path that both (a) inspects tool **content** on-by-default — decoupled from the egress content-capture opt-in — and (b) rewrites a tool's input before it runs; a bug either leaks content across INV-2, corrupts/loosens a tool call, or rewrites a structural locator). **Status target:** review (build + validations + both reviews, pending brian G3 + Sam G_SEC).

## Source
- **Design:** `.fab7/sdlc/design/sidecar-policy-sync.md` §7 (verdict tiering, ratified brian 2026-07-14) + §8 tiering follow-on `E6-S9`: "**Tier-1 local secret/entropy detection + redact-and-continue.** New deterministic secret detector in the sidecar evaluator; wire its redaction into the built E6-S4 `updatedInput` path (content-only fields). On by default, local-only (OD-SYNC-10). Highest adoption value; no core surface."
- **OD-SYNC-10 (RESOLVED, brian 2026-07-14):** Tier-1 secret/entropy detection runs **on by default, local-only** — inspects Edit/Write content locally, never egresses (finding/redaction stays on `sidecar.Decision`, never `client.Evaluation`), **decoupled from the content-capture-for-egress opt-in**. Honors INV-2 (egress-only). `updatedInput` rewrites **content-only fields**, never structural locators (`file_path`/`command`) — the E6-S4/S7 carry-forward.
- **Design §7 T1:** "The only tier allowed to block Edit/Write." Cached OPA policy **+ NEW local deterministic secret/entropy detectors (regex + high-entropy string detection, gitleaks/trufflehog-style)**. "Produces the **redact-and-continue** rewrite (secret → env-var ref) → feeds the **already-built E6-S4 `updatedInput` path**. This 'fourth verdict' is ~half-built already; it needs a local redaction *source*, not new apply plumbing. Rewrite content-only fields, never `file_path`/`command`."
- **E6-S4 (DONE, committed):** built the `updatedInput` apply path + the `RedactedInput` carrier on `sidecar.DecisionResponse`/`sidecar.Decision`, but it is **INERT in the field** — the metadata-only `bundleEvaluator` produces no redaction (`[EXT-guardrail-redaction]`). E6-S9 provides the FIRST live redaction source.
- **E6-S7 carry-forward (deferred to "the redaction-engine story" — this one):** "(1) constrain `updatedInput` to content-only fields — a compromised engine could rewrite structural locators (`file_path`/`command`), not just sanitize a body; (2) size-guard the `jsonEqual` double-parse [done in E6-S7]; (3) `fileText()` under-captures MultiEdit's nested `edits[].new_string`."

## Cross-repo recon (Explore, 2026-07-14)
- **Secret detection is genuinely NET-NEW.** No openbox repo carries an owned regex/entropy secret detector; the only "secret scan" anywhere is a separate MIT toolkit shelling out to Yelp `detect-secrets`. So there is **no existing pattern set / entropy threshold to mirror** — we author a conservative, low-false-positive set.
- **Reference SDK redaction shape = per-arg PARTIAL PATCH, not a monolithic blob** (`openbox-temporal-sdk-python activity_interceptor.py:441-478` + `_deep_update_dataclass:29-68`): `redacted_input` is positionally aligned to args; for a structured (dataclass) arg it is a **field-level merge** (only present keys overwrite; others untouched), for a plain value a full replacement; a non-dict → **warn-and-skip, return original unchanged**. This VALIDATES the content-field-reconstruction approach below over a blind full-object replacement.
- **OpenBox redaction placeholder precedent** (`openbox-guardrails-api`): PII → **typed placeholder `<ENTITY_TYPE>`** (`guardrails-pii/spans.py:17-20`); ban-list → **`*`×len length-preserving mask** (`ban_list_check.py:26-44`). **No env-var-ref style exists anywhere.** Design §7 explicitly ratified "secret → env-var ref" — we honor that (it is the correct *nudge for a secret*: point the developer at externalizing the value, distinct from masking PII), typed-but-env-var-shaped: `${OPENBOX_REDACTED_<CATEGORY>}`.

## The design (content-field reconstruction — the load-bearing security choice)
E6-S4 carried the redaction as `RedactedInput json.RawMessage` — a **full replacement `tool_input` object** emitted verbatim as CC `updatedInput`. That is exactly the structural-rewrite hole E6-S7 flagged: a buggy/compromised engine could alter `file_path`/`command`, not just sanitize a body. **E6-S9 closes it by construction:**

- The **sidecar** returns only the redacted **content string(s)** (on `DecisionResponse.RedactedContent *client.Content`, mirroring the request's `Content`), plus **`RedactionCategories []string`** (category names only — `aws_key`, `github_token`, `entropy`, … — never the secret, INV-2-safe).
- The **adapter** reconstructs the `tool_input` OBJECT itself: it unmarshals the ORIGINAL `tool_input`, replaces ONLY the recognized content field (Write `content` / Edit `new_string`) with the redacted string, and re-marshals. **Every structural field passes through byte-identical** — the adapter never copies a field from the sidecar. A misbehaving sidecar can only change a content-field VALUE; it can never introduce or alter `file_path`/`command`. This makes "content-only fields, never structural locators" a structural guarantee, not a convention.

This replaces the E6-S4 full-object `RedactedInput` carrier (rename → `RedactedContent`); the apply plumbing (`applyInputRedaction` → `updatedInput`, proceed-path-only, differ-check) is otherwise the E6-S4 shape, unchanged.

## The detector (`sidecar/secrets.go`) — deterministic, local, on-by-default
- **Named high-confidence patterns** (low false-positive): AWS access key id (`AKIA`/`ASIA`…), AWS secret assignment, GitHub tokens (`ghp_`/`gho_`/`ghs_`/`ghr_`/`github_pat_`), Slack (`xox[baprs]-`), Google API (`AIza…`), Stripe (`sk_live`/`rk_live`…), OpenAI/Anthropic-style (`sk-…`/`sk-ant-…`), JWT (`eyJ….eyJ….…`), PEM private-key blocks, and a generic `KEY=VALUE` assignment for `api_key|secret|token|password|…` (value-only redaction, key preserved).
- **Entropy fallback** (gitleaks-style): candidate tokens (base64/hex runs ≥ a min length) with Shannon entropy over a conservative threshold are redacted — catching generic high-entropy secrets no named pattern covers. Tuned to avoid flagging ordinary prose/code identifiers.
- **Replacement:** each secret → `${OPENBOX_REDACTED_<CATEGORY>}` (env-var-ref, design §7). Pure, stateless, **concurrency-safe** (regexes compiled once at package init), **no network, no logging of the content or the secret** (INV-1/INV-2).
- **Runs decoupled from policy** (OD-SYNC-10): in `Server.decide`, on `req.Content.FileText`, **regardless of the policy verdict or whether a bundle is loaded** (even at cold-start `eval==nil`). It NEVER changes the verdict — redact-and-continue is a proceed-path rewrite orthogonal to deny/ask.

## The on-by-default, egress-decoupled gate (OD-SYNC-10 / INV-2 crux)
- New `ResolveSecretDetection()` — **default TRUE** (env `OPENBOX_SECRET_DETECTION` / config `secret_detection`, env wins; a missing config is TRUE so the protection is on by default; an explicit `false` opts out). Only meaningful in enforce mode.
- `buildDecisionRequest` populates `DecisionRequest.Content` (the file body) when **`secretDetection || contentCapture`** (was: content-capture only). So the body reaches the LOCAL sidecar for secret scanning **on by default** — but ONLY the local Unix socket. **The egress path is unchanged:** the observe Mapper still sends content only under the OD4 content-capture opt-in; `RedactedContent`/`RedactionCategories` ride the LOCAL `sidecar.Decision`, never `client.Evaluation` → never the advisory sink, the enforcement audit body, or the `/evaluate` egress. Local content inspection ≠ egress; INV-2 is egress-only.
- The apply (`applyInputRedaction`) is likewise gated on `secretDetection || contentCapture`. With BOTH off, the whole path is byte-identical to E6-S3.

## Scope boundary (what this story IS and is NOT)
- **IS:** `sidecar/secrets.go` (the detector); wire the scan into `Server.decide` (decoupled from policy); rename the carrier `RedactedInput`→`RedactedContent *client.Content` + add `RedactionCategories []string` on `DecisionResponse`/`Decision`; rework `applyInputRedaction` to **reconstruct** the tool_input touching only content fields; `ResolveSecretDetection()`; widen the content-to-sidecar + apply gate to `secretDetection || contentCapture`; thread it in `hookrun.go`; record a content-free `redacted`+`redaction_categories` audit signal; tests + a secret-in-Write E2E + conformance cases.
- **IS NOT:** the verdict cascade / deny-ask writer (E6-S2, unchanged); the failure policy (E6-S3, unchanged); the `sidecar.Client` fail-open primitive (unchanged); Tier-2 sync `/evaluate` escalation (E6-S10); the rego evaluator / policy sync (E6-S8); Bash-command redaction (§7 T1 scopes redaction to Edit/Write bodies — rewriting a command is a riskier semantic change, out of scope); MultiEdit `edits[].new_string[]` (under-captured by `fileText()`; under-capture is INV-2-safe → nothing to redact; carried forward). **NO core/backend surface.**

## Acceptance Criteria
1. **Live redaction (redact-and-continue)** — enforce on + secret detection on (default): a `Write`/`Edit` whose body contains a detectable secret and whose verdict is proceed (no deny/ask) → the sidecar returns `RedactedContent` + `RedactionCategories`; `applyDecision` emits `hookSpecificOutput.updatedInput` = the ORIGINAL tool_input with ONLY the content field replaced by the redacted body, NO `permissionDecision`, exit 0. The tool then runs under CC's own flow on the sanitized input.
2. **Structural fields are inviolable (E6-S7 carry-forward closed)** — the emitted `updatedInput` is byte-identical to the original in every field except the single content field; `file_path` (and any other structural key) is never added, dropped, or altered, EVEN IF the sidecar returns a `RedactedContent` that tries to. A test drives a hostile/oversized `RedactedContent` and asserts structural fields survive verbatim.
3. **On by default, egress-decoupled (OD-SYNC-10 / INV-2)** — with `content_capture` OFF (the default) but secret detection ON (the default), a file-body secret is still detected + redacted locally. The body reaches ONLY the local socket; a test asserts the emitted `/evaluate` payload, the advisory sink, and the enforcement audit contain NO file body and NO secret/redacted content (only category names in the audit). `client.Evaluation`/`GuardrailResult` never carry redaction.
4. **Opt-out honored + inert-when-both-off** — `OPENBOX_SECRET_DETECTION=false` (or config) disables the detector: no content sent for scanning by that leg, no redaction applied. With secret detection AND content capture both off, the PreToolUse path is **byte-identical to E6-S3** (a test asserts both legs inert).
5. **No loosening / no-op safety** — `permissionDecision:"allow"` is NEVER emitted; `updatedInput` only ever STRIPS content. A body with no detectable secret, or where the redacted body equals the original, emits nothing (no pointless rewrite). A non-object / empty / null reconstruction is skipped (never rewrites a tool call to garbage).
6. **Detector correctness + no false-positive floods** — named patterns detect representative real-shaped secrets and redact them to `${OPENBOX_REDACTED_<CATEGORY>}`; the entropy fallback catches a generic high-entropy key; ordinary prose/code (imports, comments, short identifiers, lorem-ipsum) is NOT redacted. Deterministic + concurrency-safe (a `-race` test scans concurrently). The secret plaintext never appears in the detector's return beyond the redacted output, and never in any log.
7. **Verdict independence + fail-open** — the secret scan never changes the verdict (a BLOCK stays a deny; an ALLOW stays proceed); it runs even with no policy bundle (cold start). Any scan/marshal fault degrades to "no redaction" (proceed on the original), never wedges or blocks a tool call (INV-3b fail-open).

## Write Scope
- `sidecar/secrets.go` (new) — the deterministic detector + env-var-ref redaction.
- `sidecar/secrets_test.go` (new) — detection/redaction/entropy/false-positive/`-race` cases.
- `sidecar/protocol.go` — `DecisionResponse`: rename `RedactedInput`→`RedactedContent *client.Content`; add `RedactionCategories []string`.
- `sidecar/server.go` — run the scan in `decide` (decoupled from policy/bundle); attach `RedactedContent`/`RedactionCategories`; `Server` holds the scanner (default constructed); `ServerConfig` optional override.
- `sidecar/client.go` — `Decision`: rename `RedactedInput`→`RedactedContent`; add `RedactionCategories`; copy both in `Decide`.
- `adapters/claude-code/enforce.go` — `applyInputRedaction` → content-field reconstruction (`redactToolInput`); `applyDecision`/`buildDecisionRequest` gate `secretDetection || contentCapture`; `recordEnforcement` gains content-free `Redacted`+`RedactionCategories`.
- `adapters/claude-code/creds.go` — `ResolveSecretDetection()` (default TRUE) + `DevConfig.SecretDetection *bool` + `envSecretDetection`.
- `adapters/claude-code/hookrun.go` — resolve secret detection; thread `localRedaction = secretDetection || contentCapture` into the enforce branch.
- `adapters/claude-code/enforce_test.go`, `enforce_conformance_test.go`, `creds_test.go`, `sidecar/protocol_test.go`, `sidecar/server_client_test.go` — the ACs above.

## Invariants
- **INV-2 (load-bearing):** file body reaches ONLY the local sidecar (on-by-default for local scan, decoupled from egress opt-in); `RedactedContent`/`RedactionCategories` are LOCAL-only (never `client.Evaluation`, never egressed / logged / in either JSONL sink except category names). The observe Mapper egress path is unchanged (still metadata-only unless content-capture on).
- **INV-3b:** redaction is applied pre-execution on the proceed path; the scan is in-process (no network, no unbounded wait); any fault fails open (proceed).
- **Content-only rewrite (structural guarantee):** `updatedInput` differs from the original only in a recognized content field; structural locators are reconstructed from the original, never from the sidecar.
- **Tighten-only:** stdout carries only `deny`/`ask` OR a content-STRIPPING `updatedInput`; never `permissionDecision:allow`.

## Human Gates
| Gate | Question | Owner | Outcomes |
|---|---|---|---|
| G3_REVIEW | Does the local detector + content-field reconstruction correctly produce a redact-and-continue `updatedInput`, on-by-default, that only ever strips content and never touches structural fields — with sensible detection and no false-positive floods? | brian | approve / revise |
| G_SEC | Is the file body strictly LOCAL (never egressed/audited/logged; the observe path unchanged), the on-by-default decoupling genuinely INV-2-safe (egress-only), the structural-field guarantee airtight against a hostile `RedactedContent`, and the whole path fail-open? | Sam | approve / revise / block |

## Validation
```bash
cd sidecar && go build ./... && go vet ./... && go test -race ./...
cd ../adapters/claude-code && go build ./... && go vet ./... && go test -race ./...
cd ../../cli && go build ./... && go vet ./... && go test ./...
# Live: enforce on + secret detection ON (default) + content_capture OFF, `openbox sidecar serve`:
#   Write with an AWS key in `content` → stdout updatedInput has content redacted, file_path intact, no permissionDecision.
# Assert: /evaluate payload + advisories.jsonl + enforcements.jsonl carry NO file body / no secret (audit: category names only).
# Live: OPENBOX_SECRET_DETECTION=false + content_capture off → byte-identical to E6-S3 (no updatedInput).
```

## Stop conditions
- If a file body, a secret, or redacted content ever appears on `client.Evaluation`, in `enforcements.jsonl`/`advisories.jsonl` (beyond category names), or in an egressed `/evaluate` payload → STOP (INV-2).
- If the emitted `updatedInput` ever differs from the original in a structural field (`file_path`/`command`/anything but a content field) → STOP (the E6-S7 carry-forward).
- If a redaction ever emits `permissionDecision:allow`, or rewrites to an empty/identical/invalid input → STOP.
- If the secret scan ever changes the verdict, blocks, or wedges a tool call on a fault → STOP (it is a proceed-path, fail-open rewrite).
- If this story modifies `mapVerdict`/`applyFailurePolicy`/the `sidecar.Client` fail-open primitive, touches core/backend, or adds Tier-2 network escalation → STOP (out of scope).
- If the secret plaintext is logged anywhere → STOP (INV-1).
