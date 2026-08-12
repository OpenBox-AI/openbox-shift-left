> **SUPERSEDED 2026-08-12.** The keychain/DPAPI design was dropped at the user's
> direction in favour of a plaintext dotenv file at `~/.openbox/.env` with keychain
> support removed entirely. No DPAPI backend, no build-tag split, no `x/sys`
> storage dependency. Kept for history and because its LocalFree/ownership and
> build-constraint findings would apply if the decision is ever revisited.
> Current design: [../plan.md](../plan.md) and ADR-0015 (phase 1).

# Windows DPAPI secret backend — research report

Scope: cli/internal/secret/ needs a windows.GOOS branch. Decision already made: DPAPI file, not Credential Manager. Q's below map 1:1 to the task's numbered list.

## 1. Calling DPAPI without cgo

`golang.org/x/sys/windows` **already exports** `CryptProtectData`, `CryptUnprotectData`, and the `DataBlob` type directly — confirmed on the live package doc page (pkg.go.dev/golang.org/x/sys/windows, Functions/Types sections). This was added via an explicit CL ("[sys] windows: add support for DPAPI", groups.google.com/g/golang-codereviews/c/vHa3w7q5Vm0), so it's not incidental — the Go team accepted DPAPI as in-scope for x/sys. Reference full wrappers exist too: github.com/billgraziano/dpapi, pkg.go.dev/github.com/zavla/dpapi — both are 3rd-party convenience layers *on top of* the same two functions, not needed if you call windows.CryptProtectData directly.

Minimal sketch (field names `Size`/`Data` per godoc; verify exact case in the installed x/sys version before coding):

```go
func protect(plain, entropy []byte) ([]byte, error) {
    in := windows.DataBlob{Size: uint32(len(plain)), Data: &plain[0]}
    var ent *windows.DataBlob
    if len(entropy) > 0 {
        ent = &windows.DataBlob{Size: uint32(len(entropy)), Data: &entropy[0]}
    }
    var out windows.DataBlob
    if err := windows.CryptProtectData(&in, nil, ent, 0, nil,
        windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
        return nil, err
    }
    defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data))) // MUST free — API allocates via LocalAlloc
    cipher := make([]byte, out.Size)
    copy(cipher, unsafe.Slice(out.Data, out.Size)) // COPY before defer runs, else use-after-free
    return cipher, nil
}
```
Ownership hazard worth flagging explicitly: `out.Data` is allocated *by the DLL* (LocalAlloc contract, per learn.microsoft.com/.../nf-dpapi-cryptprotectdata), caller must LocalFree it — true whether you call via x/sys, cgo, or raw syscall, the Win32 ABI contract doesn't change. The bug to avoid: returning an `unsafe.Slice` view directly and letting `defer LocalFree` fire after — that frees the backing memory the returned slice still points at. Copy into a Go-owned `[]byte` first. `CryptUnprotectData` mirrors this exactly (same struct, same LocalFree obligation on its output blob).

## 2. Dependency question — the key one

- Manual `syscall.NewLazyDLL("crypt32.dll")` + `NewProc("CryptProtectData")` uses **only** `syscall` (stdlib) — zero new lines in cli/go.mod, zero go.sum entries. This is the literal "no new dependency" path.
- `golang.org/x/sys/windows` **is** a dependency in the go.mod/go.sum sense — it must be added, versioned, and shows up in `go mod tidy` / any dep audit. It is about as low-risk as a Go dep gets: maintained by the Go team, part of the x/ semi-stdlib set, and literally vendored *into the Go toolchain itself* (tip.golang.org/src/cmd/vendor/golang.org/x/sys/windows/) for cmd/go and cmd/link — that's about as strong a trust signal as exists outside stdlib proper. But it is still a new supply-chain node the repo doesn't have today.
- Trade-off is code-you-write vs. dependency-you-add: manual binding is ~40-60 LOC of struct/proc plumbing you own and test; x/sys/windows gives you the same two functions pre-wrapped for the cost of one go.mod line.

Given the repo's stated zero-dep invariant (context: "adding any third-party dep is an architectural departure that must be justified") and that the Win32 DPAPI ABI has been frozen since Windows 2000 (low maintenance risk either way), the manual-binding path is the one that doesn't require justifying an exception at all.

## 3. CRYPTPROTECT_* flags

- `CRYPTPROTECT_UI_FORBIDDEN` (0x1, WinCrypt.h) — required for any CLI/service call: forbids DPAPI from popping a UI prompt; without it a headless call could hang or fail unpredictably. learn.microsoft.com/.../nf-dpapi-cryptprotectdata confirms: with this flag + a promptStruct, the call fails with ERROR_PASSWORD_RESTRICTION instead of prompting — so also pass `nil` for the prompt struct.
- `CRYPTPROTECT_LOCAL_MACHINE` (0x4) — do **not** set. Docs: setting it means "any user on the computer... can decrypt the data" — the opposite of the user-scoped design already decided. Default (flag unset) ties the key to the caller's logon credentials, which is per-user scope.
- Optional entropy — pass a fixed, non-secret, app-specific byte string (e.g. a constant tied to the service name). Not a secrecy boundary (any code running as that user can supply the same constant), but it's free domain-separation so a blob written by another local app can't be swapped in and silently decrypt.
- Recommended flags: `CRYPTPROTECT_UI_FORBIDDEN` only, entropy = fixed app constant, promptStruct = nil, reserved = 0.

## 4. File placement — os.UserConfigDir()

Go stdlib (pkg.go.dev/os#UserConfigDir, `src/os/file.go`): on windows it returns `%AppData%` (Roaming), **not** `%LOCALAPPDATA%`. (High-confidence stdlib fact, not re-fetched live this pass — verify against installed Go version's doc if it matters.) Since the repo's file.go backend already calls os.UserConfigDir() today, keep using it unchanged for the DPAPI file too — same path on all 3 platforms, no new GOOS-specific path logic, and DPAPI's security guarantee doesn't depend on which folder holds the ciphertext.
Caveat: %AppData% is the Roaming profile — in AD domain-joined roaming-profile setups it (and DPAPI master keys) can sync across machines; on non-domain/local accounts or OneDrive Known-Folder-Move sync, a copied blob may land on a machine that can't decrypt it (different/unsynced master key) → worst case is a silent re-auth prompt, not a security hole. Acceptable, same failure class as any machine-bound secret.

## 5. File permissions / ACLs

`os.Chmod` on Windows only toggles the read-only attribute (owner-writable bit); it does not implement POSIX permission bits (pkg.go.dev/os#Chmod doc, stdlib fact, not re-fetched live). So "0600" is a no-op for actual access control on Windows — any local account can still open/read the raw file bytes.
Does that matter here? No, for confidentiality: DPAPI ties decryption to the calling user's logon credentials (learn.microsoft.com/.../nf-dpapi-cryptprotectdata — "typically only a user with the same logon credential... can decrypt"), so another account reading the raw bytes gets ciphertext it cannot open. DPAPI also embeds a MAC, so tampering is detected on unprotect, not silently accepted — covers integrity too.
So: real ACL restriction (e.g. via `SetNamedSecurityInfo`, or a community helper like github.com/hectane/go-acl — **not verified this session, flagging as unconfirmed**, would need its own dependency-cost check) is defense-in-depth against other-user deletion/DoS, not a confidentiality requirement. Recommend skipping ACL work for v1 — DPAPI alone satisfies the actual threat model (secrecy of the stored value); revisit only if a future audit wants belt-and-suspenders against local tampering.

## 6. Build-tag structure

Go's filename-based build constraints recognize exact GOOS suffixes automatically: `_windows.go` compiles windows-only, `_linux.go`/`_darwin.go` likewise (pkg.go.dev/cmd/go#hdr-Build_constraints — general Go knowledge, not re-fetched live this pass). There is no automatic "_unix.go" GOOS value — "unix" is a Go 1.19+ **pseudo build-tag** usable only via an explicit `//go:build unix` comment (matches linux, darwin, the BSDs, etc.); it is not a magic filename suffix on its own.

Given secret.go's existing `switch runtime.GOOS` (established context), the additive, non-invasive change:
- Add `case "windows": return newDPAPIStore(...)` to the existing switch in secret.go (secret.go itself carries no build tag today and must keep compiling on every GOOS).
- `secret_windows.go` (bare `_windows.go` suffix, no comment needed) — real impl using windows.CryptProtectData/CryptUnprotectData, defines `newDPAPIStore`.
- A second file with a **neutral name** (no GOOS suffix, e.g. `secret_dpapi_stub.go`) carrying an explicit `//go:build !windows` comment, providing a stub `newDPAPIStore` (e.g. returns `ErrNoStore`) so linux/darwin builds still resolve the symbol. Neutral name matters: naming it e.g. `secret_stub_windows.go` would itself be windows-suffixed and collide/duplicate-symbol on windows builds.

This mirrors stdlib's own impl+stub split pattern (os/removeall_at.go + removeall_noat.go, net/lookup_windows.go + lookup_unix.go). `go build ./...` on macOS simply never sees `secret_windows.go`'s file set — no import of windows-only packages leaks into non-windows builds, so nothing to guard beyond this file split.

## Comparable Go CLIs

Docker's `docker-credential-helpers` wincred backend and aws-vault's Windows backend both use github.com/danieljoos/wincred (github.com/docker/docker-credential-helpers/wincred; dev.to/jajera/... aws-vault wincred writeup) — i.e. they target **Windows Credential Manager**, not raw DPAPI, which is the option this repo already explicitly rejected. No mainstream Go CLI reference for "raw DPAPI file" was found in this pass beyond the standalone wrapper libs (billgraziano/dpapi, zavla/dpapi) — this repo's chosen approach (DPAPI-encrypted file, no Credential Manager) is less trodden than the wincred path; the two wrapper libs above are the closest prior art for the raw-DPAPI code shape itself.

## Recommended approach

1. Manual `syscall.NewLazyDLL("crypt32.dll")` bindings for CryptProtectData/CryptUnprotectData + own `dataBlob{cbData uint32; pbData *byte}` struct + `kernel32.LocalFree` — zero new deps, preserves cli/go.mod's invariant, Win32 ABI is frozen so low maintenance risk. Rank 1.
2. If the team decides a dep is acceptable: `golang.org/x/sys/windows` directly (skip 3rd-party wrapper libs) — same two functions, less code to own, at the cost of one go.mod line. Rank 2, acceptable fallback only if #1's hand-rolled binding is judged not worth owning.
3. Flags: CRYPTPROTECT_UI_FORBIDDEN only, fixed app-constant entropy, no LOCAL_MACHINE.
4. Path: keep os.UserConfigDir() (%AppData%) unchanged, consistent with existing file.go.
5. Permissions: DPAPI encryption alone; skip ACL work for v1.
6. Files: secret_windows.go (real, bare suffix) + secret_dpapi_stub.go (`//go:build !windows`, neutral name) + one new `case "windows"` line in secret.go's existing switch.

## Unresolved questions

- Exact field names/casing of `windows.DataBlob` (Size/Data vs cbData/pbData) — confirm against the actual installed x/sys/windows version before coding; this pass used the package doc summary, not raw source.
- `github.com/hectane/go-acl` (or equivalent) existence/API was not verified this session — only relevant if the team overrides the "skip ACL" recommendation.
- os.UserConfigDir/os.Chmod Windows semantics and the Go build-constraint filename rules are stated from well-established stdlib documentation but were not re-fetched live in this pass (budget-constrained); low risk given how stable these are, but worth a quick doc glance before implementation.
- No live example found of a shipping Go CLI using raw DPAPI (vs. Credential Manager) for its store — this repo's approach has less direct prior art than the wincred path other tools take.

Status: DONE_WITH_CONCERNS
Summary: x/sys/windows exports CryptProtectData/CryptUnprotectData/DataBlob directly (confirmed on pkg.go.dev), but manual syscall.NewLazyDLL binding is the zero-dependency path and is recommended given the repo's no-dep invariant; DPAPI user-scope encryption alone is sufficient without ACL work; build-tag split follows stdlib's impl+stub convention. Concerns: exact DataBlob field casing and the ACL-helper package were not independently verified this pass (budget-limited to 5 tool calls).
