package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mktcz/uniproxy/internal/protocols"
)

func TestDecodeTypedFunctionLoopAndStripUnsupported(t *testing.T) {
	body := `{
      "model":"gpt-test","instructions":"help","stream":true,
      "temperature":0.5,"metadata":{"harness":"other"},"store":true,"max_output_tokens":4096,
      "reasoning":{"effort":"high","summary":"auto"},
      "input":[
        {"type":"message","role":"system","content":[{"type":"input_text","text":"rules","cache_control":{"type":"ephemeral"}}]},
        {"type":"function_call","id":"call-item","call_id":"call_1","name":"lookup","arguments":"{}"},
        {"type":"function_call_output","call_id":"call_1","output":""},
        {"type":"reasoning","id":"reasoning-1","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"opaque"}
      ],
      "tools":[{"type":"function","name":"lookup","description":"Lookup","parameters":{"type":"object"},"strict":true,"cache_control":{}}],
      "tool_choice":{"type":"function","name":"lookup"}
    }`
	request, stream, err := (Codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !stream || request.Model != "gpt-test" || len(request.Input) != 4 || len(request.Tools) != 1 {
		t.Fatalf("unexpected request: %#v", request)
	}
	if request.Input[0].Role != "system" || request.Input[2].Output != "" {
		t.Fatalf("typed items were not preserved: %#v", request.Input)
	}
	if request.ToolChoice == nil || request.ToolChoice.Name != "lookup" || request.Reasoning.Effort != "high" {
		t.Fatalf("tool/reasoning configuration missing: %#v %#v", request.ToolChoice, request.Reasoning)
	}
	if request.MaxOutputTokens == nil || *request.MaxOutputTokens != 4096 {
		t.Fatalf("max_output_tokens was not preserved: %#v", request.MaxOutputTokens)
	}
}

func TestDecodeMultimodalContent(t *testing.T) {
	body := `{
      "model":"gpt-test",
      "input":[{
        "role":"user",
        "content":[
          {"type":"input_text","text":"describe"},
          {"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"high"},
          {"type":"input_audio","data":"SUQz","format":"mp3"},
          {"type":"input_file","file_data":"data:application/pdf;base64,JVBE","filename":"doc.pdf"},
          {"type":"input_audio","input_audio":{"data":"QUFB","format":"wav"}}
        ]
      }]
    }`
	request, _, err := (Codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	parts := request.Input[0].Content
	if len(parts) != 5 {
		t.Fatalf("expected 5 parts, got %d", len(parts))
	}
	if parts[1].Type != "input_image" || parts[1].ImageURL == "" || parts[1].Detail != "high" {
		t.Fatalf("image part: %#v", parts[1])
	}
	if parts[2].AudioData != "SUQz" || parts[2].AudioFormat != "mp3" {
		t.Fatalf("flat audio part: %#v", parts[2])
	}
	if parts[3].Filename != "doc.pdf" || parts[3].FileData == "" {
		t.Fatalf("file part: %#v", parts[3])
	}
	if parts[4].AudioData != "QUFB" || parts[4].AudioFormat != "wav" {
		t.Fatalf("nested audio part: %#v", parts[4])
	}
	encoded, err := (Codec{}).EncodeResponse(protocols.Response{Output: request.Input})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"type":"input_image"`) || !strings.Contains(string(encoded), `"type":"input_file"`) {
		t.Fatalf("multimodal parts not encoded: %s", encoded)
	}
}

func TestDecodeAndEncodeMultimodalFunctionCallOutput(t *testing.T) {
	body := `{
      "model":"gpt-test",
      "input":[{
        "type":"function_call_output",
        "call_id":"call_1",
        "output":[
          {"type":"input_text","text":"screenshot"},
          {"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"high"},
          {"type":"input_file","file_data":"data:application/pdf;base64,JVBE","filename":"doc.pdf"}
        ]
      }]
    }`
	request, _, err := (Codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 1 || len(request.Input[0].OutputContent) != 3 {
		t.Fatalf("unexpected function output: %#v", request.Input)
	}
	if request.Input[0].OutputContent[1].ImageURL != "data:image/png;base64,AA==" {
		t.Fatalf("image output was not preserved: %#v", request.Input[0].OutputContent[1])
	}
	encoded, err := (Codec{}).EncodeResponse(protocols.Response{Output: request.Input})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"output":[{"text":"screenshot","type":"input_text"}`) ||
		!strings.Contains(string(encoded), `"type":"input_image"`) {
		t.Fatalf("multimodal function output not encoded: %s", encoded)
	}
}

func TestDecodeRejectsEmptyMedia(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-test","input":[{"role":"user","content":[{"type":"input_image"}]}]}`,
		`{"model":"gpt-test","input":[{"role":"user","content":[{"type":"input_file"}]}]}`,
		`{"model":"gpt-test","input":[{"role":"assistant","content":[{"type":"input_image","image_url":"http://x"}]}]}`,
	} {
		if _, _, err := (Codec{}).DecodeRequest([]byte(body)); err == nil {
			t.Fatalf("expected rejection for %s", body)
		}
	}
}

func TestDecodeStripsUnsupportedToolFormat(t *testing.T) {
	body := `{
      "model":"gpt-test",
      "input":"hi",
      "tools":[{"type":"function","name":"lookup","parameters":{"type":"object"},"format":{"type":"text"}}]
    }`
	request, _, err := (Codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Tools) != 1 || request.Tools[0].Name != "lookup" {
		t.Fatalf("unexpected tools: %#v", request.Tools)
	}
}

func TestDecodeCustomToolLifecycle(t *testing.T) {
	body := `{
      "model":"gpt-test","stream":true,
      "input":[
        {"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"shell","input":"pwd"},
        {"type":"custom_tool_call_output","call_id":"call_1","output":"/workspace"}
      ],
      "tools":[{
        "type":"custom","name":"shell","description":"Run shell input",
        "format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}
      }],
      "tool_choice":{"type":"custom","name":"shell"}
    }`
	request, stream, err := (Codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !stream || len(request.Tools) != 1 || request.Tools[0].Type != "custom" {
		t.Fatalf("unexpected custom tool request: %#v", request)
	}
	if string(request.Tools[0].Format) == "" || request.ToolChoice == nil || request.ToolChoice.Mode != "custom" {
		t.Fatalf("custom tool configuration missing: %#v %#v", request.Tools[0], request.ToolChoice)
	}
	if request.Input[0].Input != "pwd" || request.Input[1].Output != "/workspace" {
		t.Fatalf("custom tool history missing: %#v", request.Input)
	}
}

func TestEncodeCustomToolEvents(t *testing.T) {
	encoded, err := (Codec{}).EncodeEvent(protocols.StreamEvent{
		Type: protocols.EventCustomToolInputDone, SequenceNumber: 4, ItemID: "ctc_1", Input: "pwd",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "event: response.custom_tool_call_input.done") || !strings.Contains(string(encoded), `"input":"pwd"`) {
		t.Fatalf("unexpected custom input event: %s", encoded)
	}

	response, err := (Codec{}).EncodeResponse(protocols.Response{Output: []protocols.Item{{
		Type: "custom_tool_call", ID: "ctc_1", CallID: "call_1", Name: "shell", Input: "pwd",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(response), `"type":"custom_tool_call"`) || !strings.Contains(string(response), `"input":"pwd"`) {
		t.Fatalf("unexpected custom tool response: %s", response)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-test","mystery":true}`,
		`{"model":"gpt-test","input":[{"role":"user","content":"hi","mystery":true}]}`,
	} {
		if _, _, err := (Codec{}).DecodeRequest([]byte(body)); err == nil {
			t.Fatalf("expected rejection for %s", body)
		}
	}
}

func TestDecodeAndEncodeAssistantMessagePhase(t *testing.T) {
	body := `{
      "model":"gpt-test",
      "input":[
        {"type":"message","id":"msg_1","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Checking"}]},
        {"type":"message","id":"msg_2","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Done"}]}
      ]
    }`
	request, _, err := (Codec{}).DecodeRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if request.Input[0].Phase != "commentary" || request.Input[1].Phase != "final_answer" {
		t.Fatalf("message phases were not preserved: %#v", request.Input)
	}
	encoded, err := (Codec{}).EncodeResponse(protocols.Response{Output: request.Input})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"phase":"commentary"`) || !strings.Contains(string(encoded), `"phase":"final_answer"`) {
		t.Fatalf("message phases were not encoded: %s", encoded)
	}
}

func TestDecodeRejectsInvalidMessagePhase(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-test","input":[{"role":"user","phase":"commentary","content":"hi"}]}`,
		`{"model":"gpt-test","input":[{"role":"assistant","phase":"analysis","content":"hi"}]}`,
	} {
		if _, _, err := (Codec{}).DecodeRequest([]byte(body)); err == nil {
			t.Fatalf("expected invalid phase rejection for %s", body)
		}
	}
}

func TestEncodeErrorEventUsesTopLevelErrorFields(t *testing.T) {
	encoded, err := (Codec{}).EncodeEvent(protocols.StreamEvent{
		Type: protocols.EventError, SequenceNumber: 3,
		Error: &protocols.APIError{Message: "broken", Code: "bad", Param: "input"},
	})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(encoded), "data: ")
	if len(parts) != 2 {
		t.Fatalf("unexpected SSE: %s", encoded)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(parts[1])), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["message"] != "broken" || payload["code"] != "bad" || payload["error"] != nil {
		t.Fatalf("unexpected error event: %#v", payload)
	}
}
