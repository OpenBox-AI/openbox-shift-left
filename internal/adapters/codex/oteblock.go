package codex

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// otelEnvironmentMarker identifies an [otel] table this installer owns.
// Codex's own `environment` field is free text (the probe recorded
// "openbox-probe"), so pinning it to this exact value is a PARSED ownership
// check -- decoding the table and comparing one field -- never a substring
// scan of the raw file, the same rule writeHooks holds for hooks.json.
const otelEnvironmentMarker = "openbox-shift-left"

// otelExporterProtocol matches the shape Codex itself wrote when probed:
// OTLP/HTTP, protocol "binary", pointed at a loopback receiver.
const otelExporterProtocol = "binary"

// otelLogUserPrompt is pinned false: the lane's content posture is decided by
// the single content_capture gate downstream, never a second Codex-specific
// switch that could let a prompt egress this surface uninspected.
const otelLogUserPrompt = false

// otelBeginMarker and otelEndMarker bracket the exact byte range this
// installer ever writes into config.toml. TOML comments are invisible to
// toml.Unmarshal, so they cost nothing in the parsed ownership check
// (isOpenBoxOtel still looks only at the `environment` field), but they let
// WriteOtel/RemoveOtel locate and splice their own region by text instead of
// decoding the whole document into a map and re-encoding it canonically --
// which is what used to silently discard every comment and key ordering a
// developer had in the rest of config.toml. Whatever is outside this range
// never gets rebuilt, so it survives byte-for-byte.
const (
	otelBeginMarker = "# openbox-shift-left: managed [otel] block below -- `openbox init --provider codex` rewrites it, `openbox uninstall` removes it"
	otelEndMarker   = "# openbox-shift-left: end managed [otel] block"
)

// ConfigTOMLPath is ~/.codex/config.toml, honouring CODEX_HOME the same way
// defaultHooksPath does. Exported so an uninstall or doctor can look where
// the install wrote without duplicating the resolution.
func ConfigTOMLPath() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return filepath.Join(h, "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".codex", "config.toml")
}

// WriteOtel merges an OpenBox-owned [otel] block into config.toml, pointing
// Codex's own OTLP/HTTP exporter at the loopback telemetry lane. Ownership-
// aware: an existing [otel] table without the OpenBox marker is foreign, and
// this refuses to overwrite it -- exactly as writeHooks refuses an
// unparsable hooks.json rather than guess which entries are ours.
//
// Everything outside OpenBox's own marker-delimited region is spliced
// through untouched -- comments, blank-line grouping, foreign key order --
// rather than decoded into a map and re-encoded canonically. Re-running with
// the same endpoint reproduces the exact same region, so the whole file is
// byte-idempotent.
func WriteOtel(path, endpoint string) error {
	doc, err := readTOMLDoc(path)
	if err != nil {
		return err
	}
	if existing, ok := doc["otel"]; ok && !isOpenBoxOtel(existing) {
		return fmt.Errorf("codex install: %s already has a foreign [otel] block; refusing to overwrite it", path)
	}
	blockText, err := renderOtelBlock(endpoint)
	if err != nil {
		return err
	}
	raw, err := readRawFile(path)
	if err != nil {
		return err
	}
	return commitTOMLFile(path, mergeOwnedBlock(raw, blockText))
}

// otelExporterTopKey is the exporter table's key, named once so the writer
// and the ownership check can never spell it two different ways.
const otelExporterTopKey = "exporter"

func otelBlockFor(endpoint string) map[string]any {
	return map[string]any{
		"otlp-http": map[string]any{
			"endpoint": endpoint,
			"protocol": otelExporterProtocol,
		},
	}
}

// renderOtelBlock marshals ONLY the [otel] table OpenBox owns, never the
// surrounding document -- the piece mergeOwnedBlock splices in.
func renderOtelBlock(endpoint string) ([]byte, error) {
	doc := map[string]any{
		"otel": map[string]any{
			"environment":      otelEnvironmentMarker,
			"log_user_prompt":  otelLogUserPrompt,
			otelExporterTopKey: otelBlockFor(endpoint),
		},
	}
	out, err := toml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("codex install: marshal otel block: %w", err)
	}
	return out, nil
}

// RemoveOtel removes only an OpenBox-owned [otel] block, leaving a foreign
// one (or none at all) untouched. removed reports whether anything was
// actually deleted, so uninstall can report truthfully instead of claiming a
// removal that never happened. Like WriteOtel, this splices the owned region
// out of the raw bytes rather than decoding and re-encoding the document, so
// a write-then-remove round-trips to the original bytes exactly.
func RemoveOtel(path string) (removed bool, err error) {
	doc, err := readTOMLDoc(path)
	if err != nil {
		return false, err
	}
	existing, ok := doc["otel"]
	if !ok || !isOpenBoxOtel(existing) {
		return false, nil
	}
	raw, err := readRawFile(path)
	if err != nil {
		return false, err
	}
	out, found := removeOwnedBlock(raw)
	if !found {
		// The parsed document says this [otel] table is ours, but the
		// marker-delimited region that WriteOtel always wraps it in is
		// missing -- the file was hand-edited to strip the comments while
		// keeping the environment field. Refuse rather than guess a byte
		// range to delete; the same rule an unparsable file gets above.
		return false, fmt.Errorf("codex uninstall: %s's [otel] block is owned but its managed-region markers are missing; refusing to guess what to delete", path)
	}
	if err := commitTOMLFile(path, out); err != nil {
		return false, err
	}
	return true, nil
}

// HasOwnedOtel reports whether config.toml carries an OpenBox-owned [otel]
// block, for doctor and uninstall's inventory to check without writing
// anything. A read failure (unparsable file, permissions) reads as "no",
// which is the fail-open direction a reporting-only check must take.
func HasOwnedOtel(path string) bool {
	doc, err := readTOMLDoc(path)
	if err != nil {
		return false
	}
	existing, ok := doc["otel"]
	return ok && isOpenBoxOtel(existing)
}

// isOpenBoxOtel is the one parsed ownership check both the writer and the
// remover use, so they can never disagree about what counts as "ours".
func isOpenBoxOtel(v any) bool {
	table, ok := v.(map[string]any)
	if !ok {
		return false
	}
	env, _ := table["environment"].(string)
	return env == otelEnvironmentMarker
}

// readRawFile is os.ReadFile with the same absent-file tolerance readTOMLDoc
// gives a parsed document: nil, nil rather than an error, so a caller
// splicing bytes never has to special-case "config.toml does not exist yet".
func readRawFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("codex install: read %s: %w", path, err)
	}
	return raw, nil
}

// decodeTOMLDoc parses raw into a plain map for the ownership check ONLY --
// never re-marshaled back over the file, which is what used to discard every
// comment and key ordering. A pre-existing file this parser cannot read is a
// hard error -- never clobber a file we cannot understand, the same rule
// writeHooks holds for hooks.json -- and an empty/absent document reads as an
// empty map rather than an error, matching Codex's own tolerance for a
// config.toml that does not exist yet.
func decodeTOMLDoc(raw []byte, path string) (map[string]any, error) {
	doc := map[string]any{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return doc, nil
	}
	if err := toml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("codex install: refusing to modify unparsable %s: %w", path, err)
	}
	return doc, nil
}

// readTOMLDoc reads and parses path in one step, for the callers that only
// need the parsed ownership view.
func readTOMLDoc(path string) (map[string]any, error) {
	raw, err := readRawFile(path)
	if err != nil {
		return nil, err
	}
	return decodeTOMLDoc(raw, path)
}

// ownedBlockSpan locates the byte range in raw occupied by OpenBox's whole
// managed region: the blank separator line immediately before the begin
// marker (when mergeOwnedBlock added one), the begin marker, everything
// through the end marker, and its own trailing newline. This is exactly, and
// only, what mergeOwnedBlock ever inserts, which is what makes
// removeOwnedBlock's deletion of it an exact inverse.
func ownedBlockSpan(raw []byte) (start, end int, ok bool) {
	beginBytes := []byte(otelBeginMarker)
	beginIdx := bytes.Index(raw, beginBytes)
	if beginIdx < 0 {
		return 0, 0, false
	}
	endBytes := []byte(otelEndMarker)
	endIdx := bytes.Index(raw[beginIdx:], endBytes)
	if endIdx < 0 {
		return 0, 0, false
	}
	end = beginIdx + endIdx + len(endBytes)
	if end < len(raw) && raw[end] == '\n' {
		end++
	}
	start = beginIdx
	// A blank separator line immediately precedes the marker when there was
	// content before it: "...<content>\n" + "\n" (the blank line) + marker.
	// Both newlines land right before start, so folding one of them into the
	// span is what makes it exactly what was inserted.
	if start >= 2 && raw[start-1] == '\n' && raw[start-2] == '\n' {
		start--
	}
	return start, end, true
}

// ownedRegion renders the bytes mergeOwnedBlock ever inserts for one write:
// an optional leading blank line (when something precedes it), the begin
// marker, the block itself, the end marker, each on its own line.
func ownedRegion(precededByContent bool, blockText []byte) []byte {
	var buf bytes.Buffer
	if precededByContent {
		buf.WriteByte('\n')
	}
	buf.WriteString(otelBeginMarker)
	buf.WriteByte('\n')
	buf.Write(blockText)
	if len(blockText) == 0 || blockText[len(blockText)-1] != '\n' {
		buf.WriteByte('\n')
	}
	buf.WriteString(otelEndMarker)
	buf.WriteByte('\n')
	return buf.Bytes()
}

// mergeOwnedBlock returns raw with OpenBox's managed region replaced by a
// freshly rendered one (when an owned region already exists) or appended at
// the end (when it does not). Every byte outside that region -- including
// comments and key order the whole-document decode/re-encode used to lose --
// is copied through unchanged.
func mergeOwnedBlock(raw, blockText []byte) []byte {
	if start, end, ok := ownedBlockSpan(raw); ok {
		var buf bytes.Buffer
		buf.Write(raw[:start])
		buf.Write(ownedRegion(start > 0, blockText))
		buf.Write(raw[end:])
		return buf.Bytes()
	}
	var buf bytes.Buffer
	buf.Write(raw)
	if buf.Len() > 0 {
		if b := buf.Bytes(); b[len(b)-1] != '\n' {
			buf.WriteByte('\n')
		}
	}
	buf.Write(ownedRegion(buf.Len() > 0, blockText))
	return buf.Bytes()
}

// removeOwnedBlock deletes OpenBox's managed region from raw, reporting
// whether one was found. Because ownedBlockSpan captures exactly what
// mergeOwnedBlock inserts (including the one blank separator line it adds),
// deleting that span is an exact inverse: raw[:start]+raw[end:] reproduces
// whatever preceded the region byte-for-byte.
func removeOwnedBlock(raw []byte) (out []byte, found bool) {
	start, end, ok := ownedBlockSpan(raw)
	if !ok {
		return raw, false
	}
	out = make([]byte, 0, len(raw)-(end-start))
	out = append(out, raw[:start]...)
	out = append(out, raw[end:]...)
	return out, true
}

// commitTOMLFile writes content the way writeHooksFile does: 0600, one
// trailing newline, atomic rename via hookflow's writer, so a partially
// written config.toml is never a state a crash can leave behind.
func commitTOMLFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("codex install: config dir: %w", err)
	}
	return writeHooksFile(path, content, "codex install: commit "+path)
}
