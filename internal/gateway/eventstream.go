package gateway

import (
	"encoding/json"
	"mime"
	"net/http"
	"slices"
	"strings"
)

// assembledMessage is a streamed reply rebuilt into the one message the stream
// described. A struct and not a map so the short, fixed fields come first and
// `content` comes last: a downstream head cut then takes the end of the reply
// text rather than the stop reason and usage.
type assembledMessage struct {
	ID           string          `json:"id,omitempty"`
	Type         string          `json:"type,omitempty"`
	Role         string          `json:"role,omitempty"`
	Model        string          `json:"model,omitempty"`
	StopReason   json.RawMessage `json:"stop_reason,omitempty"`
	StopSequence json.RawMessage `json:"stop_sequence,omitempty"`
	Usage        map[string]any  `json:"usage,omitempty"`
	Error        json.RawMessage `json:"error,omitempty"`
	Assembly     assemblyNote    `json:"openbox_assembly"`
	Content      []any           `json:"content"`
}

// assemblyNote says what reassembly did, so the rebuilt document never reads as
// the verbatim reply. Loss is acceptable; unreported loss is not -- the rule
// selectionNote follows on the request side.
type assemblyNote struct {
	Source string `json:"source"`
	Frames int    `json:"frames"`
	// Incomplete is true when no message_stop arrived: the stream was cut or
	// aborted, and the content is the prefix that did.
	Incomplete bool `json:"incomplete"`
	// SkippedEventTypes names the frame types that carried nothing into the
	// document (pings, and a surface's own events such as claude.ai's
	// conversation_ready), sorted, so a reader can see what was left out.
	SkippedEventTypes []string `json:"skipped_event_types,omitempty"`
}

// streamFrame is the part of one SSE data payload reassembly reads.
type streamFrame struct {
	Type         string          `json:"type"`
	Index        int             `json:"index"`
	Message      json.RawMessage `json:"message"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        json.RawMessage `json:"delta"`
	Usage        map[string]any  `json:"usage"`
	Error        json.RawMessage `json:"error"`
}

// assembleEventStream rebuilds an Anthropic-format event stream into one message
// document, and returns the body unchanged whenever it cannot.
//
// The raw stream stored 9-42 ping frames and one frame per token on every live
// claude.ai turn, so the Completed row held the reply at a few percent signal
// density, split across hundreds of JSON fragments. It also split any secret the
// model echoed across delta frames, where the keyword-driven redactor could not
// see it whole; rebuilding BEFORE redaction lets it.
//
// Tolerant by design, like the request selector: a body that is not
// text/event-stream, a marker, or a stream with no message_start (another
// provider's format) is returned as it came. Each data payload is parsed as JSON
// and one that does not parse is dropped, never regexed -- a truncation mark or a
// half frame must not become model output.
func assembleEventStream(body string, h http.Header) string {
	if body == "" || strings.HasPrefix(body, markerPrefix) || !isEventStream(h) {
		return body
	}
	body = strings.TrimSuffix(body, bodyCutNote)

	var (
		msg      assembledMessage
		started  bool
		blocks   = map[int]map[string]any{}
		toolJSON = map[int]*strings.Builder{}
		skipped  = map[string]bool{}
	)
	msg.Assembly.Source = "text/event-stream"
	msg.Assembly.Incomplete = true

	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data:")
		if !ok {
			continue
		}
		var f streamFrame
		if json.Unmarshal([]byte(strings.TrimSpace(payload)), &f) != nil || f.Type == "" {
			continue
		}
		msg.Assembly.Frames++
		switch f.Type {
		case "message_start":
			// A narrow struct, not &msg: the message object must not be able to
			// reach the assembly note or the content blocks. Its own content is
			// always empty; blocks arrive by index.
			var m struct {
				ID    string         `json:"id"`
				Type  string         `json:"type"`
				Role  string         `json:"role"`
				Model string         `json:"model"`
				Usage map[string]any `json:"usage"`
			}
			if json.Unmarshal(f.Message, &m) == nil {
				started = true
				msg.ID, msg.Type, msg.Role, msg.Model = m.ID, m.Type, m.Role, m.Model
				msg.Usage = m.Usage
			}
		case "content_block_start":
			block := map[string]any{}
			if json.Unmarshal(f.ContentBlock, &block) == nil {
				blocks[f.Index] = block
			}
		case "content_block_delta":
			applyDelta(blocks, toolJSON, f.Index, f.Delta)
		case "content_block_stop":
			finishToolInput(blocks, toolJSON, f.Index)
		case "message_delta":
			var d struct {
				StopReason   json.RawMessage `json:"stop_reason"`
				StopSequence json.RawMessage `json:"stop_sequence"`
			}
			if json.Unmarshal(f.Delta, &d) == nil {
				if len(d.StopReason) > 0 {
					msg.StopReason = d.StopReason
				}
				if len(d.StopSequence) > 0 {
					msg.StopSequence = d.StopSequence
				}
			}
			for k, v := range f.Usage {
				if msg.Usage == nil {
					msg.Usage = map[string]any{}
				}
				msg.Usage[k] = v
			}
		case "message_stop":
			msg.Assembly.Incomplete = false
		case "error":
			msg.Error = f.Error
		default:
			skipped[f.Type] = true
		}
	}
	if !started {
		return body
	}
	// A block still open when the stream ended keeps what arrived of its input.
	for i := range toolJSON {
		finishToolInput(blocks, toolJSON, i)
	}

	indexes := make([]int, 0, len(blocks))
	for i := range blocks {
		indexes = append(indexes, i)
	}
	slices.Sort(indexes)
	msg.Content = make([]any, 0, len(indexes))
	for _, i := range indexes {
		msg.Content = append(msg.Content, blocks[i])
	}
	for t := range skipped {
		msg.Assembly.SkippedEventTypes = append(msg.Assembly.SkippedEventTypes, t)
	}
	slices.Sort(msg.Assembly.SkippedEventTypes)

	out, err := marshalNoHTMLEscape(msg)
	if err != nil {
		return body
	}
	return string(out)
}

// applyDelta folds one content_block_delta into its block. Text and thinking
// append to the block's own field; tool input arrives as partial JSON and is
// parsed once the block stops. A signature is opaque verification material with
// no reading value, so it is not accumulated.
func applyDelta(blocks map[int]map[string]any, toolJSON map[int]*strings.Builder, index int, raw json.RawMessage) {
	var d struct {
		Type        string          `json:"type"`
		Text        string          `json:"text"`
		Thinking    string          `json:"thinking"`
		PartialJSON string          `json:"partial_json"`
		Citation    json.RawMessage `json:"citation"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return
	}
	block := blocks[index]
	if block == nil {
		// A delta for a block whose start never arrived (a cut stream's head).
		block = map[string]any{}
		blocks[index] = block
	}
	switch d.Type {
	case "text_delta":
		block["text"] = stringField(block, "text") + d.Text
	case "thinking_delta":
		block["thinking"] = stringField(block, "thinking") + d.Thinking
	case "input_json_delta":
		b := toolJSON[index]
		if b == nil {
			b = &strings.Builder{}
			toolJSON[index] = b
		}
		b.WriteString(d.PartialJSON)
	case "citations_delta":
		if len(d.Citation) > 0 {
			list, _ := block["citations"].([]any)
			block["citations"] = append(list, d.Citation)
		}
	}
}

// finishToolInput parses a block's accumulated tool input. Input that does not
// parse -- a stream cut mid-argument -- is kept as the string that arrived.
func finishToolInput(blocks map[int]map[string]any, toolJSON map[int]*strings.Builder, index int) {
	b := toolJSON[index]
	if b == nil {
		return
	}
	delete(toolJSON, index)
	block := blocks[index]
	if block == nil || b.Len() == 0 {
		return
	}
	var input json.RawMessage
	if json.Unmarshal([]byte(b.String()), &input) == nil {
		block["input"] = input
		return
	}
	block["input"] = b.String()
}

func stringField(block map[string]any, key string) string {
	s, _ := block[key].(string)
	return s
}

func isEventStream(h http.Header) bool {
	mediaType, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "text/event-stream")
}
