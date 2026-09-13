package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mktcz/uniproxy/internal/protocols"
)

type Codec struct{}

func (Codec) Name() string { return "openai" }

func (c Codec) NewStreamEncoder(_ protocols.StreamMeta) protocols.StreamEncoder {
	return streamEncoder{codec: c}
}

type streamEncoder struct{ codec Codec }

func (e streamEncoder) Encode(event protocols.StreamEvent) ([][]byte, error) {
	frame, err := e.codec.EncodeEvent(event)
	if err != nil {
		return nil, err
	}
	if len(frame) == 0 {
		return nil, nil
	}
	return [][]byte{frame}, nil
}

var strippedTopLevel = map[string]struct{}{
	"background": {}, "context_management": {}, "conversation": {}, "include": {},
	"max_tool_calls": {}, "metadata": {}, "previous_response_id": {},
	"prompt": {}, "prompt_cache_retention": {}, "safety_identifier": {}, "service_tier": {},
	"store": {}, "stream_options": {}, "temperature": {}, "text": {}, "top_logprobs": {},
	"top_p": {}, "truncation": {}, "user": {},
}

func (Codec) DecodeRequest(data []byte) (protocols.Request, bool, error) {
	var fields map[string]json.RawMessage
	if err := decodeOne(data, &fields); err != nil {
		return protocols.Request{}, false, clientError("invalid JSON body: "+err.Error(), "", "invalid_json")
	}
	var request protocols.Request
	request.ParallelToolCalls = true
	stream := false
	for key, raw := range fields {
		var err error
		switch key {
		case "model":
			err = json.Unmarshal(raw, &request.Model)
		case "instructions":
			if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				err = json.Unmarshal(raw, &request.Instructions)
			}
		case "input":
			request.Input, err = decodeInput(raw)
		case "tools":
			request.Tools, err = decodeTools(raw)
		case "tool_choice":
			request.ToolChoice, err = decodeToolChoice(raw)
		case "parallel_tool_calls":
			err = json.Unmarshal(raw, &request.ParallelToolCalls)
		case "reasoning":
			request.Reasoning, err = decodeReasoningConfig(raw)
		case "stream":
			err = json.Unmarshal(raw, &stream)
		case "prompt_cache_key":
			err = json.Unmarshal(raw, &request.PromptCacheKey)
		case "max_output_tokens":
			var value int
			err = json.Unmarshal(raw, &value)
			if err == nil && value <= 0 {
				err = fmt.Errorf("must be greater than zero")
			}
			if err == nil {
				request.MaxOutputTokens = &value
			}
		default:
			if _, strip := strippedTopLevel[key]; strip {
				continue
			}
			return protocols.Request{}, false, clientError("unsupported field: "+key, key, "unsupported_field")
		}
		if err != nil {
			return protocols.Request{}, false, clientError("invalid field "+key+": "+err.Error(), key, "invalid_request")
		}
	}
	request.Model = strings.TrimSpace(request.Model)
	if request.Model == "" {
		return protocols.Request{}, false, clientError("model is required", "model", "missing_required_field")
	}
	return request, stream, nil
}

func decodeInput(raw json.RawMessage) ([]protocols.Item, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []protocols.Item{{Type: "message", Role: "user", Content: []protocols.ContentPart{{Type: "input_text", Text: text}}}}, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("must be a string or array")
	}
	result := make([]protocols.Item, 0, len(items))
	for index, itemRaw := range items {
		item, err := decodeItem(itemRaw)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", index, err)
		}
		result = append(result, item)
	}
	return result, nil
}

func decodeItem(raw json.RawMessage) (protocols.Item, error) {
	var fields map[string]json.RawMessage
	if err := decodeOne(raw, &fields); err != nil {
		return protocols.Item{}, err
	}
	itemType, err := optionalString(fields, "type")
	if err != nil {
		return protocols.Item{}, err
	}
	if itemType == "" {
		if _, ok := fields["role"]; ok {
			itemType = "message"
		}
	}
	item := protocols.Item{Type: itemType}
	allowed := map[string]bool{"type": true, "id": true, "status": true}
	switch itemType {
	case "message":
		allowed["role"], allowed["content"], allowed["phase"] = true, true, true
		item.ID, err = optionalString(fields, "id")
		if err == nil {
			item.Status, err = optionalString(fields, "status")
		}
		if err == nil {
			item.Role, err = requiredString(fields, "role")
		}
		if err == nil {
			if item.Role != "system" && item.Role != "developer" && item.Role != "user" && item.Role != "assistant" {
				err = fmt.Errorf("unsupported message role %q", item.Role)
			}
		}
		if err == nil {
			item.Phase, err = optionalString(fields, "phase")
			if err == nil && item.Phase != "" {
				if item.Role != "assistant" {
					err = fmt.Errorf("phase is only supported on assistant messages")
				} else if item.Phase != "commentary" && item.Phase != "final_answer" {
					err = fmt.Errorf("unsupported assistant message phase %q", item.Phase)
				}
			}
		}
		if err == nil {
			content, ok := fields["content"]
			if !ok {
				err = fmt.Errorf("content is required")
			} else {
				item.Content, err = decodeContent(content, item.Role)
			}
		}
	case "function_call":
		for _, key := range []string{"call_id", "name", "arguments"} {
			allowed[key] = true
		}
		item.ID, err = optionalString(fields, "id")
		if err == nil {
			item.Status, err = optionalString(fields, "status")
		}
		if err == nil {
			item.CallID, err = requiredString(fields, "call_id")
		}
		if err == nil {
			item.Name, err = requiredString(fields, "name")
		}
		if err == nil {
			item.Arguments, err = requiredStringAllowEmpty(fields, "arguments")
		}
	case "function_call_output":
		allowed["call_id"], allowed["output"] = true, true
		item.ID, err = optionalString(fields, "id")
		if err == nil {
			item.Status, err = optionalString(fields, "status")
		}
		if err == nil {
			item.CallID, err = requiredString(fields, "call_id")
		}
		if err == nil {
			item.Output, item.OutputContent, err = decodeFunctionCallOutput(fields["output"])
		}
	case "custom_tool_call":
		for _, key := range []string{"call_id", "name", "input"} {
			allowed[key] = true
		}
		item.ID, err = optionalString(fields, "id")
		if err == nil {
			item.Status, err = optionalString(fields, "status")
		}
		if err == nil {
			item.CallID, err = requiredString(fields, "call_id")
		}
		if err == nil {
			item.Name, err = requiredString(fields, "name")
		}
		if err == nil {
			item.Input, err = requiredStringAllowEmpty(fields, "input")
		}
	case "custom_tool_call_output":
		allowed["call_id"], allowed["output"] = true, true
		item.ID, err = optionalString(fields, "id")
		if err == nil {
			item.Status, err = optionalString(fields, "status")
		}
		if err == nil {
			item.CallID, err = requiredString(fields, "call_id")
		}
		if err == nil {
			item.Output, err = requiredStringAllowEmpty(fields, "output")
		}
	case "reasoning":
		allowed["summary"], allowed["encrypted_content"] = true, true
		item.ID, err = optionalString(fields, "id")
		if err == nil {
			item.Status, err = optionalString(fields, "status")
		}
		if err == nil {
			item.EncryptedContent, err = optionalString(fields, "encrypted_content")
		}
		if err == nil {
			item.Summary, err = decodeSummary(fields["summary"])
		}
	default:
		return protocols.Item{}, fmt.Errorf("unsupported item type %q", itemType)
	}
	if err != nil {
		return protocols.Item{}, err
	}
	for key := range fields {
		if !allowed[key] {
			return protocols.Item{}, fmt.Errorf("unsupported field %q on %s item", key, itemType)
		}
	}
	return item, nil
}

func decodeFunctionCallOutput(raw json.RawMessage) (string, []protocols.ContentPart, error) {
	var output string
	if json.Unmarshal(raw, &output) == nil {
		return output, nil, nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return "", nil, fmt.Errorf("output must be a string or array")
	}
	parts := make([]protocols.ContentPart, 0, len(values))
	for index, value := range values {
		part, err := decodeContentPart(value, "user")
		if err != nil {
			return "", nil, fmt.Errorf("output %d: %w", index, err)
		}
		if part.Type == "input_audio" {
			return "", nil, fmt.Errorf("output %d: input_audio is not supported in function call output", index)
		}
		parts = append(parts, part)
	}
	return "", parts, nil
}

func decodeContent(raw json.RawMessage, role string) ([]protocols.ContentPart, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		partType := "input_text"
		if role == "assistant" {
			partType = "output_text"
		}
		return []protocols.ContentPart{{Type: partType, Text: text}}, nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("content must be a string or array")
	}
	parts := make([]protocols.ContentPart, 0, len(values))
	for index, value := range values {
		part, err := decodeContentPart(value, role)
		if err != nil {
			return nil, fmt.Errorf("content %d: %w", index, err)
		}
		parts = append(parts, part)
	}
	return parts, nil
}

func decodeContentPart(raw json.RawMessage, role string) (protocols.ContentPart, error) {
	var fields map[string]json.RawMessage
	if err := decodeOne(raw, &fields); err != nil {
		return protocols.ContentPart{}, err
	}
	partType, err := requiredString(fields, "type")
	if err != nil {
		return protocols.ContentPart{}, err
	}
	switch partType {
	case "input_text", "output_text":
		if role == "assistant" && partType != "output_text" {
			return protocols.ContentPart{}, fmt.Errorf("assistant messages require output_text content")
		}
		if role != "assistant" && partType != "input_text" {
			return protocols.ContentPart{}, fmt.Errorf("%s messages require input_text content", role)
		}
		partText, err := requiredStringAllowEmpty(fields, "text")
		if err != nil {
			return protocols.ContentPart{}, err
		}
		for key := range fields {
			switch key {
			case "type", "text", "annotations", "cache_control", "prompt_cache_breakpoint":
			default:
				return protocols.ContentPart{}, fmt.Errorf("unsupported content field %q", key)
			}
		}
		return protocols.ContentPart{Type: partType, Text: partText}, nil
	case "input_image", "input_audio", "input_file":
		if role == "assistant" {
			return protocols.ContentPart{}, fmt.Errorf("assistant messages do not support %s content", partType)
		}
		return decodeMediaContentPart(partType, fields)
	default:
		return protocols.ContentPart{}, fmt.Errorf("unsupported content type %q", partType)
	}
}

func decodeMediaContentPart(partType string, fields map[string]json.RawMessage) (protocols.ContentPart, error) {
	part := protocols.ContentPart{Type: partType}
	switch partType {
	case "input_image":
		for key := range fields {
			switch key {
			case "type", "image_url", "file_id", "detail", "cache_control", "prompt_cache_breakpoint":
			default:
				return protocols.ContentPart{}, fmt.Errorf("unsupported content field %q", key)
			}
		}
		var err error
		part.ImageURL, err = optionalString(fields, "image_url")
		if err != nil {
			return protocols.ContentPart{}, err
		}
		part.FileID, err = optionalString(fields, "file_id")
		if err != nil {
			return protocols.ContentPart{}, err
		}
		part.Detail, err = optionalString(fields, "detail")
		if err != nil {
			return protocols.ContentPart{}, err
		}
		if part.ImageURL == "" && part.FileID == "" {
			return protocols.ContentPart{}, fmt.Errorf("input_image requires image_url or file_id")
		}
	case "input_audio":
		for key := range fields {
			switch key {
			case "type", "data", "format", "input_audio", "cache_control", "prompt_cache_breakpoint":
			default:
				return protocols.ContentPart{}, fmt.Errorf("unsupported content field %q", key)
			}
		}
		if nested := fields["input_audio"]; len(nested) > 0 {
			var nestedFields map[string]json.RawMessage
			if err := decodeOne(nested, &nestedFields); err != nil {
				return protocols.ContentPart{}, fmt.Errorf("input_audio must be an object")
			}
			for key := range nestedFields {
				if key != "data" && key != "format" {
					return protocols.ContentPart{}, fmt.Errorf("unsupported input_audio field %q", key)
				}
			}
			var err error
			part.AudioData, err = requiredString(nestedFields, "data")
			if err != nil {
				return protocols.ContentPart{}, err
			}
			part.AudioFormat, err = requiredString(nestedFields, "format")
			if err != nil {
				return protocols.ContentPart{}, err
			}
		} else {
			var err error
			part.AudioData, err = requiredString(fields, "data")
			if err != nil {
				return protocols.ContentPart{}, err
			}
			part.AudioFormat, err = requiredString(fields, "format")
			if err != nil {
				return protocols.ContentPart{}, err
			}
		}
	case "input_file":
		for key := range fields {
			switch key {
			case "type", "file_data", "file_url", "file_id", "filename", "cache_control", "prompt_cache_breakpoint":
			default:
				return protocols.ContentPart{}, fmt.Errorf("unsupported content field %q", key)
			}
		}
		var err error
		part.FileData, err = optionalString(fields, "file_data")
		if err != nil {
			return protocols.ContentPart{}, err
		}
		part.FileURL, err = optionalString(fields, "file_url")
		if err != nil {
			return protocols.ContentPart{}, err
		}
		part.FileID, err = optionalString(fields, "file_id")
		if err != nil {
			return protocols.ContentPart{}, err
		}
		part.Filename, err = optionalString(fields, "filename")
		if err != nil {
			return protocols.ContentPart{}, err
		}
		if part.FileData == "" && part.FileURL == "" && part.FileID == "" {
			return protocols.ContentPart{}, fmt.Errorf("input_file requires file_data, file_url, or file_id")
		}
	}
	return part, nil
}

func decodeSummary(raw json.RawMessage) ([]protocols.SummaryPart, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var values []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := strictDecode(raw, &values); err != nil {
		return nil, err
	}
	result := make([]protocols.SummaryPart, 0, len(values))
	for _, value := range values {
		if value.Type != "summary_text" {
			return nil, fmt.Errorf("unsupported summary type %q", value.Type)
		}
		result = append(result, protocols.SummaryPart{Type: value.Type, Text: value.Text})
	}
	return result, nil
}

func decodeTools(raw json.RawMessage) ([]protocols.Tool, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("must be an array")
	}
	result := make([]protocols.Tool, 0, len(values))
	for index, value := range values {
		var fields map[string]json.RawMessage
		if err := decodeOne(value, &fields); err != nil {
			return nil, fmt.Errorf("tool %d: %w", index, err)
		}
		for key := range fields {
			switch key {
			case "type", "name", "description", "parameters", "strict", "cache_control", "format":
			default:
				return nil, fmt.Errorf("tool %d: unsupported field %q", index, key)
			}
		}
		toolType, err := requiredString(fields, "type")
		if err != nil {
			return nil, fmt.Errorf("tool %d: %w", index, err)
		}
		name, err := requiredString(fields, "name")
		if err != nil {
			return nil, fmt.Errorf("tool %d: %w", index, err)
		}
		tool := protocols.Tool{Type: toolType, Name: name}
		tool.Description, err = optionalString(fields, "description")
		if err != nil {
			return nil, fmt.Errorf("tool %d: %w", index, err)
		}
		switch toolType {
		case "function":
			parameters := fields["parameters"]
			if len(parameters) == 0 || !json.Valid(parameters) || bytes.TrimSpace(parameters)[0] != '{' {
				return nil, fmt.Errorf("tool %d: parameters must be a JSON object", index)
			}
			tool.Parameters = bytes.Clone(parameters)
			if strictRaw := fields["strict"]; len(strictRaw) > 0 {
				var strict bool
				if err := json.Unmarshal(strictRaw, &strict); err != nil {
					return nil, fmt.Errorf("tool %d: strict must be boolean", index)
				}
				tool.Strict = &strict
			}
		case "custom":
			if format := fields["format"]; len(format) > 0 {
				if err := validateCustomToolFormat(format); err != nil {
					return nil, fmt.Errorf("tool %d: %w", index, err)
				}
				tool.Format = bytes.Clone(format)
			}
		default:
			return nil, fmt.Errorf("tool %d: unsupported tool type %q", index, toolType)
		}
		result = append(result, tool)
	}
	return result, nil
}

func decodeToolChoice(raw json.RawMessage) (*protocols.ToolChoice, error) {
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		switch mode {
		case "auto", "none", "required":
			return &protocols.ToolChoice{Mode: mode}, nil
		default:
			return nil, fmt.Errorf("unsupported tool choice %q", mode)
		}
	}
	var fields map[string]json.RawMessage
	if err := decodeOne(raw, &fields); err != nil {
		return nil, err
	}
	for key := range fields {
		if key != "type" && key != "name" {
			return nil, fmt.Errorf("unsupported tool_choice field %q", key)
		}
	}
	typeName, err := requiredString(fields, "type")
	if err != nil {
		return nil, err
	}
	if typeName != "function" && typeName != "custom" {
		return nil, fmt.Errorf("unsupported tool choice type %q", typeName)
	}
	name, err := requiredString(fields, "name")
	if err != nil {
		return nil, err
	}
	return &protocols.ToolChoice{Mode: typeName, Name: name}, nil
}

func validateCustomToolFormat(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := decodeOne(raw, &fields); err != nil {
		return fmt.Errorf("format must be an object")
	}
	formatType, err := requiredString(fields, "type")
	if err != nil {
		return fmt.Errorf("invalid format: %w", err)
	}
	for key := range fields {
		if key != "type" && key != "syntax" && key != "definition" {
			return fmt.Errorf("unsupported format field %q", key)
		}
	}
	switch formatType {
	case "text":
		if len(fields) != 1 {
			return fmt.Errorf("text format only accepts type")
		}
	case "grammar":
		syntax, err := requiredString(fields, "syntax")
		if err != nil {
			return fmt.Errorf("invalid grammar format: %w", err)
		}
		if syntax != "lark" && syntax != "regex" {
			return fmt.Errorf("unsupported grammar syntax %q", syntax)
		}
		if _, err := requiredStringAllowEmpty(fields, "definition"); err != nil {
			return fmt.Errorf("invalid grammar format: %w", err)
		}
	default:
		return fmt.Errorf("unsupported custom tool format type %q", formatType)
	}
	return nil
}

func decodeReasoningConfig(raw json.RawMessage) (*protocols.ReasoningConfig, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := decodeOne(raw, &fields); err != nil {
		return nil, err
	}
	for key := range fields {
		if key != "effort" && key != "summary" {
			return nil, fmt.Errorf("unsupported reasoning field %q", key)
		}
	}
	effort, err := optionalString(fields, "effort")
	if err != nil {
		return nil, err
	}
	if effort != "" && !contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, effort) {
		return nil, fmt.Errorf("unsupported reasoning effort %q", effort)
	}
	summary, err := optionalString(fields, "summary")
	if err != nil {
		return nil, err
	}
	if summary != "" && !contains([]string{"auto", "concise", "detailed"}, summary) {
		return nil, fmt.Errorf("unsupported reasoning summary %q", summary)
	}
	return &protocols.ReasoningConfig{Effort: effort, Summary: summary}, nil
}

func (Codec) EncodeResponse(response protocols.Response) ([]byte, error) {
	return json.Marshal(responseValue(response))
}

func (Codec) EncodeEvent(event protocols.StreamEvent) ([]byte, error) {
	wireType, ok := responseEventType(event.Type)
	if !ok {
		return nil, fmt.Errorf("unsupported canonical event type %q", event.Type)
	}
	payload := map[string]any{"type": wireType, "sequence_number": event.SequenceNumber}
	if event.Response != nil {
		payload["response"] = responseValue(*event.Response)
	}
	if event.Item != nil {
		payload["item"] = itemValue(*event.Item)
	}
	if event.Part != nil {
		payload["part"] = contentPartValue(*event.Part)
	}
	if event.OutputIndex != nil {
		payload["output_index"] = *event.OutputIndex
	}
	if event.ContentIndex != nil {
		payload["content_index"] = *event.ContentIndex
	}
	if event.ItemID != "" {
		payload["item_id"] = event.ItemID
	}
	if event.Delta != "" || strings.HasSuffix(string(event.Type), "_delta") {
		payload["delta"] = event.Delta
	}
	if event.Text != "" || event.Type == protocols.EventTextDone || event.Type == protocols.EventReasoningDone || event.Type == protocols.EventReasoningSummaryDone {
		payload["text"] = event.Text
	}
	if event.Arguments != "" || event.Type == protocols.EventToolArgumentsDone {
		payload["arguments"] = event.Arguments
	}
	if event.Input != "" || event.Type == protocols.EventCustomToolInputDone {
		payload["input"] = event.Input
	}
	if event.Error != nil && event.Type == protocols.EventError {
		payload["message"] = event.Error.Message
		payload["code"] = event.Error.Code
		payload["param"] = event.Error.Param
	} else if event.Error != nil {
		payload["error"] = event.Error
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []byte("event: " + wireType + "\ndata: " + string(data) + "\n\n"), nil
}

func responseValue(response protocols.Response) map[string]any {
	object := response.Object
	if object == "" {
		object = "response"
	}
	output := make([]map[string]any, 0, len(response.Output))
	for _, item := range response.Output {
		output = append(output, itemValue(item))
	}
	value := map[string]any{
		"id": response.ID, "object": object, "created_at": response.CreatedAt,
		"status": response.Status, "model": response.Model, "output": output,
	}
	if response.CompletedAt != nil {
		value["completed_at"] = *response.CompletedAt
	}
	if response.Usage != nil {
		value["usage"] = response.Usage
	}
	if response.Error != nil {
		value["error"] = response.Error
	}
	if response.IncompleteDetails != nil {
		value["incomplete_details"] = response.IncompleteDetails
	}
	return value
}

func itemValue(item protocols.Item) map[string]any {
	value := map[string]any{"type": item.Type}
	if item.ID != "" {
		value["id"] = item.ID
	}
	if item.Status != "" {
		value["status"] = item.Status
	}
	switch item.Type {
	case "message":
		content := make([]map[string]any, 0, len(item.Content))
		for _, part := range item.Content {
			content = append(content, contentPartValue(part))
		}
		value["role"], value["content"] = item.Role, content
		if item.Phase != "" {
			value["phase"] = item.Phase
		}
	case "reasoning":
		summary := item.Summary
		if summary == nil {
			summary = []protocols.SummaryPart{}
		}
		value["summary"] = summary
		if item.EncryptedContent != "" {
			value["encrypted_content"] = item.EncryptedContent
		}
	case "function_call":
		value["call_id"], value["name"], value["arguments"] = item.CallID, item.Name, item.Arguments
	case "function_call_output":
		value["call_id"] = item.CallID
		if len(item.OutputContent) > 0 {
			output := make([]map[string]any, 0, len(item.OutputContent))
			for _, part := range item.OutputContent {
				output = append(output, contentPartValue(part))
			}
			value["output"] = output
		} else {
			value["output"] = item.Output
		}
	case "custom_tool_call":
		value["call_id"], value["name"], value["input"] = item.CallID, item.Name, item.Input
	case "custom_tool_call_output":
		value["call_id"], value["output"] = item.CallID, item.Output
	}
	return value
}

func contentPartValue(part protocols.ContentPart) map[string]any {
	value := map[string]any{"type": part.Type}
	switch part.Type {
	case "input_text", "output_text":
		value["text"] = part.Text
		if part.Type == "output_text" {
			annotations := part.Annotations
			if annotations == nil {
				annotations = []json.RawMessage{}
			}
			value["annotations"] = annotations
		}
	case "input_image":
		if part.ImageURL != "" {
			value["image_url"] = part.ImageURL
		}
		if part.FileID != "" {
			value["file_id"] = part.FileID
		}
		if part.Detail != "" {
			value["detail"] = part.Detail
		}
	case "input_audio":
		value["data"] = part.AudioData
		value["format"] = part.AudioFormat
	case "input_file":
		if part.FileData != "" {
			value["file_data"] = part.FileData
		}
		if part.FileURL != "" {
			value["file_url"] = part.FileURL
		}
		if part.FileID != "" {
			value["file_id"] = part.FileID
		}
		if part.Filename != "" {
			value["filename"] = part.Filename
		}
	default:
		if part.Text != "" {
			value["text"] = part.Text
		}
	}
	return value
}

func responseEventType(eventType protocols.EventType) (string, bool) {
	wire, ok := map[protocols.EventType]string{
		protocols.EventResponseCreated:       "response.created",
		protocols.EventResponseQueued:        "response.queued",
		protocols.EventResponseInProgress:    "response.in_progress",
		protocols.EventResponseCompleted:     "response.completed",
		protocols.EventResponseIncomplete:    "response.incomplete",
		protocols.EventResponseFailed:        "response.failed",
		protocols.EventError:                 "error",
		protocols.EventOutputItemAdded:       "response.output_item.added",
		protocols.EventOutputItemDone:        "response.output_item.done",
		protocols.EventContentPartAdded:      "response.content_part.added",
		protocols.EventContentPartDone:       "response.content_part.done",
		protocols.EventTextDelta:             "response.output_text.delta",
		protocols.EventTextDone:              "response.output_text.done",
		protocols.EventReasoningDelta:        "response.reasoning_text.delta",
		protocols.EventReasoningDone:         "response.reasoning_text.done",
		protocols.EventReasoningPartAdded:    "response.reasoning_summary_part.added",
		protocols.EventReasoningPartDone:     "response.reasoning_summary_part.done",
		protocols.EventReasoningSummaryDelta: "response.reasoning_summary_text.delta",
		protocols.EventReasoningSummaryDone:  "response.reasoning_summary_text.done",
		protocols.EventToolArgumentsDelta:    "response.function_call_arguments.delta",
		protocols.EventToolArgumentsDone:     "response.function_call_arguments.done",
		protocols.EventCustomToolInputDelta:  "response.custom_tool_call_input.delta",
		protocols.EventCustomToolInputDone:   "response.custom_tool_call_input.done",
	}[eventType]
	return wire, ok
}

func (Codec) EncodeError(err error) (int, []byte) {
	status := http.StatusBadGateway
	apiErr := protocols.APIError{Message: "upstream request failed", Type: "api_error", Code: "upstream_error"}
	var clientErr *protocols.ClientError
	var upstreamErr *protocols.UpstreamError
	switch {
	case errors.As(err, &clientErr):
		status = clientErr.Status
		apiErr = protocols.APIError{Message: clientErr.Message, Type: "invalid_request_error", Param: clientErr.Param, Code: clientErr.Code}
	case errors.As(err, &upstreamErr):
		status = upstreamErr.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		apiErr = protocols.APIError{Message: upstreamErr.Message, Type: "api_error", Code: upstreamErr.Code}
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
		apiErr.Message = "upstream response timed out"
		apiErr.Code = "upstream_timeout"
	}
	payload, _ := json.Marshal(map[string]any{"error": apiErr})
	return status, payload
}

func decodeOne(data []byte, value any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err == nil {
		return fmt.Errorf("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

func strictDecode(data []byte, value any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	return nil
}

func requiredString(fields map[string]json.RawMessage, key string) (string, error) {
	result, err := requiredStringAllowEmpty(fields, key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(result) == "" {
		return "", fmt.Errorf("%s cannot be empty", key)
	}
	return result, nil
}

func requiredStringAllowEmpty(fields map[string]json.RawMessage, key string) (string, error) {
	raw, ok := fields[key]
	if !ok {
		return "", fmt.Errorf("%s is required", key)
	}
	var result string
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return result, nil
}

func optionalString(fields map[string]json.RawMessage, key string) (string, error) {
	raw, ok := fields[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var result string
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("%s must be a string", key)
	}
	return result, nil
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func clientError(message, param, code string) error {
	return &protocols.ClientError{Status: http.StatusBadRequest, Message: message, Param: param, Code: code}
}
