package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mktcz/uniproxy/internal/protocols"
	"github.com/mktcz/uniproxy/internal/protocols/anthropic"
	"github.com/mktcz/uniproxy/internal/protocols/openai"
	"github.com/mktcz/uniproxy/internal/providers"
)

type fakeProvider struct{}

func (fakeProvider) Name() string { return "fake" }
func (fakeProvider) Models() []protocols.ModelInfo {
	return []protocols.ModelInfo{
		{ID: "codex/gpt-5.6-luna", OwnedBy: "codex"},
		{ID: "codex/gpt-test", OwnedBy: "codex"},
	}
}
func (fakeProvider) Generate(_ context.Context, request protocols.Request) (protocols.Response, error) {
	return protocols.Response{ID: "resp_1", Object: "response", CreatedAt: 1, Status: "completed", Model: request.Model, Output: []protocols.Item{}}, nil
}
func (fakeProvider) Stream(_ context.Context, _ protocols.Request) (<-chan protocols.StreamResult, error) {
	results := make(chan protocols.StreamResult, 2)
	results <- protocols.StreamResult{Event: &protocols.StreamEvent{Type: protocols.EventTextDelta, SequenceNumber: 1, Delta: "hi"}}
	results <- protocols.StreamResult{Event: &protocols.StreamEvent{Type: protocols.EventResponseCompleted, SequenceNumber: 2, Response: &protocols.Response{ID: "resp_1", Object: "response", Status: "completed", Model: "gpt-test", Output: []protocols.Item{}}}}
	close(results)
	return results, nil
}

func testServer() *Server {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	providerRegistry := providers.NewRegistry()
	providerRegistry.Register(fakeProvider{})
	protocolRegistry := protocols.NewRegistry()
	protocolRegistry.Register(openai.Codec{})
	protocolRegistry.Register(anthropic.Codec{})
	return New("127.0.0.1:0", time.Second, time.Second, providerRegistry, protocolRegistry, logger)
}

func TestModelsAndAnthropicRoutes(t *testing.T) {
	server := httptest.NewServer(testServer().Handler())
	defer server.Close()

	modelsResp, err := server.Client().Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(modelsResp.Body)
	_ = modelsResp.Body.Close()
	if modelsResp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"object":"list"`) || !strings.Contains(string(body), "codex/gpt-5.6-luna") {
		t.Fatalf("openai models: status=%d body=%s", modelsResp.StatusCode, body)
	}

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	anthropicModels, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(anthropicModels.Body)
	_ = anthropicModels.Body.Close()
	if anthropicModels.StatusCode != http.StatusOK || !strings.Contains(string(body), `"type":"model"`) {
		t.Fatalf("anthropic models: status=%d body=%s", anthropicModels.StatusCode, body)
	}

	msgReq, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"model":"codex/gpt-5.6-luna","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	msgReq.Header.Set("Content-Type", "application/json")
	msgResp, err := server.Client().Do(msgReq)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(msgResp.Body)
	_ = msgResp.Body.Close()
	if msgResp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"type":"message"`) {
		t.Fatalf("messages: status=%d body=%s", msgResp.StatusCode, body)
	}

	countReq, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/messages/count_tokens", strings.NewReader(`{"model":"codex/gpt-5.6-luna","messages":[{"role":"user","content":"hi"}]}`))
	countReq.Header.Set("Content-Type", "application/json")
	countResp, err := server.Client().Do(countReq)
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(countResp.Body)
	_ = countResp.Body.Close()
	if countResp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"input_tokens"`) {
		t.Fatalf("count_tokens: status=%d body=%s", countResp.StatusCode, body)
	}
}

func TestResponsesJSON(t *testing.T) {
	server := httptest.NewServer(testServer().Handler())
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"codex/gpt-test","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), `"id":"resp_1"`) {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
}

func TestResponsesStreamingAndValidation(t *testing.T) {
	server := httptest.NewServer(testServer().Handler())
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"codex/gpt-test","input":"hello","stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.Header.Get("Content-Type") != "text/event-stream" || !strings.Contains(string(body), "event: response.output_text.delta") {
		t.Fatalf("headers=%#v body=%s", response.Header, body)
	}

	bad, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-test","unknown":1}`))
	bad.Header.Set("Content-Type", "application/json")
	badResponse, err := server.Client().Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	defer badResponse.Body.Close()
	if badResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d", badResponse.StatusCode)
	}
}

func TestUnknownModelIsRejected(t *testing.T) {
	server := httptest.NewServer(testServer().Handler())
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"missing-model","input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), `"code":"model_not_found"`) {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
}
