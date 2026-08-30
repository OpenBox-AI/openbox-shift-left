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

// minSSEFrames is how many event-stream frames the streaming fixture must
// carry.
const minSSEFrames = 3

const redactedMarker = "${OPENBOX_REDACTED"

// extractProxy pulls two model-call exchanges out of the recorded proxy
// stream: one with a JSON response and one with an SSE response.
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
			"status":  resp.Status,
			"headers": withoutContentEncoding(resp.Headers),
			"body":    resp.decoded,
		},
	}, nil
}
