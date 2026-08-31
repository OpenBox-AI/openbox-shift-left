package gateway

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The two capabilities Phase 06 identified as missing. Both were absent because
// Trailer and Te sat in the hop-by-hop table and were dropped wholesale, and
// because nothing watched for an informational response.

// TestRelayPropagatesTrailers. A trailer is a header the upstream can only know
// after the body -- a final token count, a checksum. Dropping the Trailer
// announcement meant the client never learned to wait for them, so they were
// silently lost.
func TestRelayPropagatesTrailers(t *testing.T) {
	_, front := relayFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "X-Final-Tokens")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
		w.(http.Flusher).Flush()
		w.Header().Set("X-Final-Tokens", "1234")
	})

	// Read the wire directly: net/http exposes Response.Trailer only after the
	// body is drained, and going through it is exactly what a real client does.
	resp, err := probeClient().Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("relayed request: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("draining the body: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q, want the upstream's", body)
	}
	if got := resp.Trailer.Get("X-Final-Tokens"); got != "1234" {
		t.Errorf("trailer X-Final-Tokens = %q, want %q. The upstream sent it after the body and the "+
			"relay dropped it, so a client counting tokens from the trailer sees nothing", got, "1234")
	}
}

// TestRelayPropagatesAnUnannouncedTrailer. A trailer the upstream did not
// announce is legal, and an announced-only copy drops it.
func TestRelayPropagatesAnUnannouncedTrailer(t *testing.T) {
	_, front := relayFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
		w.(http.Flusher).Flush()
		w.Header().Set(http.TrailerPrefix+"X-Late", "surprise")
	})

	resp, err := probeClient().Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("relayed request: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := resp.Trailer.Get("X-Late"); got != "surprise" {
		t.Errorf("unannounced trailer X-Late = %q, want %q", got, "surprise")
	}
}

// TestRelayForwardsTheTrailersIntentUpstream. Te: trailers is how a client says
// it can take them. Dropping it entirely tells the upstream nobody downstream
// can, so the upstream never sends any and the propagation above never triggers.
func TestRelayForwardsTheTrailersIntentUpstream(t *testing.T) {
	var seen http.Header
	_, front := relayFixture(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		// net/http moves a request's TE into r.TransferEncoding handling, so
		// record both places it can land.
		if len(r.TransferEncoding) > 0 {
			seen.Set("X-Observed-Transfer-Encoding", strings.Join(r.TransferEncoding, ","))
		}
		io.WriteString(w, `{"ok":true}`)
	})

	addr := strings.TrimPrefix(front.URL, "http://")
	resp := rawExchangeTo(t, addr, "POST /v1/messages HTTP/1.1\r\n"+
		"Host: "+addr+"\r\n"+
		"Content-Length: 2\r\n"+
		"TE: trailers\r\n\r\n{}")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := seen.Get("Te"); !strings.Contains(strings.ToLower(got), "trailers") {
		t.Errorf("the upstream saw Te = %q, want it to contain \"trailers\": without it the upstream is "+
			"told nothing downstream can take a trailer, so it sends none", got)
	}
}

// TestRelayForwardsAnInformationalResponse. A 1xx is a real answer -- 100
// Continue, or 103 Early Hints -- and swallowing it means the provider said
// something the client never heard.
func TestRelayForwardsAnInformationalResponse(t *testing.T) {
	_, front := relayFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Hint", "preconnect")
		w.WriteHeader(http.StatusEarlyHints)
		// net/http does not clear the header map after an informational
		// response, so the upstream drops its own hint here. Otherwise the
		// upstream really would send X-Hint on both responses, and a relay that
		// forwards byte-identically has to carry it on both.
		w.Header().Del("X-Hint")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"ok":true}`)
	})

	// Read raw, because net/http's client consumes 1xx into a trace hook rather
	// than surfacing it on the Response.
	addr := strings.TrimPrefix(front.URL, "http://")
	conn, err := dialFront(t, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := io.WriteString(conn, "POST /v1/messages HTTP/1.1\r\nHost: "+addr+
		"\r\nContent-Length: 2\r\n\r\n{}"); err != nil {
		t.Fatalf("write: %v", err)
	}
	br := bufio.NewReader(conn)

	// The informational response comes first, on its own.
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the first status line: %v", err)
	}
	if !strings.Contains(statusLine, "103") {
		t.Fatalf("first status line = %q, want a 103 Early Hints. The provider sent an informational "+
			"response and the relay swallowed it", strings.TrimSpace(statusLine))
	}
	var sawHint bool
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading the 1xx headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		if strings.Contains(strings.ToLower(line), "x-hint: preconnect") {
			sawHint = true
		}
	}
	if !sawHint {
		t.Error("the 1xx reached the client without its headers")
	}

	// And then the real answer, on the same connection.
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the final response: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("final status = %d, want 200", resp.StatusCode)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("final body = %q, want the upstream's", body)
	}
	if got := resp.Header.Get("X-Hint"); got != "" {
		t.Errorf("the 1xx header X-Hint leaked onto the final response as %q; they belong to "+
			"different responses", got)
	}
}
