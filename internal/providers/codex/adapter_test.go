package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mktcz/uniproxy/internal/protocols"
)

func testAdapter(t *testing.T, client *http.Client, baseURL string, credential Credential) *Adapter {
	t.Helper()
	adapter := New(filepath.Join(t.TempDir(), "auth.json"), client)
	adapter.baseURL = strings.TrimRight(baseURL, "/")
	if err := adapter.save(credential); err != nil {
		t.Fatal(err)
	}
	adapter.cache = &credential
	return adapter
}

func TestBuildRequestBodyStripsModelPrefix(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"codex/gpt-5.6-luna", "gpt-5.6-luna"},
		{"gpt-5.6-luna", "gpt-5.6-luna"},
	} {
		body, err := buildRequestBody(protocols.Request{Model: tc.in})
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if got := payload["model"]; got != tc.want {
			t.Fatalf("model %q = %#v, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGenerateForwardsMultimodalContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "gpt-test" {
			t.Fatalf("upstream model = %#v, want bare gpt-test", payload["model"])
		}
		message := payload["input"].([]any)[0].(map[string]any)
		content := message["content"].([]any)
		if len(content) != 3 {
			t.Fatalf("content parts = %d", len(content))
		}
		image := content[1].(map[string]any)
		if image["type"] != "input_image" || image["image_url"] != "data:image/png;base64,AA==" {
			t.Fatalf("image part: %#v", image)
		}
		audio := content[2].(map[string]any)
		if audio["type"] != "input_audio" || audio["data"] != "SUQz" || audio["format"] != "mp3" {
			t.Fatalf("audio part: %#v", audio)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":\"gpt-test\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	adapter := testAdapter(t, server.Client(), server.URL, Credential{AccessToken: "access", RefreshToken: "refresh", AccountID: "acct", ExpiresAt: time.Now().Add(time.Hour)})
	_, err := adapter.Generate(context.Background(), protocols.Request{
		Model: "codex/gpt-test", ParallelToolCalls: true,
		Input: []protocols.Item{{Type: "message", Role: "user", Content: []protocols.ContentPart{
			{Type: "input_text", Text: "look"},
			{Type: "input_image", ImageURL: "data:image/png;base64,AA=="},
			{Type: "input_audio", AudioData: "SUQz", AudioFormat: "mp3"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGenerateForwardsMultimodalFunctionCallOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		item := payload["input"].([]any)[0].(map[string]any)
		output, ok := item["output"].([]any)
		if !ok || len(output) != 2 {
			t.Fatalf("multimodal output: %#v", item["output"])
		}
		image := output[1].(map[string]any)
		if image["type"] != "input_image" || image["image_url"] != "data:image/png;base64,AA==" {
			t.Fatalf("image output: %#v", image)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":\"gpt-test\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	adapter := testAdapter(t, server.Client(), server.URL, Credential{AccessToken: "access", RefreshToken: "refresh", AccountID: "acct", ExpiresAt: time.Now().Add(time.Hour)})
	_, err := adapter.Generate(context.Background(), protocols.Request{
		Model: "gpt-test", ParallelToolCalls: true,
		Input: []protocols.Item{{Type: "function_call_output", CallID: "call_1", OutputContent: []protocols.ContentPart{
			{Type: "input_text", Text: "screenshot"},
			{Type: "input_image", ImageURL: "data:image/png;base64,AA=="},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGenerateBuildsCleanCodexRequestAndAggregates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("Chatgpt-Account-Id") != "acct" {
			t.Errorf("missing credential headers: %#v", r.Header)
		}
		if r.Header.Get("User-Agent") != UserAgent || r.Header.Get("Originator") != Originator {
			t.Errorf("wrong harness profile: %#v", r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["stream"] != true || payload["store"] != false || payload["instructions"] != "" {
			t.Errorf("missing Codex defaults: %#v", payload)
		}
		input := payload["input"].([]any)
		message := input[0].(map[string]any)
		if message["role"] != "developer" || !strings.HasPrefix(message["id"].(string), "msg_") {
			t.Errorf("message not normalized: %#v", message)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_out\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\",\"annotations\":[]}]}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"gpt-test\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	adapter := testAdapter(t, server.Client(), server.URL, Credential{AccessToken: "access", RefreshToken: "refresh", AccountID: "acct", ExpiresAt: time.Now().Add(time.Hour)})
	response, err := adapter.Generate(context.Background(), protocols.Request{
		Model: "gpt-test", ParallelToolCalls: true,
		Input: []protocols.Item{{Type: "message", ID: "foreign-id", Role: "system", Content: []protocols.ContentPart{{Type: "input_text", Text: "rules"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ID != "resp_1" || len(response.Output) != 1 || response.Output[0].Content[0].Text != "hello" {
		t.Fatalf("unexpected aggregate response: %#v", response)
	}
}

func TestGeneratePreservesAssistantMessagePhase(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		message := payload["input"].([]any)[0].(map[string]any)
		if message["role"] != "assistant" || message["phase"] != "commentary" {
			t.Errorf("assistant phase was not preserved: %#v", message)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":\"gpt-test\",\"output\":[{\"type\":\"message\",\"id\":\"msg_2\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\",\"annotations\":[]}]}]}}\n\n")
	}))
	defer server.Close()
	adapter := testAdapter(t, server.Client(), server.URL, Credential{AccessToken: "access", RefreshToken: "refresh", AccountID: "acct"})
	response, err := adapter.Generate(context.Background(), protocols.Request{
		Model: "gpt-test", ParallelToolCalls: true,
		Input: []protocols.Item{{Type: "message", Role: "assistant", Phase: "commentary", Content: []protocols.ContentPart{{Type: "output_text", Text: "checking"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 1 || response.Output[0].Phase != "final_answer" {
		t.Fatalf("upstream phase was not preserved: %#v", response.Output)
	}
}

func TestGenerateDoesNotRetryUpstreamFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":{"message":"busy","code":"server_error"}}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	adapter := testAdapter(t, server.Client(), server.URL, Credential{AccessToken: "access", RefreshToken: "refresh", AccountID: "acct"})
	_, err := adapter.Generate(context.Background(), protocols.Request{Model: "gpt-test", ParallelToolCalls: true})
	if err == nil {
		t.Fatal("expected upstream error")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestGeneratePassesCustomToolLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		tool := payload["tools"].([]any)[0].(map[string]any)
		if tool["type"] != "custom" || tool["name"] != "shell" || tool["parameters"] != nil {
			t.Errorf("custom tool was not preserved cleanly: %#v", tool)
		}
		format := tool["format"].(map[string]any)
		if format["type"] != "text" {
			t.Errorf("custom tool format was not preserved: %#v", format)
		}
		input := payload["input"].([]any)
		if input[0].(map[string]any)["type"] != "custom_tool_call_output" {
			t.Errorf("custom tool output was not preserved: %#v", input)
		}
		choice := payload["tool_choice"].(map[string]any)
		if choice["type"] != "custom" || choice["name"] != "shell" {
			t.Errorf("custom tool choice was not preserved: %#v", choice)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"type\":\"custom_tool_call\",\"id\":\"ctc_1\",\"call_id\":\"call_2\",\"name\":\"shell\",\"input\":\"pwd\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":\"gpt-test\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	adapter := testAdapter(t, server.Client(), server.URL, Credential{AccessToken: "access", RefreshToken: "refresh", AccountID: "acct"})
	response, err := adapter.Generate(context.Background(), protocols.Request{
		Model: "gpt-test", ParallelToolCalls: true,
		Input:      []protocols.Item{{Type: "custom_tool_call_output", CallID: "call_1", Output: "done"}},
		Tools:      []protocols.Tool{{Type: "custom", Name: "shell", Format: json.RawMessage(`{"type":"text"}`)}},
		ToolChoice: &protocols.ToolChoice{Mode: "custom", Name: "shell"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 1 || response.Output[0].Type != "custom_tool_call" || response.Output[0].Input != "pwd" {
		t.Fatalf("unexpected custom tool response: %#v", response)
	}
}

func TestReadSSEHandlesFragmentedMultilineData(t *testing.T) {
	reader := strings.NewReader("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\ndata: \"sequence_number\":4,\"delta\":\"hi\"}\n\n")
	var got protocols.StreamEvent
	if err := readSSE(reader, func(event protocols.StreamEvent) error { got = event; return nil }); err != nil {
		t.Fatal(err)
	}
	if got.Type != protocols.EventTextDelta || got.Delta != "hi" || got.SequenceNumber != 4 {
		t.Fatalf("unexpected event: %#v", got)
	}
}

func TestReadSSEHandlesCustomToolInput(t *testing.T) {
	reader := strings.NewReader("event: response.custom_tool_call_input.done\ndata: {\"type\":\"response.custom_tool_call_input.done\",\"sequence_number\":5,\"item_id\":\"ctc_1\",\"input\":\"pwd\"}\n\n")
	var got protocols.StreamEvent
	if err := readSSE(reader, func(event protocols.StreamEvent) error { got = event; return nil }); err != nil {
		t.Fatal(err)
	}
	if got.Type != protocols.EventCustomToolInputDone || got.Input != "pwd" || got.ItemID != "ctc_1" {
		t.Fatalf("unexpected custom tool event: %#v", got)
	}
}

func TestStreamReportsMissingTerminalEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"delta\":\"partial\"}\n\n")
	}))
	defer server.Close()
	adapter := testAdapter(t, server.Client(), server.URL, Credential{AccessToken: "access", RefreshToken: "refresh", AccountID: "acct"})
	results, err := adapter.Stream(context.Background(), protocols.Request{Model: "gpt-test", ParallelToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	var streamErr error
	for result := range results {
		if result.Err != nil {
			streamErr = result.Err
		}
	}
	if streamErr == nil || !strings.Contains(streamErr.Error(), "terminal response") {
		t.Fatalf("stream error = %v", streamErr)
	}
}

func TestStreamPatchesTerminalOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{}\"}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":\"gpt-test\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	adapter := testAdapter(t, server.Client(), server.URL, Credential{AccessToken: "access", RefreshToken: "refresh", AccountID: "acct"})
	results, err := adapter.Stream(context.Background(), protocols.Request{Model: "gpt-test", ParallelToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	var terminal *protocols.StreamEvent
	for result := range results {
		if result.Event != nil && result.Event.Type == protocols.EventResponseCompleted {
			terminal = result.Event
		}
	}
	if terminal == nil || terminal.Response == nil || len(terminal.Response.Output) != 1 || terminal.Response.Output[0].CallID != "call_1" {
		t.Fatalf("terminal event was not patched: %#v", terminal)
	}
}
