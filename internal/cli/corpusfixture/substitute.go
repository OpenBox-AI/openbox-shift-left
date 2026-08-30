package corpusfixture

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"
)

// SubstitutePromptText replaces every recorded free-text leaf in a model-call
// request body with filler of the same rune length, and returns any other
// document unchanged.
//
// It exists because no scanner can recognize private content. A recorded prompt
// carries whatever the developer's machine put in front of the model that day —
// another project's configuration, a file body, a colleague's name — and none of
// that has a shape. The only reliable control is to never write recorded prose to
// disk, which is what this does.
//
// The body is patched as raw token bytes rather than decoded and re-encoded: a
// re-encode reorders every key and rewrites every escape, so the committed
// fixture would stop resembling the request a provider SDK actually sends, and
// the diff of a future regeneration would be unreadable.
func SubstitutePromptText(body string) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		return body
	}
	if _, ok := top["messages"]; !ok {
		return body
	}

	tokens := map[string]bool{}
	collectMessages(top["messages"], tokens)
	collectSystem(top["system"], tokens)
	collectToolDescriptions(top["tools"], tokens)

	ordered := make([]string, 0, len(tokens))
	for tok := range tokens {
		ordered = append(ordered, tok)
	}
	// Longest first, so a short token that happens to be a substring of a longer
	// one cannot rewrite the middle of it.
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })

	for _, tok := range ordered {
		body = strings.ReplaceAll(body, tok, fillerToken(tok))
	}
	return body
}

// fillerToken returns a quoted filler string occupying the same runes as tok.
func fillerToken(tok string) string {
	return `"` + SyntheticProse(utf8.RuneCountInString(tok)-2) + `"`
}

func collectMessages(raw json.RawMessage, out map[string]bool) {
	var msgs []json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return
	}
	for _, m := range msgs {
		var msg map[string]json.RawMessage
		if json.Unmarshal(m, &msg) != nil {
			continue
		}
		collectContent(msg["content"], out)
	}
}

// collectContent handles both shapes a content field takes: a plain string on a
// user or system line, an array of typed blocks on an assistant one.
func collectContent(raw json.RawMessage, out map[string]bool) {
	if addIfString(raw, out) {
		return
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		var blk map[string]json.RawMessage
		if json.Unmarshal(b, &blk) != nil {
			continue
		}
		addIfString(blk["text"], out)
		addIfString(blk["thinking"], out)
		// A tool-use input and a tool-result body are arbitrary JSON carrying
		// file contents, shell output and command lines, so every string under
		// them is content rather than structure.
		addStringLeaves(blk["input"], out)
		if _, ok := blk["tool_use_id"]; ok {
			addStringLeaves(blk["content"], out)
		}
	}
}

func collectSystem(raw json.RawMessage, out map[string]bool) {
	if addIfString(raw, out) {
		return
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		var blk map[string]json.RawMessage
		if json.Unmarshal(b, &blk) != nil {
			continue
		}
		addIfString(blk["text"], out)
	}
}

// collectToolDescriptions treats a tool's prose as content while leaving its
// name, types and schema keys alone.
//
// The provider's own tool documentation is public, but the same array carries
// the description of every MCP server the developer has installed, and those are
// written by whoever wrote the server. Keeping the names and the schema is what
// leaves the request recognizably shaped.
func collectToolDescriptions(raw json.RawMessage, out map[string]bool) {
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return
	}
	for _, t := range tools {
		collectDescriptions(t, out)
	}
}

func collectDescriptions(raw json.RawMessage, out map[string]bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for k, v := range obj {
			if k == "description" {
				addIfString(v, out)
				continue
			}
			collectDescriptions(v, out)
		}
		return
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		for _, v := range arr {
			collectDescriptions(v, out)
		}
	}
}

func addIfString(raw json.RawMessage, out map[string]bool) bool {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return false
	}
	if tok := string(raw); tok != fillerToken(tok) {
		out[tok] = true
	}
	return true
}

func addStringLeaves(raw json.RawMessage, out map[string]bool) {
	if addIfString(raw, out) {
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for _, v := range obj {
			addStringLeaves(v, out)
		}
		return
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		for _, v := range arr {
			addStringLeaves(v, out)
		}
	}
}
