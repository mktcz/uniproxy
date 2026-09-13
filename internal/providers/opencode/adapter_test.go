package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mktcz/uniproxy/internal/protocols"
)

func testAdapter(t *testing.T, client *http.Client) *Adapter {
	t.Helper()
	adapter := New(filepath.Join(t.TempDir(), "auth.json"), client)
	adapter.seedCatalog(
		map[string]modelMeta{
			"big-pickle":            {ID: "big-pickle", Plan: PlanZen, Protocol: ProtocolChat, AllowAnonymous: true},
			"nemotron-3-ultra-free": {ID: "nemotron-3-ultra-free", Plan: PlanZen, Protocol: ProtocolChat, AllowAnonymous: true},
			"gpt-5.5":               {ID: "gpt-5.5", Plan: PlanZen, Protocol: ProtocolResponses},
			"claude-sonnet-4-6":     {ID: "claude-sonnet-4-6", Plan: PlanZen, Protocol: ProtocolMessages, NPM: "@ai-sdk/anthropic"},
			"gemini-3-pro":          {ID: "gemini-3-pro", Plan: PlanZen, Protocol: ProtocolGemini, NPM: "@ai-sdk/google"},
		},
		map[string]modelMeta{
			"kimi-k3": {ID: "kimi-k3", Plan: PlanGo, Protocol: ProtocolChat},
		},
		[]protocols.ModelInfo{
			{ID: "opencode/big-pickle", OwnedBy: "opencode"},
			{ID: "opencode-go/kimi-k3", OwnedBy: "opencode-go"},
		},
	)
	return adapter
}

func TestResolveRoutePrefixes(t *testing.T) {
	adapter := testAdapter(t, http.DefaultClient)
	zen := adapter.resolveRoute("opencode/big-pickle")
	if zen.Plan != PlanZen || zen.WireModel != "big-pickle" || zen.Protocol != ProtocolChat || !zen.AllowAnonymous {
		t.Fatalf("zen route: %+v", zen)
	}
	goRoute := adapter.resolveRoute("opencode-go/kimi-k3")
	if goRoute.Plan != PlanGo || goRoute.WireModel != "kimi-k3" || goRoute.Protocol != ProtocolChat {
		t.Fatalf("go route: %+v", goRoute)
	}
	responses := adapter.resolveRoute("opencode/gpt-5.5")
	if responses.Protocol != ProtocolResponses {
		t.Fatalf("responses protocol: %+v", responses)
	}
	messages := adapter.resolveRoute("opencode/claude-sonnet-4-6")
	if messages.Protocol != ProtocolMessages {
		t.Fatalf("messages protocol: %+v", messages)
	}
	gemini := adapter.resolveRoute("opencode/gemini-3-pro")
	if gemini.Protocol != ProtocolGemini {
		t.Fatalf("gemini protocol: %+v", gemini)
	}
}

func TestFreeChatRequiresSessionAndPublicKey(t *testing.T) {
	var gotAuth, gotSession, gotUA, gotPath string
	var gotMaxTokens float64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("x-opencode-session")
		gotUA = r.Header.Get("User-Agent")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "big-pickle" {
			t.Fatalf("wire model = %#v", payload["model"])
		}
		gotMaxTokens, _ = payload["max_tokens"].(float64)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chat_1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"cost\":\"0\"}\n\n")
	}))
	defer server.Close()

	adapter := testAdapter(t, server.Client())
	adapter.SetBases(server.URL, server.URL+"/go", server.URL+"/go-root")

	resp, err := adapter.Generate(context.Background(), protocols.Request{
		Model:           "opencode/big-pickle",
		PromptCacheKey:  "sess-free-1",
		MaxOutputTokens: intPtr(1234),
		Input: []protocols.Item{{
			Type: "message", Role: "user",
			Content: []protocols.ContentPart{{Type: "input_text", Text: "hello"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer public" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if gotSession != "sess-free-1" {
		t.Fatalf("session = %q", gotSession)
	}
	if gotUA != UserAgent {
		t.Fatalf("ua = %q, want %q", gotUA, UserAgent)
	}
	if !strings.HasSuffix(gotPath, "/chat/completions") {
		t.Fatalf("path = %s", gotPath)
	}
	if gotMaxTokens != 1234 {
		t.Fatalf("max_tokens = %v", gotMaxTokens)
	}
	if len(resp.Output) == 0 || resp.Output[0].Content[0].Text != "hi" {
		t.Fatalf("response: %#v", resp)
	}
}

func TestChatRejectsTruncatedStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chat_cut\",\"choices\":[{\"delta\":{\"content\":\"**Debug\"}}]}\n\n")
	}))
	defer server.Close()

	adapter := testAdapter(t, server.Client())
	adapter.SetBases(server.URL, server.URL+"/go", server.URL+"/go-root")
	_, err := adapter.Generate(context.Background(), protocols.Request{
		Model: "opencode/big-pickle",
		Input: []protocols.Item{{Type: "message", Role: "user", Content: []protocols.ContentPart{{Type: "input_text", Text: "hi"}}}},
	})
	upstream, ok := err.(*protocols.UpstreamError)
	if !ok || upstream.Code != "incomplete_stream" {
		t.Fatalf("error = %#v", err)
	}
}

func TestChatPreservesLengthFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chat_length\",\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"length\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	adapter := testAdapter(t, server.Client())
	adapter.SetBases(server.URL, server.URL+"/go", server.URL+"/go-root")
	response, err := adapter.Generate(context.Background(), protocols.Request{
		Model: "opencode/big-pickle",
		Input: []protocols.Item{{Type: "message", Role: "user", Content: []protocols.ContentPart{{Type: "input_text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "incomplete" || response.IncompleteDetails == nil || response.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("response = %#v", response)
	}
}

func TestGoRequiresAPIKey(t *testing.T) {
	adapter := testAdapter(t, http.DefaultClient)
	_, err := adapter.Generate(context.Background(), protocols.Request{
		Model: "opencode-go/kimi-k3",
		Input: []protocols.Item{{Type: "message", Role: "user", Content: []protocols.ContentPart{{Type: "input_text", Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("expected missing api key error")
	}
	upstream, ok := err.(*protocols.UpstreamError)
	if !ok || upstream.Code != "missing_api_key" {
		t.Fatalf("error = %#v", err)
	}
}

func TestPaidResponsesWithAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/responses" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":\"gpt-5.5\",\"output\":[]}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	adapter := testAdapter(t, server.Client())
	adapter.SetBases(server.URL, server.URL+"/go", server.URL+"/go-root")
	if err := adapter.save(Credential{APIKey: "sk-test"}); err != nil {
		t.Fatal(err)
	}

	resp, err := adapter.Generate(context.Background(), protocols.Request{
		Model: "opencode/gpt-5.5",
		Input: []protocols.Item{{Type: "message", Role: "user", Content: []protocols.ContentPart{{Type: "input_text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "resp_1" || len(resp.Output) != 1 || resp.Output[0].Content[0].Text != "ok" {
		t.Fatalf("response: %#v", resp)
	}
}

func TestAnthropicMessagesStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-test" {
			t.Errorf("x-api-key = %q", r.Header.Get("x-api-key"))
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_a\",\"model\":\"claude-sonnet-4-6\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hey\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()

	adapter := testAdapter(t, server.Client())
	adapter.SetBases(server.URL+"/v1", server.URL+"/go/v1", server.URL)
	if err := adapter.save(Credential{APIKey: "sk-test"}); err != nil {
		t.Fatal(err)
	}

	resp, err := adapter.Generate(context.Background(), protocols.Request{
		Model: "opencode/claude-sonnet-4-6",
		Input: []protocols.Item{{Type: "message", Role: "user", Content: []protocols.ContentPart{{Type: "input_text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 1 || resp.Output[0].Content[0].Text != "hey" {
		t.Fatalf("response: %#v", resp)
	}
}

func TestAnthropicMessagesRejectsTruncatedStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_cut\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"cut\"}}\n\n")
	}))
	defer server.Close()

	adapter := testAdapter(t, server.Client())
	adapter.SetBases(server.URL+"/v1", server.URL+"/go/v1", server.URL)
	if err := adapter.save(Credential{APIKey: "sk-test"}); err != nil {
		t.Fatal(err)
	}
	_, err := adapter.Generate(context.Background(), protocols.Request{
		Model: "opencode/claude-sonnet-4-6",
		Input: []protocols.Item{{Type: "message", Role: "user", Content: []protocols.ContentPart{{Type: "input_text", Text: "hi"}}}},
	})
	upstream, ok := err.(*protocols.UpstreamError)
	if !ok || upstream.Code != "incomplete_stream" {
		t.Fatalf("error = %#v", err)
	}
}

func TestGeminiStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "sk-test" {
			t.Errorf("goog key = %q", r.Header.Get("x-goog-api-key"))
		}
		if !strings.Contains(r.URL.Path, "/models/gemini-3-pro:streamGenerateContent") {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"gem\"}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	adapter := testAdapter(t, server.Client())
	adapter.SetBases(server.URL, server.URL+"/go", server.URL+"/go-root")
	if err := adapter.save(Credential{APIKey: "sk-test"}); err != nil {
		t.Fatal(err)
	}

	resp, err := adapter.Generate(context.Background(), protocols.Request{
		Model: "opencode/gemini-3-pro",
		Input: []protocols.Item{{Type: "message", Role: "user", Content: []protocols.ContentPart{{Type: "input_text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Output) != 1 || resp.Output[0].Content[0].Text != "gem" {
		t.Fatalf("response: %#v", resp)
	}
}

func TestLoginFromPersistsCredential(t *testing.T) {
	adapter := testAdapter(t, http.DefaultClient)
	cred, err := adapter.LoginFrom(context.Background(), strings.NewReader("sk-login-key\n"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cred.APIKey != "sk-login-key" {
		t.Fatalf("cred = %#v", cred)
	}
	loaded, err := adapter.loadOptional()
	if err != nil || loaded == nil || loaded.APIKey != "sk-login-key" {
		t.Fatalf("loaded = %#v err=%v", loaded, err)
	}
}

func TestPaidModelWithoutKeyFails(t *testing.T) {
	adapter := testAdapter(t, http.DefaultClient)
	_, err := adapter.Generate(context.Background(), protocols.Request{
		Model: "opencode/gpt-5.5",
		Input: []protocols.Item{{Type: "message", Role: "user", Content: []protocols.ContentPart{{Type: "input_text", Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}
