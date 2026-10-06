package memhttptest_test

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/internal/client/memhttptest"
)

// TestHandlerWriteJustBeforeReturnIsNotLost is the regression this package's
// hand-rolled buffered conn existed for: net.Pipe is synchronous, so a
// response written on the last line of a handler could be discarded when the
// server closed the connection before the client had read it. It is written
// against the hand-rolled implementation first, so that swapping in
// grpc's bufconn is a change with a test already watching it rather than a
// swap taken on trust.
func TestHandlerWriteJustBeforeReturnIsNotLost(t *testing.T) {
	for _, size := range []int{1, 4 << 10, 512 << 10} {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			body := strings.Repeat("x", size)
			srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, body) // last statement; the handler returns immediately after
			}))
			defer srv.Close()

			resp, err := srv.Client().Get(srv.URL)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if len(got) != size {
				t.Errorf("read %d bytes, want %d: the tail of a response written just before the "+
					"handler returned was dropped when the connection closed", len(got), size)
			}
		})
	}
}

// TestBodyLargerThanTheConnectionBufferStillArrives. The buffer between the
// two halves is finite, and a body past it makes the writer wait for the
// reader rather than fail. Flow control, not a cliff -- but only while both
// halves are actually being pumped, which is the property worth pinning.
func TestBodyLargerThanTheConnectionBufferStillArrives(t *testing.T) {
	const size = 3 << 20 // deliberately past any sane buffer choice
	want := make([]byte, size)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}

	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(want)
	}))
	defer srv.Close()

	resp, err := srv.Client().Post(srv.URL, "application/octet-stream", strings.NewReader(strings.Repeat("q", size)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(got) != size {
		t.Fatalf("read %d bytes, want %d", len(got), size)
	}
	if string(got) != string(want) {
		t.Error("body came back altered")
	}
}

// TestCloseIsIdempotent. Call sites use both `defer srv.Close()` and the
// t.Cleanup NewServer registers itself, so every server in the suite is closed
// at least twice.
func TestCloseIsIdempotent(t *testing.T) {
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	srv.Close()
	srv.Close()
	srv.Close()

	if _, err := srv.Client().Get(srv.URL); err == nil {
		t.Error("a closed server still answered; the registry entry outlived it")
	}
}

// TestDialContextReachesARegisteredServer holds the exported seam the eight
// tests whose code builds its own http.Transport depend on.
func TestDialContextReachesARegisteredServer(t *testing.T) {
	srv := memhttptest.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "reached")
	}))
	defer srv.Close()

	c := &http.Client{Transport: &http.Transport{DialContext: memhttptest.DialContext}}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET through an own-Transport client: %v", err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "reached" {
		t.Errorf("body = %q, want %q", got, "reached")
	}
}
