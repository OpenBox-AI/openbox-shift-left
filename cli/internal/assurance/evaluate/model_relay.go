package evaluate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// modelReceipt is what the model route independently shows: which model was
// asked, and which tools it CHOSE to call. That choice is the evidence a
// backend record cannot give — the SDK reports that a tool ran, not whether the
// model or the application decided it should.
type modelReceipt struct {
	Requests     int
	Model        string
	ToolCalls    []string
	FinishReason string
	ObservedAt   time.Time
}

// modelRelay forwards the guest's OpenAI-compatible calls to the host's Ollama
// and keeps a receipt. Only the local-ollama route has one: a gateway-served
// route is resolved inside OpenShell, where this process cannot stand.
type modelRelay struct {
	listener net.Listener
	server   *http.Server
	client   HTTPDoer
	upstream string
	mu       sync.Mutex
	receipt  modelReceipt
}

const ollamaOpenAIBase = "http://127.0.0.1:11434"

func startModelRelay(dependencies Dependencies) (*modelRelay, error) {
	listener, err := dependencies.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	relay := &modelRelay{listener: listener, client: dependencies.inferenceHTTP(), upstream: ollamaOpenAIBase}
	relay.server = &http.Server{Handler: http.HandlerFunc(relay.serveHTTP), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 32 << 10}
	go func() { _ = relay.server.Serve(listener) }()
	return relay, nil
}

func (relay *modelRelay) Port() int { return relay.listener.Addr().(*net.TCPAddr).Port }
func (relay *modelRelay) Close(ctx context.Context) error {
	if err := relay.server.Shutdown(ctx); err != nil {
		return relay.server.Close()
	}
	return nil
}
func (relay *modelRelay) Receipt() modelReceipt {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	receipt := relay.receipt
	receipt.ToolCalls = append([]string(nil), relay.receipt.ToolCalls...)
	return receipt
}

func (relay *modelRelay) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" {
		http.Error(response, "not found", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, 4<<20))
	if err != nil {
		http.Error(response, "invalid", http.StatusBadRequest)
		return
	}
	upstream, err := http.NewRequestWithContext(request.Context(), http.MethodPost, relay.upstream+request.URL.Path, bytes.NewReader(body))
	if err != nil {
		http.Error(response, "invalid", http.StatusBadRequest)
		return
	}
	upstream.Header.Set("content-type", "application/json")
	result, err := relay.client.Do(upstream)
	if err != nil {
		http.Error(response, "model route unavailable", http.StatusBadGateway)
		return
	}
	defer result.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(result.Body, 8<<20))
	if err != nil {
		http.Error(response, "model route unavailable", http.StatusBadGateway)
		return
	}

	var asked struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &asked)
	tools, finish := chosenTools(answer)
	relay.mu.Lock()
	relay.receipt.Requests++
	relay.receipt.Model = asked.Model
	relay.receipt.ToolCalls = append(relay.receipt.ToolCalls, tools...)
	if finish != "" {
		relay.receipt.FinishReason = finish
	}
	relay.receipt.ObservedAt = time.Now().UTC()
	relay.mu.Unlock()

	if contentType := result.Header.Get("content-type"); contentType != "" {
		response.Header().Set("content-type", contentType)
	}
	response.WriteHeader(result.StatusCode)
	_, _ = response.Write(answer)
}

// chosenTools reads the tool calls and finish reason from either a plain
// completion or a streamed one, since SDKs use both for the same call.
func chosenTools(answer []byte) ([]string, string) {
	type choice struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			ToolCalls []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		Delta struct {
			ToolCalls []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	}
	var names []string
	finish := ""
	read := func(document []byte) {
		var completion struct{ Choices []choice }
		if json.Unmarshal(document, &completion) != nil {
			return
		}
		for _, c := range completion.Choices {
			for _, call := range append(c.Message.ToolCalls, c.Delta.ToolCalls...) {
				if call.Function.Name != "" {
					names = append(names, call.Function.Name)
				}
			}
			if c.FinishReason != "" {
				finish = c.FinishReason
			}
		}
	}
	trimmed := bytes.TrimSpace(answer)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		read(trimmed)
		return names, finish
	}
	scanner := bufio.NewScanner(bytes.NewReader(answer))
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		if line, ok := strings.CutPrefix(scanner.Text(), "data: "); ok && line != "[DONE]" {
			read([]byte(line))
		}
	}
	return names, finish
}
