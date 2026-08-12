# Phase 04 — Prompter: TTY detection, masked input, `--*-stdin`

## Context links

- Parent: [plan.md](plan.md) · Depends on: phase 1 (`x/term` pinned)
- Blocks: phase 5 · Parallel-safe with: phases 2, 3
- Research: `research/researcher-02-interactive-auth-ux-report.md` (still valid)

## Overview

- **Date:** 2026-08-12
- **Description:** A small `Prompter` package: masked and plain line input, TTY
  detection, and a stdin path for automation. No TUI framework.
- **Priority:** P1 · **Implementation status:** implemented 2026-08-13 · **Review status:** self-reviewed

## Key Insights

- `golang.org/x/term` is the only option that works on **native Windows** —
  its Windows build drives `GetConsoleMode`/`SetConsoleMode` directly. `stty -echo`
  is unix-only (no `stty.exe` outside Git Bash/WSL), so it is ruled out.
- **Use `term.IsTerminal`, not `os.Stdin.Stat()`.** On Windows, console handles set
  `ModeCharDevice` but **not** `ModeDevice` ([golang/go#23123]), so the stdlib mode
  check silently misbehaves there.
- `term.ReadPassword(fd int)` takes a raw fd, not an `io.Reader` — **that is the
  testability problem.** Wrap it behind an interface and inject, mirroring Cobra's
  `SetIn`/`InOrStdin` pattern.
- INV-1 makes the prompt the *correct* design, not a compromise: a masked prompt
  keeps secrets off argv, which a `--api-key <value>` flag would violate.
  For automation, name a **source** not a value — `docker login --password-stdin`
  is the precedent.
- Masking here defends against terminal scrollback, screen sharing and tmux
  buffers. It does **not** protect the secret at rest — phase 1's ADR says so.

## Requirements

1. `cli/internal/prompt` exposing:
   ```go
   type Prompter interface {
       Line(prompt, current string) (string, error)   // blank ⇒ keep current
       Secret(prompt string, hasCurrent bool) (string, error) // blank ⇒ keep current
       Confirm(prompt string, defaultYes bool) (bool, error)
       Printf(format string, a ...any)                 // to the writer, never stdout-for-hooks
   }
   ```
2. `New(stdin *os.File, out io.Writer) Prompter` — masks when
   `term.IsTerminal(int(stdin.Fd()))`, otherwise reads plain lines via `bufio`.
3. **Fail fast, never hang:** when stdin is not a terminal AND no `--*-stdin` flag
   was given, return an error naming the automation flags and the `OPENBOX_*`
   env vars instead of blocking.
4. Test prompter backed by `strings.Reader` + `bytes.Buffer`; no real TTY needed.
5. Never echo a secret to the writer — not on read-back, not in errors.
6. Trim trailing `\r` from every line read (Windows / piped CRLF input).

## Architecture

Two implementations of one interface. `realPrompter` holds `*os.File` + writer and
decides masking per call from `IsTerminal`. `testPrompter` reads scripted lines.
Phase 5 takes a `Prompter` in its command constructor and never touches
`os.Stdin` directly — that is the whole point of the seam.

## Related code files

| Path | Why |
|---|---|
| `cli/internal/prompt/prompt.go` | new — interface + real impl |
| `cli/internal/prompt/prompt_test.go` | new — table-driven, no TTY |
| `cli/cmd/openbox/main.go:40-70` | `app` struct already carries stdin/stdout/stderr; wire the Prompter the same way |

## Implementation Steps

1. Define the interface and the test implementation first; write the tests before
   the real impl so the seam is proven independent of a TTY.
2. `realPrompter.Secret`: `IsTerminal` ⇒ `term.ReadPassword` + explicit newline to
   the writer (ReadPassword eats the echo of Enter); else `bufio` line.
3. `Line` shows `[current]` when a current value exists; blank input keeps it.
4. `Confirm` defaults to **No** for anything irreversible.
5. Fail-fast path for non-TTY + no stdin flag, with the exact remediation text.
6. CRLF trimming on every read; test with `\r\n` input.

## Todo list

- [x] Interface + `testPrompter` + tests, no TTY required
- [x] `realPrompter` masking via `IsTerminal`/`ReadPassword`
- [x] `[current]` display; blank keeps current
- [x] `Confirm` defaults No
- [x] Non-TTY fail-fast with remediation text
- [x] CRLF trimmed; no secret ever written to the writer

## Success Criteria

- `go test ./cli/internal/prompt/...` green with no TTY.
- Piping input with no `--*-stdin` flag errors immediately rather than hanging.
- `\r\n`-terminated piped input yields values with no trailing `\r`.
- Grep confirms no `fmt.Print` of a secret anywhere in the package.

## Risk Assessment

| Risk | L×I | Observable signal it broke | Pre-decided response |
|---|---|---|---|
| `ReadPassword` misbehaves in a non-standard terminal (IDE console, mintty) | M×M | no prompt shown, or input echoed | **Accepted, mitigated:** `IsTerminal` gates it; a non-TTY falls back to plain read. Document that IDE consoles may not mask. |
| Fail-fast text sends users to a flag that phase 5 renames | M×L | error names a flag that does not exist | **Adjust:** phase 5 owns flag names; keep the message in one constant and have phase 5 update it. |
| `x/term` unavailable for a target | L×H | build fails for GOOS/GOARCH | **Stop and replan:** x/term supports darwin/linux/windows; a failure means the target is outside plan scope. |
| Masking gives false reassurance | M×M | reviewer assumes the secret is protected | **Adjust:** the package doc comment states it defends scrollback only, pointing at ADR-0015. |

## Security Considerations

Masked input keeps the secret out of terminal scrollback, screen shares and
recorded sessions, and off argv (INV-1). It does not protect at rest — the value
goes to a plaintext file by design. Say that in the package doc so nobody infers
more. Errors must never quote input.

## Next steps

Phase 5 consumes `Prompter`.
