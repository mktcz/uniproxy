package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mktcz/uniproxy/internal/protocols"
)

func TestDecodeMessagesRequest(t *testing.T) {
	body := `{
      "model":"gpt-5.6-luna",
      "max_tokens":1024,
      "stream":true,
      "system":[{"type":"text","text":"be helpful"}],
      "messages":[
        {"role":"user","content":[
          {"type":"text","text":"look"},
          {"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}},
          {"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBE"}}
        ]},
        {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}]},
        {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
      ],
      "tools":[{"name":"lookup","description":"Lookup","input_schema":{"type":"object"}}],
      "tool_choice":{"type":"auto"},
      "thinking":{"type":"enabled","budget_tokens":2000}
    }`
	request, stream, err := (Codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !stream || request.Model != "gpt-5.6-luna" || request.Instructions != "be helpful" {
		t.Fatalf("unexpected request meta: %#v", request)
	}
	if request.MaxOutputTokens == nil || *request.MaxOutputTokens != 1024 {
		t.Fatalf("max_tokens was not preserved: %#v", request.MaxOutputTokens)
	}
	if len(request.Input) < 3 || request.Input[0].Content[1].Type != "input_image" {
		t.Fatalf("unexpected input: %#v", request.Input)
	}
	if len(request.Tools) != 1 || request.ToolChoice == nil || request.Reasoning == nil {
		t.Fatalf("tools/reasoning missing: %#v %#v %#v", request.Tools, request.ToolChoice, request.Reasoning)
	}
}

func TestDecodeMultimodalToolResult(t *testing.T) {
	body := `{
      "model":"gpt-5.6-luna",
      "max_tokens":1024,
      "messages":[
        {"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"image.png"}}]},
        {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[
          {"type":"text","text":"image/png"},
          {"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}
        ]}]}
      ]
    }`
	request, _, err := (Codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 2 || len(request.Input[1].OutputContent) != 2 {
		t.Fatalf("unexpected decoded items: %#v", request.Input)
	}
	parts := request.Input[1].OutputContent
	if parts[0].Type != "input_text" || parts[0].Text != "image/png" ||
		parts[1].Type != "input_image" || parts[1].ImageURL != "data:image/png;base64,AA==" {
		t.Fatalf("multimodal tool result was not preserved: %#v", parts)
	}
}

func TestDecodeClaudeCodeCompatibilityFields(t *testing.T) {
	body := `{
      "model":"opencode/big-pickle",
      "max_tokens":8192,
      "thinking":{"type":"adaptive","display":"omitted"},
      "messages":[
        {"role":"user","content":"hola"},
        {"role":"system","content":[{"type":"text","text":"runtime reminder"}]}
      ]
    }`
	request, _, err := (Codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if request.Reasoning == nil || request.Reasoning.Effort != "high" {
		t.Fatalf("thinking was not decoded: %#v", request.Reasoning)
	}
	if len(request.Input) != 2 || request.Input[1].Role != "system" || request.Input[1].Content[0].Text != "runtime reminder" {
		t.Fatalf("system message was not preserved: %#v", request.Input)
	}
}

func TestDecodeRejectsInvalidMaxTokens(t *testing.T) {
	_, _, err := (Codec{}).DecodeRequest([]byte(`{"model":"gpt-test","max_tokens":0,"messages":[{"role":"user","content":"hi"}]}`))
	if err == nil || !strings.Contains(err.Error(), "greater than zero") {
		t.Fatalf("error = %v", err)
	}
}

func TestEncodeResponseAndCountTokens(t *testing.T) {
	encoded, err := (Codec{}).EncodeResponse(protocols.Response{
		ID: "resp_1", Model: "gpt-5.6-luna", Status: "completed",
		Output: []protocols.Item{
			{Type: "message", Role: "assistant", Content: []protocols.ContentPart{{Type: "output_text", Text: "hi"}}},
			{Type: "function_call", CallID: "call_1", Name: "lookup", Arguments: `{"q":"x"}`},
		},
		Usage: &protocols.Usage{InputTokens: 3, OutputTokens: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"stop_reason":"tool_use"`) || !strings.Contains(string(encoded), `"type":"tool_use"`) {
		t.Fatalf("unexpected response: %s", encoded)
	}
	count, err := (Codec{}).CountTokens([]byte(`{"model":"gpt-5.6-luna","messages":[{"role":"user","content":"hello world"}]}`))
	if err != nil || count < 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestStreamEncoderEmitsAnthropicEvents(t *testing.T) {
	encoder := NewStreamEncoder("gpt-5.6-luna")
	var all []byte
	appendFrames := func(event protocols.StreamEvent) {
		frames, err := encoder.Encode(event)
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range frames {
			all = append(all, frame...)
		}
	}
	appendFrames(protocols.StreamEvent{Type: protocols.EventResponseCreated, Response: &protocols.Response{ID: "resp_1", Model: "gpt-5.6-luna"}})
	appendFrames(protocols.StreamEvent{Type: protocols.EventTextDelta, Delta: "hi"})
	appendFrames(protocols.StreamEvent{Type: protocols.EventTextDone, Text: "hi"})
	appendFrames(protocols.StreamEvent{Type: protocols.EventResponseCompleted, Response: &protocols.Response{
		ID: "resp_1", Model: "gpt-5.6-luna", Status: "completed",
		Usage:  &protocols.Usage{InputTokens: 1, OutputTokens: 1},
		Output: []protocols.Item{{Type: "message", Content: []protocols.ContentPart{{Type: "output_text", Text: "hi"}}}},
	}})
	body := string(all)
	for _, want := range []string{"event: message_start", "event: content_block_start", "event: content_block_delta", "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	var payload map[string]any
	for _, chunk := range strings.Split(body, "\n\n") {
		if strings.HasPrefix(chunk, "event: message_delta\n") {
			data := strings.TrimPrefix(strings.SplitN(chunk, "\n", 2)[1], "data: ")
			_ = json.Unmarshal([]byte(data), &payload)
		}
	}
	if payload["type"] != "message_delta" {
		t.Fatalf("missing message_delta payload: %#v", payload)
	}
}
