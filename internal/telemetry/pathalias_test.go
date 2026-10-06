package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/plog"
)

// TestEventNameFallsBackToUnderscoreKey: Muse names its event `event_name`
// (snake_case, like every attribute it exports), where Claude Code and Codex
// use `event.name`. The dotted key still wins when both are present.
func TestEventNameFallsBackToUnderscoreKey(t *testing.T) {
	for _, tc := range []struct {
		name  string
		attrs map[string]string
		want  string
	}{
		{"dotted only", map[string]string{"event.name": "api_request"}, "api_request"},
		{"underscore only", map[string]string{"event_name": "model_call"}, "model_call"},
		{"dotted wins", map[string]string{"event.name": "api_request", "event_name": "model_call"}, "api_request"},
		{"neither", map[string]string{"other": "x"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cap := &captureEmitter{}
			r := newTestReceiver(t, cap)
			if err := r.consumeLogs(context.Background(), logsWith(nil, nil, tc.attrs, time.Now())); err != nil {
				t.Fatal(err)
			}
			if len(cap.records) != 1 || cap.records[0].EventName != tc.want {
				t.Fatalf("records = %+v, want one with EventName %q", cap.records, tc.want)
			}
		})
	}
}

// TestPathAliasRewritesOnlyMusePaths: Muse exports to
// <endpoint>/muse-code/telemetry/{logs,traces}; the collector serves /v1/*.
// The alias rewrites exactly those two paths and nothing else, and leaves the
// caller's request untouched.
func TestPathAliasRewritesOnlyMusePaths(t *testing.T) {
	wrap, err := pathAlias{}.GetHTTPHandler(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var seen string
	h, err := wrap(context.Background(), http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.URL.Path }))
	if err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{
		"/muse-code/telemetry/logs":    "/v1/logs",
		"/muse-code/telemetry/traces":  "/v1/traces",
		"/v1/logs":                     "/v1/logs",
		"/v1/metrics":                  "/v1/metrics",
		"/muse-code/telemetry/metrics": "/muse-code/telemetry/metrics",
		"/muse-code/telemetry/logs/x":  "/muse-code/telemetry/logs/x",
	} {
		req := httptest.NewRequest(http.MethodPost, in, nil)
		h.ServeHTTP(httptest.NewRecorder(), req)
		if seen != want {
			t.Errorf("%s served as %s, want %s", in, seen, want)
		}
		if req.URL.Path != in {
			t.Errorf("the caller's request was mutated: %s -> %s", in, req.URL.Path)
		}
	}
}

func museFixtureProto(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "muse", "logs.json"))
	if err != nil {
		t.Fatal(err)
	}
	ld, err := (&plog.JSONUnmarshaler{}).UnmarshalLogs(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := (&plog.ProtoMarshaler{}).MarshalLogs(ld)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestReceiverServesMusePathsOverHTTP is the socket control: a gzip protobuf
// POST to Muse's path reaches the emitter as records carrying Muse's own
// attribute names and an event name. Skipped where the host cannot bind.
func TestReceiverServesMusePathsOverHTTP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a loopback listener here: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	var mu sync.Mutex
	var got []Record
	r, err := New(Config{Addr: addr}, WithLogWriter(&bytes.Buffer{}), WithEmitter(emitterFunc(func(_ context.Context, rec Record) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, rec)
		return nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.StartStandalone(context.Background()); err != nil {
		t.Skipf("cannot start the receiver here: %v", err)
	}
	defer r.Shutdown(context.Background())
	defer http.DefaultClient.CloseIdleConnections()

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(museFixtureProto(t))
	_ = zw.Close()

	for _, path := range []string{"/muse-code/telemetry/logs", "/v1/logs"} {
		req, _ := http.NewRequest(http.MethodPost, "http://"+addr+path, bytes.NewReader(gz.Bytes()))
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Content-Encoding", "gzip")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s = %d, want 200", path, resp.StatusCode)
		}
	}

	// And traces on Muse's path are accepted (and only counted).
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/muse-code/telemetry/traces", bytes.NewReader(nil))
	req.Header.Set("Content-Type", "application/x-protobuf")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST traces: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Error("Muse's traces path 404s; Muse would log an export error on every batch")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 12 {
		t.Fatalf("got %d records over two posts of a 6-record fixture, want 12", len(got))
	}
	models := 0
	for _, rec := range got {
		if rec.EventName == "model_call" && rec.Attrs["session_id"] != "" {
			models++
		}
	}
	if models != 6 {
		t.Errorf("model_call records = %d, want 6 (3 per post)", models)
	}
}

type emitterFunc func(context.Context, Record) error

func (f emitterFunc) Emit(ctx context.Context, r Record) error { return f(ctx, r) }
