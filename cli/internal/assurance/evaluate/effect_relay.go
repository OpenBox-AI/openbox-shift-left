package evaluate

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

type effectReceipt struct {
	Attempts, MatchingReceipts int
	MatchedAt                  time.Time
}

type effectRelay struct {
	listener     net.Listener
	server       *http.Server
	evaluationID string
	mu           sync.Mutex
	receipt      effectReceipt
}

func startEffectRelay(dependencies Dependencies, evaluationID string) (*effectRelay, error) {
	listener, err := dependencies.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	relay := &effectRelay{listener: listener, evaluationID: evaluationID}
	relay.server = &http.Server{Handler: http.HandlerFunc(relay.serveHTTP), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() { _ = relay.server.Serve(listener) }()
	return relay, nil
}

func (relay *effectRelay) Port() int { return relay.listener.Addr().(*net.TCPAddr).Port }
func (relay *effectRelay) Close(ctx context.Context) error {
	if err := relay.server.Shutdown(ctx); err != nil {
		return relay.server.Close()
	}
	return nil
}
func (relay *effectRelay) Receipt() effectReceipt {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return relay.receipt
}

func (relay *effectRelay) serveHTTP(response http.ResponseWriter, request *http.Request) {
	relay.mu.Lock()
	relay.receipt.Attempts++
	relay.mu.Unlock()
	if request.Method != http.MethodPost || request.URL.Path != "/effects/safe" || request.URL.RawQuery != "" || request.Header.Get("content-type") != "application/json" {
		http.Error(response, "not found", http.StatusNotFound)
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(response, request.Body, 1024))
	decoder.DisallowUnknownFields()
	var body struct {
		EvaluationID string `json:"evaluation_id"`
	}
	if decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF || body.EvaluationID != relay.evaluationID {
		http.Error(response, "invalid", http.StatusBadRequest)
		return
	}
	relay.mu.Lock()
	relay.receipt.MatchingReceipts++
	relay.receipt.MatchedAt = time.Now().UTC()
	relay.mu.Unlock()
	response.WriteHeader(http.StatusNoContent)
}
