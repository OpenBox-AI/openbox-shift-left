package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// minSSEFrames is how many event-stream frames the streaming fixture must carry.
//
// Per-chunk delivery cannot be demonstrated with fewer than two, and the third is
// margin: the smallest recorded exchange is chosen deliberately, and the very
// smallest streams are truncated or aborted ones whose framing proves nothing.
const minSSEFrames = 3

// redactedMarker is this repository's own enforce-path redactor, visible in the
// recording.
//
// The corpus was recorded during governed sessions in this very repo, so a
// request body can arrive already rewritten — an API-key literal replaced by
// ${OPENBOX_REDACTED_GENERIC_API_KEY} before it ever left the machine. Such an
// exchange is skipped rather than committed: a fixture carrying one still parses
// and still replays, and every assertion built on it would be a statement about
// that accident instead of about the product.
//
// It is worth knowing WHY this was invisible until now. While the fixture carried
// the response compressed, the marker sat inside gzip and matched nothing — the
// same reason this repository already documents for a content-encoded body
// defeating its own detector. Decompressing made it visible on the first run.
const redactedMarker = "${OPENBOX_REDACTED"

// extractProxy pulls two model-call exchanges out of the recorded proxy stream:
// one with a JSON response and one with an SSE response.
//
// Two honest limits, both recorded in the fixture itself rather than left for a
// reader to discover.
//
// The recorder flattened the response into ONE body, so real chunk boundaries do
// not exist in the corpus. A replay reconstructs them from SSE frame separators,
// which is what a client actually sees but is not what the wire did.
//
// And the smallest available request is chosen deliberately: 96.75% of recorded
// model-call request bodies exceed the 65,536-rune egress cap (p50 ~529k runes),
// so the median request is far too large to commit. Byte-identity is a property
// of the relay, not of the payload size, so a small real request proves it; the
// cap is exercised by a synthetic oversized fixture instead.
func extractProxy(corpus, out string) error {
	f, err := os.Open(filepath.Join(corpus, "proxy", "events.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()

	type exchange struct {
		req, resp *event
	}
	pending := map[string]*event{}
	var jsonPick, ssePick exchange
	var rewritten int

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), "/v1/messages") && !strings.Contains(string(line), "proxy.response") {
			continue
		}
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Kind {
		case "proxy.request":
			if strings.Contains(ev.URL, "/v1/messages") && !strings.Contains(ev.URL, "count_tokens") {
				cp := ev
				pending[ev.FlowID] = &cp
			}
		case "proxy.response":
			req, ok := pending[ev.FlowID]
			if !ok {
				continue
			}
			delete(pending, ev.FlowID)
			cp := ev
			ex := exchange{req: req, resp: &cp}
			// A brotli body is skipped rather than carried. The standard library
			// cannot decompress it, and taking a brotli dependency to widen a
			// fixture set would be a dependency decision made by a test-data
			// script. gzip covers every recorded event-stream response and the
			// uncompressed ones cover JSON; measured on the corpus: gzip 3569
			// (all event-stream), br 1573 (all JSON), none 5.
			body, ok := decodedBody(&cp)
			if !ok {
				continue
			}
			if strings.Contains(body, redactedMarker) || strings.Contains(req.bodyText(), redactedMarker) {
				rewritten++
				continue
			}
			cp.decoded = body
			if strings.Contains(strings.ToLower(ev.Headers["content-type"]), "event-stream") {
				if strings.Count(body, "\n\n") < minSSEFrames-1 {
					continue
				}
				ssePick = smaller(ssePick, ex)
			} else if strings.Contains(strings.ToLower(ev.Headers["content-type"]), "json") {
				jsonPick = smaller(jsonPick, ex)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}

	if rewritten > 0 {
		fmt.Printf("skipped %d exchange(s) already rewritten by this repo's own redactor\n", rewritten)
	}

	for name, ex := range map[string]exchange{
		"messages-json.json": jsonPick,
		"messages-sse.json":  ssePick,
	} {
		if ex.req == nil || ex.resp == nil {
			return fmt.Errorf("no %s exchange found under %s", name, corpus)
		}
		doc, err := fixtureFor(ex.req, ex.resp)
		if err != nil {
			return err
		}
		if err := write(filepath.Join(out, "transport", "testdata", "corpus", name), doc); err != nil {
			return err
		}
	}
	return nil
}

// smaller keeps the smallest request body seen, so the committed fixture is a
// real exchange rather than a truncated one.
func smaller(cur, next struct {
	req, resp *event
}) struct {
	req, resp *event
} {
	if cur.req == nil {
		return next
	}
	if len(next.req.bodyText()) < len(cur.req.bodyText()) {
		return next
	}
	return cur
}

// decodedBody returns the body as the wire semantically carried it, and reports
// whether this exchange is usable at all.
//
// The recorder stored the response exactly as it arrived, which means COMPRESSED:
// every recorded event-stream response is gzip. Committing those bytes produces a
// fixture with no visible frame boundaries, on which a streaming assertion is
// vacuous, and on which the capture assertions measure nothing — this repository
// already documents that a content-encoded body defeats its own detector and is
// marked rather than scanned.
//
// So the fixture carries the DECOMPRESSED body and drops the content-encoding
// header with it, which is stated in the fixture's own note. What that costs is
// the ability to replay a compressed response; nothing in this phase needs one,
// and the gateway's content-encoding marker has its own test.
func decodedBody(e *event) (string, bool) {
	raw := e.bodyText()
	if raw == "" {
		return "", false
	}
	switch strings.ToLower(e.Headers["content-encoding"]) {
	case "":
		return raw, true
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader([]byte(raw)))
		if err != nil {
			return "", false
		}
		defer zr.Close()
		out, err := io.ReadAll(zr)
		if err != nil {
			return "", false
		}
		return string(out), true
	default:
		return "", false
	}
}

type event struct {
	decoded string

	Kind    string            `json:"kind"`
	FlowID  string            `json:"flow_id"`
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Status  int               `json:"status_code"`
	Headers map[string]string `json:"headers"`
	Body    struct {
		Encoding string          `json:"encoding"`
		Value    json.RawMessage `json:"value"`
	} `json:"body"`
}

// bodyText normalizes the recorder's three encodings into the bytes the wire
// actually carried.
func (e *event) bodyText() string {
	switch e.Body.Encoding {
	case "base64":
		var s string
		if json.Unmarshal(e.Body.Value, &s) != nil {
			return ""
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return ""
		}
		return string(b)
	case "json":
		return string(e.Body.Value)
	case "utf-8":
		var s string
		if json.Unmarshal(e.Body.Value, &s) == nil {
			return s
		}
		return string(e.Body.Value)
	}
	return ""
}

// withoutContentEncoding copies the header map minus the compression claim.
func withoutContentEncoding(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if strings.EqualFold(k, "content-encoding") {
			continue
		}
		out[k] = v
	}
	return out
}

func fixtureFor(req, resp *event) (any, error) {
	return map[string]any{
		"note": "Sanitized from an openbox-logger desktop-observation run. " +
			"Every free-text field is synthetic filler of the recorded rune length: the request's " +
			"prompts, thinking, tool arguments, tool output and tool descriptions, and the response's " +
			"event-stream deltas. No recorded prose is committed, and every consumer is content-agnostic. " +
			"The response body is DECOMPRESSED and its content-encoding header removed: " +
			"every recorded event-stream response was gzip. " +
			"Chunk boundaries are NOT recorded either — the recorder flattened the response " +
			"into one body, so a replay reconstructs them from SSE frame separators, " +
			"which is what a client sees but not what the wire did.",
		"request": map[string]any{
			"method":  req.Method,
			"url":     req.URL,
			"headers": req.Headers,
			"body":    req.bodyText(),
		},
		"response": map[string]any{
			"status": resp.Status,
			// content-encoding is dropped along with the compression: the body
			// below is the decompressed one, and a fixture whose header claimed
			// gzip over plaintext would make a replaying client fail to decode.
			"headers": withoutContentEncoding(resp.Headers),
			"body":    resp.decoded,
		},
	}, nil
}
