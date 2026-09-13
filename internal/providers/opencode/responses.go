package opencode

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/mktcz/uniproxy/internal/protocols"
)

func buildResponsesBody(request protocols.Request, wireModel string) ([]byte, error) {
	input := make([]map[string]any, 0, len(request.Input))
	for _, item := range request.Input {
		wire, err := encodeResponsesItem(item)
		if err != nil {
			return nil, err
		}
		input = append(input, wire)
	}
	payload := map[string]any{
		"model":               wireModel,
		"input":               input,
		"stream":              true,
		"store":               false,
		"parallel_tool_calls": request.ParallelToolCalls,
	}
	if request.Instructions != "" {
		payload["instructions"] = request.Instructions
	}
	if len(request.Tools) > 0 {
		payload["tools"] = request.Tools
	}
	if request.ToolChoice != nil {
		if request.ToolChoice.Mode == "function" || request.ToolChoice.Mode == "custom" {
			payload["tool_choice"] = map[string]string{"type": request.ToolChoice.Mode, "name": request.ToolChoice.Name}
		} else {
			payload["tool_choice"] = request.ToolChoice.Mode
		}
	}
	if request.Reasoning != nil {
		payload["reasoning"] = request.Reasoning
	}
	if request.PromptCacheKey != "" {
		payload["prompt_cache_key"] = request.PromptCacheKey
	}
	if request.MaxOutputTokens != nil {
		payload["max_output_tokens"] = *request.MaxOutputTokens
	}
	return json.Marshal(payload)
}

func encodeResponsesItem(item protocols.Item) (map[string]any, error) {
	wire := map[string]any{"type": item.Type}
	if item.ID != "" {
		wire["id"] = item.ID
	}
	if item.Status != "" {
		wire["status"] = item.Status
	}
	switch item.Type {
	case "message":
		role := item.Role
		if role == "system" {
			role = "developer"
		}
		content := make([]map[string]any, 0, len(item.Content))
		for _, part := range item.Content {
			encoded, err := encodeResponsesPart(part)
			if err != nil {
				return nil, err
			}
			content = append(content, encoded)
		}
		wire["role"], wire["content"] = role, content
	case "function_call":
		wire["call_id"], wire["name"], wire["arguments"] = item.CallID, item.Name, item.Arguments
	case "function_call_output":
		wire["call_id"] = item.CallID
		if len(item.OutputContent) > 0 {
			output := make([]map[string]any, 0, len(item.OutputContent))
			for _, part := range item.OutputContent {
				encoded, err := encodeResponsesPart(part)
				if err != nil {
					return nil, err
				}
				output = append(output, encoded)
			}
			wire["output"] = output
		} else {
			wire["output"] = item.Output
		}
	case "custom_tool_call":
		wire["call_id"], wire["name"], wire["input"] = item.CallID, item.Name, item.Input
	case "custom_tool_call_output":
		wire["call_id"], wire["output"] = item.CallID, item.Output
	case "reasoning":
		wire["summary"] = item.Summary
		if item.EncryptedContent != "" {
			wire["encrypted_content"] = item.EncryptedContent
		}
	default:
		return nil, fmt.Errorf("unsupported responses item type %q", item.Type)
	}
	return wire, nil
}

func encodeResponsesPart(part protocols.ContentPart) (map[string]any, error) {
	switch part.Type {
	case "input_text", "output_text":
		return map[string]any{"type": part.Type, "text": part.Text}, nil
	case "input_image":
		wire := map[string]any{"type": "input_image"}
		if part.ImageURL != "" {
			wire["image_url"] = part.ImageURL
		}
		if part.FileID != "" {
			wire["file_id"] = part.FileID
		}
		if part.Detail != "" {
			wire["detail"] = part.Detail
		}
		return wire, nil
	case "input_file":
		wire := map[string]any{"type": "input_file"}
		if part.FileData != "" {
			wire["file_data"] = part.FileData
		}
		if part.FileURL != "" {
			wire["file_url"] = part.FileURL
		}
		if part.FileID != "" {
			wire["file_id"] = part.FileID
		}
		if part.Filename != "" {
			wire["filename"] = part.Filename
		}
		return wire, nil
	default:
		return nil, fmt.Errorf("unsupported responses content part %q", part.Type)
	}
}

func decodeResponsesEvent(payload []byte, eventName string) (protocols.StreamEvent, error) {
	var wire struct {
		Type           string          `json:"type"`
		SequenceNumber int             `json:"sequence_number"`
		Response       json.RawMessage `json:"response"`
		Item           json.RawMessage `json:"item"`
		Part           json.RawMessage `json:"part"`
		OutputIndex    *int            `json:"output_index"`
		ContentIndex   *int            `json:"content_index"`
		ItemID         string          `json:"item_id"`
		Delta          string          `json:"delta"`
		Text           string          `json:"text"`
		Arguments      string          `json:"arguments"`
		Input          string          `json:"input"`
		Error          json.RawMessage `json:"error"`
		Message        string          `json:"message"`
		Code           json.RawMessage `json:"code"`
		Param          string          `json:"param"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return protocols.StreamEvent{}, fmt.Errorf("decode responses SSE event: %w", err)
	}
	if wire.Type == "" {
		wire.Type = eventName
	}
	if wire.Type == "" {
		return protocols.StreamEvent{}, fmt.Errorf("responses SSE event has no type")
	}
	eventType, ok := responsesEventType(wire.Type)
	if !ok {
		return protocols.StreamEvent{}, nil
	}
	event := protocols.StreamEvent{
		Type: eventType, SequenceNumber: wire.SequenceNumber, OutputIndex: wire.OutputIndex,
		ContentIndex: wire.ContentIndex, ItemID: wire.ItemID, Delta: wire.Delta,
		Text: wire.Text, Arguments: wire.Arguments, Input: wire.Input,
	}
	if len(wire.Response) > 0 && !bytes.Equal(wire.Response, []byte("null")) {
		var response protocols.Response
		if err := json.Unmarshal(wire.Response, &response); err != nil {
			return protocols.StreamEvent{}, fmt.Errorf("decode responses payload: %w", err)
		}
		event.Response = &response
	}
	if len(wire.Item) > 0 && !bytes.Equal(wire.Item, []byte("null")) {
		var item protocols.Item
		if err := json.Unmarshal(wire.Item, &item); err != nil {
			return protocols.StreamEvent{}, fmt.Errorf("decode responses item: %w", err)
		}
		event.Item = &item
	}
	if len(wire.Part) > 0 && !bytes.Equal(wire.Part, []byte("null")) {
		var part protocols.ContentPart
		if err := json.Unmarshal(wire.Part, &part); err != nil {
			return protocols.StreamEvent{}, fmt.Errorf("decode responses part: %w", err)
		}
		event.Part = &part
	}
	if len(wire.Error) > 0 && !bytes.Equal(wire.Error, []byte("null")) {
		var apiErr protocols.APIError
		if err := json.Unmarshal(wire.Error, &apiErr); err == nil {
			event.Error = &apiErr
		}
	}
	if event.Error == nil && wire.Type == "error" {
		code := ""
		if len(wire.Code) > 0 {
			_ = json.Unmarshal(wire.Code, &code)
		}
		event.Error = &protocols.APIError{Message: wire.Message, Type: "api_error", Param: wire.Param, Code: code}
	}
	return event, nil
}

func responsesEventType(eventType string) (protocols.EventType, bool) {
	m := map[string]protocols.EventType{
		"response.created":                       protocols.EventResponseCreated,
		"response.queued":                        protocols.EventResponseQueued,
		"response.in_progress":                   protocols.EventResponseInProgress,
		"response.completed":                     protocols.EventResponseCompleted,
		"response.incomplete":                    protocols.EventResponseIncomplete,
		"response.failed":                        protocols.EventResponseFailed,
		"error":                                  protocols.EventError,
		"response.output_item.added":             protocols.EventOutputItemAdded,
		"response.output_item.done":              protocols.EventOutputItemDone,
		"response.content_part.added":            protocols.EventContentPartAdded,
		"response.content_part.done":             protocols.EventContentPartDone,
		"response.output_text.delta":             protocols.EventTextDelta,
		"response.output_text.done":              protocols.EventTextDone,
		"response.reasoning_text.delta":          protocols.EventReasoningDelta,
		"response.reasoning_text.done":           protocols.EventReasoningDone,
		"response.reasoning_summary_part.added":  protocols.EventReasoningPartAdded,
		"response.reasoning_summary_part.done":   protocols.EventReasoningPartDone,
		"response.reasoning_summary_text.delta":  protocols.EventReasoningSummaryDelta,
		"response.reasoning_summary_text.done":   protocols.EventReasoningSummaryDone,
		"response.function_call_arguments.delta": protocols.EventToolArgumentsDelta,
		"response.function_call_arguments.done":  protocols.EventToolArgumentsDone,
		"response.custom_tool_call_input.delta":  protocols.EventCustomToolInputDelta,
		"response.custom_tool_call_input.done":   protocols.EventCustomToolInputDone,
	}
	v, ok := m[eventType]
	return v, ok
}
