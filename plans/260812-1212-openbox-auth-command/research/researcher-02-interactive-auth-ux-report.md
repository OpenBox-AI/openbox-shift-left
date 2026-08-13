# Interactive Auth UX Research — `openbox auth`

## 1. Masked terminal input (cross-platform)
`golang.org/x/term` is the Go-team-maintained standard: `ReadPassword(fd int) ([]byte, error)` reads a line with echo off, no trailing `\n`. Built for linux/amd64, windows/amd64, darwin/amd64, js/wasm. [pkg.go.dev/golang.org/x/term](https://pkg.go.dev/golang.org/x/term)
- Windows IS natively supported, not emulated: x/term's windows build talks straight to `golang.org/x/sys/windows` `GetConsoleMode`/`SetConsoleMode` to clear `ENABLE_ECHO_INPUT` — no shelling out. Confirmed via package internals writeups. [GoLinuxCloud walkthrough](https://www.golinuxcloud.com/golang-hide-password-input/), [DeepWiki x/term internals](https://deepwiki.com/golang/term/2.2-reading-passwords)
- Unix build uses termios ioctl (`TCGETS`/`TCSETS`) via `x/sys/unix` to flip the ECHO bit — same public API, no branch needed in our code.
- (b) `stty -echo`: external unix-only binary; no `stty.exe` ships with native `cmd.exe`/PowerShell (only present under Git Bash/WSL) — ruled out for a native-Windows target.
- (c) hand-rolled per-platform syscalls: this is literally what x/term already does internally. Reimplementing duplicates ~200 LOC of hardened, Go-team code for zero gain.
- **Recommend:** take `x/term` as cli's first external dep. It's `golang.org/x/...` (same trust tier as `x/sys`/`x/crypto`, Go-team owned, minimal transitive deps) — the one dependency worth breaking the zero-dep baseline for. Hand-rolled syscalls are strictly worse: more code, more platform bugs, no security upside.

## 2. TTY detection
Two viable routes:
- `term.IsTerminal(int(os.Stdin.Fd()))` — same package as #1, one call, cross-platform, zero added dependency cost. [pkg.go.dev/golang.org/x/term](https://pkg.go.dev/golang.org/x/term)
- stdlib-only: `os.Stdin.Stat()` then `mode&os.ModeCharDevice != 0`. **Windows caveat (documented Go bug):** on Windows only `ModeCharDevice` is set for console handles, `ModeDevice` is NOT — code that checks `ModeDevice|ModeCharDevice` breaks on Windows unless special-cased by `runtime.GOOS`. [golang/go#23123](https://github.com/golang/go/issues/23123)
- Related gotcha: `os.Stdin.Stat()` doesn't reliably reflect a redirected stdin under Windows-hosted WSL. [golang/go#33570](https://github.com/golang/go/issues/33570). `github.com/mattn/go-isatty` exists specifically to paper over these Windows/Cygwin/MSYS2 gaps if we ever need more than x/term gives us.
- **Recommend:** `term.IsTerminal` — already a dependency from #1, sidesteps the `os.Stat` Windows footgun entirely. Fail fast with a clear message ("requires an interactive terminal; use automation flags/env for scripts") instead of hanging when piped or in CI.

## 3. Non-interactive convention (gh / docker / aws / kubectl survey)
- `docker login --password-stdin` is the canonical answer: docs explicitly warn `--password <val>` leaks into shell history/process list/logs, and prescribe piping instead (`cat secret.txt | docker login -u USER --password-stdin`). Same convention reused verbatim by GCR and GHCR examples. [docs.docker.com/reference/cli/docker/login](https://docs.docker.com/reference/cli/docker/login/), [docker/cli docs](https://github.com/docker/cli/blob/master/docs/reference/commandline/login.md)
- `gh auth login --with-token` reads a PAT from stdin (`gh auth login --with-token < token.txt`), skipping the browser/device-code flow for CI; `GH_TOKEN`/`GITHUB_TOKEN` env vars are gh's other scriptable path (cli.github.com manual — general knowledge, not freshly re-fetched this session).
- `aws configure` is interactive-by-default (Access Key ID / Secret / region / format); its automation path is env vars (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`), not stdin. Notably `aws configure set key value` DOES accept secrets as a bare CLI arg — the one tool here that doesn't fully avoid argv exposure (docs.aws.amazon.com/cli — general knowledge, not freshly re-fetched).
- `kubectl` has no "login" verb; `kubectl config set-credentials --token=` also takes secrets as a flag. Its real automation story is exec-credential plugins that mint short-lived tokens dynamically, not a stored-secret convention (kubernetes.io/docs — general knowledge, not freshly re-fetched).
- Pattern: tools treating credentials as high-sensitivity (docker, gh) use stdin-piping; tools where the secret already lives in a semi-trusted local file/profile (aws, kubectl) are looser about argv. INV-1 puts openbox in the docker/gh camp.
- **Recommend:** interactive prompt by default (masked, via #1). For automation, an explicit `--<field>-stdin`-style flag naming a *source*, never a value — mirrors `docker login --password-stdin` exactly, the most security-conscious precedent found. Never `--api-key <value>`. Continue honoring the existing `OPENBOX_API_KEY`/`OPENBOX_ED25519_SEED`/`OPENBOX_AGENT_DID` env vars as the CI-native override (parallels gh's `GH_TOKEN`).

## 4. Prompt UX for optional/derivable fields
No single canonical spec; converging convention across CLI-auth writeups: show current/default in brackets, blank Enter = accept/derive, label optional fields explicitly, and gate any irreversible remote call behind its own explicit confirm step. [WorkOS: developer's guide to CLI authentication](https://workos.com/blog/cli-authentication-guide)
- **Recommend** plain `fmt.Fprintf` prompts:
  - `API key (obx_..., leave blank to keep current): ` (masked)
  - `Ed25519 seed (base64, leave blank to generate new): ` (masked)
  - `Agent DID (leave blank to derive from key): ` (not masked — DIDs aren't secret)
  - Then a plaintext summary + a distinct `Register/rotate agent now? [y/N]` gate, defaulting to **N**, before any backend call.

## 5. Idempotent re-run: safe display of stored credentials
Established convention: correlate/display via **last 4 chars only**, never the full secret — matches how Stripe/GitHub/AWS show existing keys in their own UIs. [treblle.com: API keys explained](https://treblle.com/blog/api-keys-explained)
- SSH/GPG-style colon-hex fingerprinting (`12:34:56:...`) is the parallel convention specifically for *keys* (vs. opaque tokens) — fits the Ed25519 material. [Oracle API signing-key docs use this exact format](https://docs.oracle.com/en-us/iaas/Content/API/Concepts/apisigningkey.htm)
- **Recommend**, on every re-run, before prompting, print current state without secrets:
  - `Current API key: obx_************************a91f (32 chars)`
  - `Current Ed25519 key: fingerprint SHA256:xN3f...` — fingerprint the **derived public key**, never the seed (fingerprinting the seed risks partial leakage of secret material).
  - `Current Agent DID: did:key:z6Mk...` (shown in full — not secret).

## 6. Testability
`term.ReadPassword(fd int)` takes a raw fd, not an `io.Reader` — it talks straight to the console, which is the actual seam problem (its signature, confirmed in #1). Wrap all prompting behind a small interface:
```
type Prompter interface {
    ReadLine(prompt string) (string, error)
    ReadSecret(prompt string) (string, error)
}
```
`realPrompter`: checks `term.IsTerminal`; if TTY, calls `term.ReadPassword(int(os.Stdin.Fd()))`; else falls back to `bufio.NewReader(stdin).ReadString('\n')` for the piped/automation path. `testPrompter`: backed by an injected `io.Reader`/`io.Writer` (`strings.Reader`/`bytes.Buffer`), no masking, fully unit-testable. Inject the `Prompter` into the command constructor rather than touching `os.Stdin` inside command logic — mirrors Cobra's own `cmd.SetIn`/`InOrStdin()` pattern, the idiomatic Go-CLI answer to this exact problem.

## Recommended approach
1. Add `golang.org/x/term` as cli's first external dependency — covers both `ReadPassword` and `IsTerminal` with one well-trusted, Go-team-owned module.
2. TTY-gate first: if not a terminal, fail fast with a message pointing at the automation flags/env vars, instead of hanging.
3. Automation escape hatch: `--<field>-stdin`-style flags (docker-login-shaped) plus the existing `OPENBOX_*` env vars — never a plaintext secret flag (preserves INV-1).
4. Prompt each field with `[current/blank=default]` shown; mask only seed/api-key; leave DID unmasked.
5. Show a non-secret summary (last-4 + length for tokens, fingerprint for keys, full DID) before writing, and again behind an explicit `y/N` gate before any register/rotate network call.
6. Build the command around an injectable `Prompter` interface from the start so prompting is unit-testable without a real TTY.

## Unresolved questions
- Exact wire format for the `--<field>-stdin` automation path — one field per invocation vs. JSON/multi-line for all three secrets at once. Design decision, not a research gap.
- Whether the Ed25519 fingerprint should hash the derived pubkey or something DID-format-specific — needs input from whoever owns this repo's DID/crypto format.
- Whether `openbox auth` should detect "nothing changed" and skip straight to a no-op confirmation on repeat runs, vs. always re-prompting all three fields — a UX call.
- gh/aws/kubectl specifics in §3 are stated from general knowledge of their documented behavior, not freshly re-fetched this session (tool-call budget spent on docker + x/term + TTY + display-convention searches, which returned strong multi-source confirmation). Worth a quick confirm-only pass if this drives the actual flag-naming decision.

Status: DONE
Summary: x/term (ReadPassword+IsTerminal) is the clear cross-platform-correct dependency; stdin-piping flags (docker/gh-style) preserve INV-1 for automation; last-4/fingerprint display plus an injectable Prompter interface cover safe re-run display and testability.
