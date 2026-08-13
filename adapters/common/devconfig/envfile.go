package devconfig

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// envfile.go — the dotenv codec for ~/.openbox/.env (ADR-0015).
//
// Hand-rolled rather than a dependency: this repo writes and reads both ends of
// the format, and the whole thing is the file you are reading. ADR-0015 records
// that as a decision, so a later "just add godotenv" is a reversal, not a
// cleanup.
//
// The codec is deliberately a plain map[string]string with no knowledge of which
// keys are credentials. Precedence — real env var beats file — belongs to
// ResolveCredentials, and restricting the written key set to secrets belongs to
// `openbox auth`. Keeping policy out of the codec is what makes it testable
// without touching the environment.

// envFileHeader is written above the keys on every WriteEnvFile.
//
// It is a security control, not decoration. This file is the ONLY copy of
// credentials the backend shows exactly once, and it is plaintext — a human who
// opens it should learn both facts here rather than from an ADR they will not
// read.
const envFileHeader = `# OpenBox credentials for this machine — written by ` + "`openbox auth`" + `.
#
# PLAINTEXT. 0600 on macOS/Linux; on Windows 0600 is a no-op and other local
# accounts can read this file (ADR-0015). Anything running as you can read the
# signing key below and sign governance events as this agent.
#
# This is the ONLY copy. OpenBox shows the API key and signing key once, at
# registration, and does not store them. If you lose this file, re-issue with
# ` + "`openbox auth --rotate`" + ` or register again with ` + "`openbox auth`" + `.
#
# DO NOT COMMIT THIS FILE. Sourcing it is never required — the tools read it
# directly. A real environment variable always wins over a value here.
`

// ParseEnvFile reads a dotenv file into a map.
//
// A missing file is an empty map and a nil error: no credentials configured is a
// legitimate state that the caller reports in its own words, not an I/O failure.
//
// Supported syntax, which is the intersection of what people actually type and
// what can be parsed unambiguously:
//
//	# comment                       (and trailing blank lines)
//	KEY=value
//	export KEY=value                (pasted straight from a shell)
//	KEY="value"  /  KEY='value'     (quotes stripped, one level)
//	  KEY = value                   (whitespace around key and value)
//	KEY=                            (explicit empty)
//
// A duplicate key is an ERROR, not last-wins. Two lines setting one credential
// means the user believes something the file does not say, and silently picking
// one is how a rotated key gets shadowed by the line above it — a failure that
// surfaces as an unexplained 401 much later.
func ParseEnvFile(path string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// A pasted private key is short, but a generous ceiling costs nothing and
	// avoids a truncation that would look like a corrupt credential.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		// CRLF: a Windows user editing this in Notepad produces \r\n. A naive
		// parser leaves \r on every value, and a \r inside a base64 signing key
		// fails signature verification with an error that names neither the
		// file nor the character. Strip it here so it cannot reach a signer.
		raw := strings.TrimRight(sc.Text(), "\r")
		s := strings.TrimSpace(raw)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")

		// Split on the FIRST '=' only. Base64 values are padded with '=', so
		// splitting on all of them truncates a 32-byte key to nothing.
		eq := strings.Index(s, "=")
		if eq < 0 {
			return nil, fmt.Errorf("%s:%d: not a KEY=value line", path, line)
		}
		key := strings.TrimSpace(s[:eq])
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, line)
		}
		if _, dup := out[key]; dup {
			// Name the key and the line, never the value.
			return nil, fmt.Errorf("%s:%d: duplicate key %s — remove one of the two lines "+
				"(a second assignment silently shadows the first, which is how a rotated credential stops working)", path, line, key)
		}
		out[key] = unquote(strings.TrimSpace(s[eq+1:]))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}

// unquote strips one level of matching surrounding quotes. Unbalanced quotes are
// left alone: a value that merely contains a quote is data, not a syntax error.
func unquote(v string) string {
	if len(v) < 2 {
		return v
	}
	if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}

// WriteEnvFile merges kv over whatever is already at path and writes the result
// atomically, 0600, under a 0700 parent.
//
// Merge, not replace, and that matters more under the secrets-only split than it
// would otherwise (ADR-0015). `openbox auth` writes exactly three keys, so any
// other key in the file was put there by a human — someone who hand-added
// OPENBOX_AGENT_DID to override a coordinate has authored a key this writer must
// keep, even though nothing here will ever write it. Dropping it would silently
// undo a deliberate override.
//
// Comments are not preserved: the header is rewritten from the constant above
// and a user's own comments are lost. That is the documented limit of "preserve
// unknown keys" — keys survive, annotations do not.
func WriteEnvFile(path string, kv map[string]string) error {
	existing, err := ParseEnvFile(path)
	if err != nil {
		// A file we cannot parse must not be silently overwritten — it holds
		// the only copy of credentials. Surface it so the user fixes or moves
		// it deliberately.
		return fmt.Errorf("refusing to overwrite %s: %w", path, err)
	}
	merged := make(map[string]string, len(existing)+len(kv))
	for k, v := range existing {
		merged[k] = v
	}
	for k, v := range kv {
		merged[k] = v
	}

	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	// Stable order so a rewrite that changes one value produces a one-line diff.
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(envFileHeader)
	for _, k := range keys {
		// Single quotes: no shell expansion, and a credential never contains one.
		//
		// A value that DOES contain a single quote is refused rather than escaped.
		// The earlier version wrote shell-style `'"'"'`, which this parser — which
		// strips one outer quote pair and unescapes nothing — read back verbatim,
		// silently corrupting the value with no error. Worse, every subsequent write
		// re-serializes EVERY key, so one hand-added apostrophe would be mangled
		// further on each `openbox auth` run.
		//
		// Refusing is the right trade for a file that holds the only copy of
		// credentials: neither of the two secrets can contain a quote, so this only
		// ever fires on a key a human added, and a clear error beats silent
		// corruption. Supporting it properly means an escaping scheme on both sides,
		// which is more machinery than the format needs.
		if strings.ContainsAny(merged[k], "'\n\r") {
			return fmt.Errorf("cannot write %s to %s: its value contains a quote or newline, which this "+
				"format cannot represent without corrupting it. Remove or rewrite that value by hand", k, path)
		}
		fmt.Fprintf(&b, "%s='%s'\n", k, merged[k])
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	// Atomic: a concurrent reader (a hook firing mid-write) sees either the old
	// file or the new one, never a truncated credential. Same temp+rename shape
	// the deleted file secret backend used, which was proven in production.
	tmp, err := os.CreateTemp(dir, ".env-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	// Chmod BEFORE writing, so the secret never exists at a wider mode, not
	// even for the microseconds between write and chmod.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp credential file: %w", err)
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp credential file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp credential file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit %s: %w", path, err)
	}
	return nil
}
