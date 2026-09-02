package evaluate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type relayReceipt struct {
	ValidationAttempts   int
	ValidationSuccesses  int
	MatchingValidations  int
	GovernanceEvents     int
	LastValidationStatus int
	AuthorizationClass   string
}

type coreRelay struct {
	listener     net.Listener
	server       *http.Server
	client       HTTPDoer
	target       *url.URL
	agentID      string
	evaluationID string
	mu           sync.Mutex
	receipt      relayReceipt
}

func startCoreRelay(dependencies Dependencies, agentID, evaluationID string) (*coreRelay, error) {
	listener, err := dependencies.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("project evaluate: start Core relay: %w", err)
	}
	target, _ := url.Parse(coreURL)
	relay := &coreRelay{listener: listener, client: dependencies.HTTP, target: target, agentID: agentID, evaluationID: evaluationID}
	relay.server = &http.Server{
		Handler:           http.HandlerFunc(relay.serveHTTP),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       5 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	go func() { _ = relay.server.Serve(listener) }()
	return relay, nil
}

func (relay *coreRelay) Port() int {
	return relay.listener.Addr().(*net.TCPAddr).Port
}

func (relay *coreRelay) Close(ctx context.Context) error {
	if err := relay.server.Shutdown(ctx); err != nil {
		return relay.server.Close()
	}
	return nil
}

func (relay *coreRelay) Receipt() relayReceipt {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return relay.receipt
}

func (relay *coreRelay) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if !allowedCoreRequest(request.Method, request.URL.Path, request.URL.RawQuery) {
		http.Error(response, "not found", http.StatusNotFound)
		return
	}
	if request.Method == http.MethodGet && request.URL.Path == "/api/v1/auth/validate" {
		relay.mu.Lock()
		relay.receipt.AuthorizationClass = classifyAuthorization(request.Header.Get("authorization"))
		relay.mu.Unlock()
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		http.Error(response, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	upstreamURL := *relay.target
	upstreamURL.Path = request.URL.Path
	upstream, err := http.NewRequestWithContext(request.Context(), request.Method, upstreamURL.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(response, "bad gateway", http.StatusBadGateway)
		return
	}
	copyHeaders(upstream.Header, request.Header)
	upstream.Host = relay.target.Host
	result, err := relay.client.Do(upstream)
	if err != nil {
		http.Error(response, "bad gateway", http.StatusBadGateway)
		return
	}
	defer result.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(result.Body, (1<<20)+1))
	if err != nil || len(responseBody) > 1<<20 {
		http.Error(response, "bad gateway", http.StatusBadGateway)
		return
	}
	copyHeaders(response.Header(), result.Header)
	response.Header().Set("content-length", strconv.Itoa(len(responseBody)))
	response.Header().Set("connection", "close")
	response.WriteHeader(result.StatusCode)
	_, _ = response.Write(responseBody)
	relay.observe(request.Method, request.URL.Path, body, result.StatusCode, responseBody)
}

func classifyAuthorization(value string) string {
	switch {
	case value == "":
		return "missing"
	case strings.HasPrefix(value, "Bearer openshell:resolve:"):
		return "openshell_placeholder"
	case strings.HasPrefix(value, "Bearer obx_"):
		return "openbox_runtime_key"
	case strings.HasPrefix(value, "Bearer "):
		return "other_bearer"
	default:
		return "other_scheme"
	}
}

func (relay *coreRelay) observe(method, requestPath string, requestBody []byte, status int, responseBody []byte) {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if method == http.MethodGet && requestPath == "/api/v1/auth/validate" {
		relay.receipt.ValidationAttempts++
		relay.receipt.LastValidationStatus = status
		if status != http.StatusOK {
			return
		}
		relay.receipt.ValidationSuccesses++
		var validation struct {
			AgentID string `json:"agent_id"`
			Valid   bool   `json:"valid"`
			Active  bool   `json:"active"`
		}
		if json.Unmarshal(responseBody, &validation) == nil && validation.AgentID == relay.agentID && validation.Valid && validation.Active {
			relay.receipt.MatchingValidations++
		}
	}
	if method == http.MethodPost && requestPath == "/api/v1/governance/evaluate" && status >= 200 && status < 300 &&
		bytes.Contains(requestBody, []byte(relay.evaluationID)) {
		relay.receipt.GovernanceEvents++
	}
}

func allowedCoreRequest(method, requestPath, query string) bool {
	if query != "" {
		return false
	}
	return (method == http.MethodGet && requestPath == "/api/v1/auth/validate") ||
		(method == http.MethodPost && requestPath == "/api/v1/governance/evaluate") ||
		(method == http.MethodPost && requestPath == "/api/v1/governance/approval")
}

func copyHeaders(destination, source http.Header) {
	for name, values := range source {
		if strings.EqualFold(name, "connection") || strings.EqualFold(name, "transfer-encoding") || strings.EqualFold(name, "content-length") {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}
