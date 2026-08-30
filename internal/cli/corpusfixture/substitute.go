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
// carries whatever the developer's machine put in front of the model that day:
// another project's configuration, a file body, a colleague's name, none of
// which has a shape. The only reliable control is to never write recorded prose
// to disk.
//
// Values are spliced by BYTE OFFSET rather than replaced by value. A replace was
// tried and it corrupted the fixture on the first real body: a four-rune string
// leaf inside a tool input matched every `"text"` key and every `"type":"text"`
// discriminator, producing valid JSON of the right length that described an
// exchange no provider would send. Offsets cannot reach a key or a
// discriminator, so the collision class is gone rather than made unlikely.
func SubstitutePromptText(body string) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		return body
	}
	if _, ok := top["messages"]; !ok {
		return body
	}
	return spliceValues(body, promptTargets)
}

// SubstituteSSEDeltas does the same for a recorded event-stream response, where
// the model's reply arrives as deltas that the request-body rule never sees.
//
// Frame separators, event lines and every field but the text are untouched, so
// the per-chunk assertions built on this fixture still measure framing.
func SubstituteSSEDeltas(stream string) string {
	var out strings.Builder
	out.Grow(len(stream))
	for i, line := range strings.Split(stream, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		const prefix = "data: "
		if !strings.HasPrefix(line, prefix) {
			out.WriteString(line)
			continue
		}
		out.WriteString(prefix)
		out.WriteString(spliceValues(line[len(prefix):], deltaTargets))
	}
	return out.String()
}

// promptTargets reports whether a JSON path inside a request body addresses
// recorded free text.
//
// The path is the sequence of object keys walked to reach the value; array
// indices do not appear, because none of these rules depends on position.
func promptTargets(path []string) bool {
	if len(path) == 0 {
		return false
	}
	switch path[0] {
	case "messages":
		return inMessage(path[1:])
	case "system":
		return len(path) == 1 || (len(path) == 2 && path[1] == "text")
	case "tools":
		// A tool's prose is content: the same array carries the description of
		// every MCP server the developer has installed, written by whoever wrote
		// the server. Names and schema keys stay, so the request keeps its shape.
		return path[len(path)-1] == "description"
	}
	return false
}

// inMessage decides the part of a path below `messages`.
func inMessage(rest []string) bool {
	if len(rest) == 0 {
		return false
	}
	if rest[0] != "content" {
		return false
	}
	tail := rest[1:]
	if len(tail) == 0 {
		return true // a plain string on a user or system line
	}
	switch tail[0] {
	case "text", "thinking":
		return len(tail) == 1
	case "input", "content":
		// A tool-use input and a tool-result body are arbitrary JSON carrying
		// file contents, shell output and command lines, so every string under
		// them is content rather than structure.
		return true
	}
	return false
}

// deltaTargets reports whether a path inside one event-stream frame addresses
// the model's own text.
func deltaTargets(path []string) bool {
	if len(path) != 2 {
		return false
	}
	if path[0] != "delta" && path[0] != "content_block" {
		return false
	}
	switch path[1] {
	case "text", "thinking", "partial_json":
		return true
	}
	return false
}

// span is one string value's byte range in the source, quotes included.
type span struct{ start, end int }

// spliceValues rewrites every string value whose path satisfies want, leaving
// every other byte of the document exactly where it was.
func spliceValues(doc string, want func([]string) bool) string {
	spans := collectSpans(doc, want)
	if len(spans) == 0 {
		return doc
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start > spans[j].start })
	out := doc
	for _, s := range spans {
		tok := out[s.start:s.end]
		filler := fillerToken(tok)
		if filler == tok {
			continue
		}
		out = out[:s.start] + filler + out[s.end:]
	}
	return out
}

// fillerToken returns a quoted filler string occupying the same runes as tok.
func fillerToken(tok string) string {
	return `"` + SyntheticProse(utf8.RuneCountInString(tok)-2) + `"`
}

// collectSpans walks the document with a streaming decoder, which is what makes
// offsets available: InputOffset reports where the token just read ended.
//
// The offset before a token includes the separator and whitespace that preceded
// it, so the opening quote is found rather than assumed. Splicing from the
// reported offset ate the `:` before the value and produced JSON that no longer
// parsed.
func collectSpans(doc string, want func([]string) bool) []span {
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber()

	var (
		spans []span
		path  []string
		stack []frame
	)
	top := func() *frame {
		if len(stack) == 0 {
			return nil
		}
		return &stack[len(stack)-1]
	}
	for {
		start := dec.InputOffset()
		tok, err := dec.Token()
		if err != nil {
			return spans
		}
		end := dec.InputOffset()

		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, frame{object: true, expectKey: true})
			case '[':
				stack = append(stack, frame{})
			case '}', ']':
				f := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if f.pushed {
					path = path[:len(path)-1]
				}
				if p := top(); p != nil && p.object {
					p.expectKey = true
				}
			}
			continue
		}

		f := top()
		if f != nil && f.object && f.expectKey {
			key, _ := tok.(string)
			// One path element per object, replaced as the object's keys are
			// read: an array pushes nothing, so a path names the field a value
			// sits under rather than its position.
			if f.pushed {
				path = path[:len(path)-1]
			}
			path = append(path, key)
			f.pushed = true
			f.expectKey = false
			continue
		}
		if _, ok := tok.(string); ok && want(path) {
			if q := strings.IndexByte(doc[start:end], '"'); q >= 0 {
				spans = append(spans, span{int(start) + q, int(end)})
			}
		}
		if f != nil && f.object {
			f.expectKey = true
		}
	}
}

// frame is one open container. An object tracks which of its keys is on the path
// and whether the next token is a key or a value.
type frame struct {
	object    bool
	expectKey bool
	pushed    bool
}
