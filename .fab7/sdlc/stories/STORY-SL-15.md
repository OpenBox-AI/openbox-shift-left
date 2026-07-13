# STORY-SL-15 — Real OwnershipVerifier against the FR-7 session-ownership read API

**Risk:** medium (attribution correctness / SL5-SEC-1 — a wrong verifier would over-attribute a forged trailer; must stay fail-closed)

## Source
- **Architecture:** INV-6 (no silent wrong attribution), FR-7 (lineage read), **SL5-SEC-1** carry (the trailer is an untrusted claim — bind each value to a session owned by the authenticated pusher).
- **Backlog:** review follow-up **SL6-OWNERSHIP** — "wire a real OwnershipVerifier (backend session-ownership lookup keyed on the pusher's developer identity) so owned sessions become `attributed`; until then every deploy is honestly `inferred`."
- **Session:** Phase-1 debt review (2026-07-13) item #3. **Honest scope:** the seam is built; the read API is external (EXT-lineage/FR-7). Shift-left builds + unit-tests the verifier against the assumed API surface (OD14) and ships it **flag-off** until FR-7 is confirmed live.

## Inlined context (verified — builder need not re-read)
- **The seam already exists** (`actions/openbox-git-action/ownership.go`): `OwnershipVerifier{ OwnsSession(ctx, id) (bool, error) }`, default `NoopVerifier` (always `false,nil` → everything `inferred`), and a `verifierFunc` adapter. Contract: return `(true,nil)` ONLY on positively-established ownership; `(false, err)` is treated the same as unverified — **fail-closed: never over-attribute on a lookup failure.** A real verifier "promotes owned sessions to Attributed with no change" to the resolver.
- **The pusher's identity is available at deploy time:** the git-action already resolves `OPENBOX_DID` + `OPENBOX_API_KEY` + `OPENBOX_SEED` (`action.go`/`main.go`) — the authenticated deploying developer agent. The verifier binds trailer session ids to sessions owned by THIS DID (mirrors SL-3's DID cross-binding).
- **Signing is reusable:** the SL-3 `client` signer (`client/signing.go`) can sign a `GET` (method-agnostic — see SL-11's `Validate`), so the verifier's read call is AIP-signed like every other data-plane request.
- **Phase-1 default must stay honest:** with the flag off / API absent, deploys resolve `inferred`, `verified_session_ids` empty — unchanged from today.

## Acceptance Criteria
- New `apiVerifier` implementing `OwnershipVerifier`: an **AIP-signed** `GET` against the FR-7 read endpoint (assumed shape: `GET <backend>/agent/<pusherDID>/sessions` or a `sessions?owner=<did>` query), returning `(true,nil)` iff the resolved `sessionID` is in the pusher's owned set; `(false,nil)` when provably not owned or indeterminate; `(false, err)` on transport/lookup fault.
- **Fail-closed preserved:** any error, non-2xx, ambiguous body, or absent-API → `false` (never promote). A unit test proves a lookup error does NOT attribute.
- **Flagged, Noop default:** selected by an explicit env flag (e.g. `OPENBOX_OWNERSHIP_VERIFY=1` + the backend URL); absent → `NoopVerifier` (today's behavior byte-for-byte). No resolver/classify change — owned ids simply become `attributed`/`verified_session_ids` populated.
- **SL5-SEC-1 discharged for real** where the API exists: a forged trailer id (a session the pusher doesn't own) stays out of `verified_session_ids` and resolves `inferred`.
- Unit-tested against a **mock** FR-7 endpoint (owned → attributed; not-owned → inferred; error → inferred/fail-closed).

## Nonfunctional Requirements
- **security:** the read call is signed (INV-1: key in header/seed in sign only); a forged/other-org session never promotes (INV-4/INV-6). Requires security review (attribution boundary).
- **reliability:** bounded timeout; a slow/absent API degrades to `inferred`, never hangs the deploy or over-attributes.

## Write Scope
- `actions/openbox-git-action/` — new `apiVerifier` (+ flag wiring in `action.go`/`main.go`); reuse the SL-3 signer.

## Dependencies
- **Hard:** STORY-SL-6 (resolver + verifier seam), STORY-SL-3 (signed client).
- **External (assumed-satisfied, OD14):** [EXT-lineage / FR-7] the session-ownership read API. Buildable + unit-testable now against a mock; **cannot be live-verified until FR-7 exists → ships flag-off.**

## Invariants
- **INV-6:** never silent wrong attribution; unowned/indeterminate stays `inferred`.
- **SL5-SEC-1:** trailer is a claim, promoted only on positive ownership by the authenticated pusher.
- **INV-1:** signed read; no secret leak. **INV-4:** cross-org sessions never promote.

## Human Gates
| Gate | Question | Owner | Evidence Needed | Allowed Outcomes |
|---|---|---|---|---|
| G1_READY | Confirm the FR-7 read endpoint shape the verifier targets (OD-OWNER-API) | brian (architecture) | named endpoint + auth model | confirm shape / defer to assumed shape |
| G_SEC | Is the ownership check fail-closed and secret-safe (no over-attribution, no cross-org promote)? | Sam (security reviewer) | review of the verifier + fail-closed tests | approve / revise / block |
| G3_REVIEW | Does the real verifier promote owned → attributed with no resolver regression? | brian | diff review + mock-API tests | approve / revise |

## Validation
```bash
cd actions/openbox-git-action && go build ./... && go vet ./... && go test -race ./...
# mock FR-7: owned id -> attributed + verified_session_ids populated; not-owned -> inferred; lookup error -> inferred (fail-closed);
# flag off -> byte-identical to NoopVerifier (today's output).
```

## Stop conditions
- If the FR-7 endpoint shape/auth cannot be confirmed → implement against the documented assumed shape behind the flag (OFF), unit-test with a mock, and HALT live activation with a note (OD-OWNER-API pending). Do NOT enable by default on an unverified API.
- If any path could promote a session the pusher does not own → STOP (INV-6/SL5-SEC-1 breach); fail-closed is non-negotiable.
