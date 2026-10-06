package gateway

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestResponseIsNotBuffered is the no-buffering control.
func TestResponseIsNotBuffered(t *testing.T) {
	const stall = 600 * time.Millisecond

	upstream := sseUpstream(t, func(w http.ResponseWriter, ctl *http.ResponseController) {
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		ctl.Flush()
		time.Sleep(stall)
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		ctl.Flush()
	})
	gw := newTestGateway(t, upstream.URL)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	start := time.Now()
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	defer resp.Body.Close()

	first := make([]byte, 1)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("reading first byte: %v", err)
	}
	ttfb := time.Since(start)

	if ttfb >= stall/2 {
		t.Errorf("first byte took %v with a %v upstream stall: the response is being buffered", ttfb, stall)
	}
}

// TestPingAndCommentLinesRelayed keeps the bytes that hold a long thinking
// pause open.
func TestPingAndCommentLinesRelayed(t *testing.T) {
	upstream := sseUpstream(t, func(w http.ResponseWriter, ctl *http.ResponseController) {
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		ctl.Flush()
		io.WriteString(w, ": this is a bare comment line\n\n")
		ctl.Flush()
		io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		ctl.Flush()
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		ctl.Flush()
	})
	gw := newTestGateway(t, upstream.URL)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading stream: %v", err)
	}

	for _, want := range []string{
		"event: message_start",
		": this is a bare comment line",
		"event: ping",
		"event: message_stop",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("stream lost %q\nfull stream:\n%s", want, body)
		}
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type: got %q want text/event-stream", ct)
	}
}

// TestStreamChunkBoundariesPreserved checks that the tee does not re-frame the
// stream.
func TestStreamChunkBoundariesPreserved(t *testing.T) {
	upstream := sseUpstream(t, func(w http.ResponseWriter, ctl *http.ResponseController) {
		for _, line := range []string{"one", "two", "three"} {
			io.WriteString(w, "data: "+line+"\n\n")
			ctl.Flush()
			time.Sleep(60 * time.Millisecond)
		}
	})
	gw := newTestGateway(t, upstream.URL)

	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	resp, err := probeClient().Do(req)
	if err != nil {
		t.Fatalf("request through gateway: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	var arrivals []time.Duration
	start := time.Now()
	for {
		line, err := reader.ReadString('\n')
		if strings.HasPrefix(line, "data: ") {
			arrivals = append(arrivals, time.Since(start))
		}
		if err != nil {
			break
		}
	}

	if len(arrivals) != 3 {
		t.Fatalf("got %d data lines want 3", len(arrivals))
	}
	if arrivals[2]-arrivals[0] < 60*time.Millisecond {
		t.Errorf("all three events arrived within %v: the relay coalesced the stream", arrivals[2]-arrivals[0])
	}
}
