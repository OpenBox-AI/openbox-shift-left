package evaluate

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// The relay's one job is to record what the model chose, without changing what
// the guest receives. Both are asserted against a real HTTP round trip.
func TestModelRelayForwardsAndReceiptsTheModelsChoice(t *testing.T) {
	answer := `{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"function":{"name":"send-support-report"}}]}}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("forwarded to %s", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	defer upstream.Close()

	relay, err := startModelRelay(Dependencies{Listen: net.Listen, InferenceHTTP: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	relay.upstream = upstream.URL
	defer relay.Close(context.Background())

	response, err := http.Post("http://"+relay.listener.Addr().String()+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"granite4.1:3b","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}

	receipt := relay.Receipt()
	if receipt.Requests != 1 || receipt.Model != "granite4.1:3b" || receipt.FinishReason != "tool_calls" ||
		!reflect.DeepEqual(receipt.ToolCalls, []string{"send-support-report"}) || receipt.ObservedAt.IsZero() {
		t.Fatalf("receipt = %+v", receipt)
	}

	// Anything other than the chat route is refused and receipts nothing.
	other, err := http.Post("http://"+relay.listener.Addr().String()+"/v1/embeddings", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	other.Body.Close()
	if other.StatusCode != http.StatusNotFound || relay.Receipt().Requests != 1 {
		t.Fatalf("non-chat route: status %d, requests %d", other.StatusCode, relay.Receipt().Requests)
	}
}

// SDKs stream the same call; the tool choice arrives split across deltas.
func TestChosenToolsReadsAStreamedCompletion(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"name\":\"send-support-report\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n"
	names, finish := chosenTools([]byte(stream))
	if !reflect.DeepEqual(names, []string{"send-support-report"}) || finish != "tool_calls" {
		t.Fatalf("names=%v finish=%q", names, finish)
	}
	if names, finish := chosenTools([]byte(`{"choices":[{"finish_reason":"stop","message":{}}]}`)); len(names) != 0 || finish != "stop" {
		t.Fatalf("declined call read as names=%v finish=%q", names, finish)
	}
}
