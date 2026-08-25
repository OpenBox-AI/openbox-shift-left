//go:build ignore

// probe-server.go — the throwaway localhost server phase 03's three probes share.
//
// It is NOT shipped code and deliberately lives in the plan directory, not in a
// module: it exists to answer three questions about a provider we do not control,
// after which it is deleted. Build tag `ignore` keeps `go build ./...` away from
// it even if the plan tree is ever swept into a module.
//
// Run it, point Claude Code at it, start a session, read what it printed:
//
//	go run probe-server.go                       # P0 + P1: observe, then 200 a stub reply
//	go run probe-server.go -refuse 403 -shape anthropic   # probe A: refusal shapes
//
// # What it answers
//
//	P0  Does ANTHROPIC_BASE_URL redirect this auth mode at all? A request that
//	    ARRIVES answers yes for the mode under test; silence answers no. Run it
//	    once per mode (subscription OAuth, API key) — that per-mode yes/no is the
//	    whole deliverable, and it decides who pass-through auth can cover.
//	P1  Is an org identifier matchable from the credential or the response path?
//	    Printed as claim KEYS and value SHAPES only (see redact()).
//	A   Which refusal shape stops a call without tripping Claude Code's
//	    capability-rejection retry? Drive each -shape and watch the CLIENT.
//
// # The one rule this file enforces in code rather than in a comment
//
// It never prints a credential. Not truncated, not "just the prefix of a test
// key" — the probe reports are committed to the repo, and a report that leaks a
// live token is a worse outcome than an unanswered probe. Values are reduced to
// (kind, length, sha256[:8]) before they are printable at all. The fingerprint is
// there so two runs can be compared without either being reversible.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
)

var (
	addr    = flag.String("addr", "127.0.0.1:8787", "listen address")
	refuse  = flag.Int("refuse", 0, "if non-zero, answer every /v1/messages with this status (probe A)")
	shape   = flag.String("shape", "anthropic", "refusal body shape: anthropic|plain|empty (probe A)")
	bodyCap = flag.Int("body-cap", 4096, "how many bytes of the request body to inspect")
)

func main() {
	flag.Parse()
	http.HandleFunc("/", handle)
	log.Printf("probe server on http://%s — set ANTHROPIC_BASE_URL to it", *addr)
	if *refuse != 0 {
		log.Printf("probe A mode: refusing with %d, shape=%s", *refuse, *shape)
	}
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, int64(*bodyCap)))

	fmt.Printf("\n=== %s %s (%s) ===\n", r.Method, r.URL.Path, r.Proto)
	// P0's answer is this block existing at all: a request that arrived is a
	// request the client redirected.
	fmt.Println("-- headers (values reduced; see redact) --")
	for _, k := range sortedKeys(r.Header) {
		for _, v := range r.Header[k] {
			fmt.Printf("  %-28s %s\n", k+":", redact(k, v))
		}
	}
	// P1: an org identifier, if one is reachable at all, is either a JWT claim on
	// the bearer or a response header the provider sets. Only the first is visible
	// from here; the second needs a real upstream and is the runbook's step 3b.
	if claims := bearerClaimShapes(r.Header.Get("Authorization")); claims != "" {
		fmt.Printf("-- bearer is a JWT; claim keys and value SHAPES --\n%s\n", claims)
	}
	fmt.Printf("-- request body: %d bytes inspected, keys: %s --\n", len(body), topLevelKeys(body))

	if *refuse != 0 {
		writeRefusal(w, *refuse, *shape)
		return
	}
	// A minimal, well-formed non-streaming reply. Enough for the client to accept
	// the exchange and move on; not enough to be mistaken for a real model.
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"msg_probe","type":"message","role":"assistant",` +
		`"model":"probe","content":[{"type":"text","text":"probe"}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
}

// writeRefusal renders one candidate refusal shape for probe A. The question each
// shape answers is not "does the call fail" — every shape fails the call — but
// "does the CLIENT treat it as a policy refusal, a transport error it retries, or
// a capability it disables for the rest of the session".
func writeRefusal(w http.ResponseWriter, status int, shape string) {
	w.Header().Set("Content-Type", "application/json")
	switch shape {
	case "anthropic":
		// The provider's own error envelope. The hypothesis worth testing first:
		// a client that special-cases upstream wording will not special-case a
		// message it has never seen.
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"permission_error",` +
			`"message":"OpenBox policy refused this request."}}`))
	case "plain":
		w.WriteHeader(status)
		_, _ = w.Write([]byte("OpenBox policy refused this request.\n"))
	case "empty":
		w.WriteHeader(status)
	default:
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"unknown shape"}`))
	}
}

// sensitive matches headers whose VALUE must never be printed. Matched on the
// header name, and deliberately broad: a header this probe has not seen before is
// more likely to carry a secret than to be worth reading verbatim.
var sensitive = regexp.MustCompile(`(?i)authorization|api[-_]?key|token|cookie|secret|session`)

// redact reduces a header value to a shape. For everything non-sensitive the
// value is printed as-is — the whole point is to see what the client sends.
func redact(name, v string) string {
	if !sensitive.MatchString(name) {
		return v
	}
	sum := sha256.Sum256([]byte(v))
	return fmt.Sprintf("<%s len=%d sha256=%s>", credKind(v), len(v), hex.EncodeToString(sum[:])[:8])
}

// credKind names the credential class without revealing it. This is P0's second
// half: which auth mode reached the gateway, and did the header arrive intact.
func credKind(v string) string {
	switch {
	case strings.HasPrefix(v, "Bearer sk-ant-oat"):
		return "oauth-access-token"
	case strings.HasPrefix(v, "Bearer sk-ant-"):
		return "anthropic-api-key(bearer)"
	case strings.HasPrefix(v, "sk-ant-"):
		return "anthropic-api-key"
	case strings.HasPrefix(v, "Bearer ey"):
		return "jwt"
	case strings.HasPrefix(v, "Bearer "):
		return "bearer-opaque"
	default:
		return "opaque"
	}
}

// bearerClaimShapes decodes a JWT bearer's claim KEYS and the SHAPE of each
// value — never a value. If an org id is reachable from the credential, it shows
// up here as a uuid-shaped claim, and the runbook records the KEY NAME only.
func bearerClaimShapes(auth string) string {
	tok := strings.TrimPrefix(auth, "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	var b strings.Builder
	for _, k := range sortedMapKeys(claims) {
		fmt.Fprintf(&b, "  %-24s %s\n", k+":", valueShape(claims[k]))
	}
	return b.String()
}

var (
	uuidRe  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// valueShape classifies a claim value. uuid and email are called out by name
// because they are exactly what P1 is looking for: an org identifier and the
// account it belongs to.
func valueShape(v any) string {
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("<%T>", v)
	}
	switch {
	case uuidRe.MatchString(s):
		return "<uuid — MATCHABLE as an org/account id>"
	case emailRe.MatchString(s):
		return "<email>"
	default:
		return fmt.Sprintf("<string len=%d>", len(s))
	}
}

// topLevelKeys names the request body's keys without printing any value: the
// body is the developer's prompt and their file contents.
func topLevelKeys(body []byte) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return "(not JSON, or truncated by -body-cap)"
	}
	return strings.Join(sortedMapKeys(toAny(m)), ", ")
}

func toAny[T any](m map[string]T) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sortedKeys(h http.Header) []string {
	out := make([]string, 0, len(h))
	for k := range h {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortedMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// sortStrings avoids importing sort for one call, keeping this file's dependency
// surface to what a throwaway probe needs.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
