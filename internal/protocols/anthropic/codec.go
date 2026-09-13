package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/mktcz/uniproxy/internal/protocols"
)

type Codec struct{}

func (Codec) Name() string { return "anthropic" }

func (Codec) NewStreamEncoder(meta protocols.StreamMeta) protocols.StreamEncoder {
	return NewStreamEncoder(meta.Model)
}

var strippedTopLevel = map[string]struct{}{
	"metadata": {}, "stop_sequences": {}, "temperature": {},
	"top_k": {}, "top_p": {}, "service_tier": {}, "container": {}, "mcp_servers": {},
	"context_management": {}, "output_config": {}, "inference_geo": {},
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
		case "system":
			request.Instructions, err = decodeSystem(raw)
		case "messages":
			request.Input, err = decodeMessages(raw)
		case "tools":
			request.Tools, err = decodeTools(raw)
		case "tool_choice":
			request.ToolChoice, err = decodeToolChoice(raw)
		case "stream":
			err = json.Unmarshal(raw, &stream)
		case "thinking":
			request.Reasoning, err = decodeThinking(raw)
		case "max_tokens":
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
	if len(request.Input) == 0 {
		return protocols.Request{}, false, clientError("messages is required", "messages", "missing_required_field")
	}
	return request, stream, nil
}

func (c Codec) CountTokens(data []byte) (int, error) {
	request, _, err := c.DecodeRequest(data)
	if err != nil {
		return 0, err
	}
	return estimateTokens(request), nil
}

func (Codec) EncodeResponse(response protocols.Response) ([]byte, error) {
	message := map[string]any{
		"id": response.ID, "type": "message", "role": "assistant", "model": response.Model,
		"content": contentBlocksFromOutput(response.Output), "stop_reason": stopReason(response),
		"stop_sequence": nil, "usage": usageValue(response.Usage),
	}
	if message["id"] == "" {
		message["id"] = "msg_uniproxy"
	}
	return json.Marshal(message)
}

func (Codec) EncodeError(err error) (int, []byte) {
	status := http.StatusBadGateway
	apiErr := map[string]any{"type": "api_error", "message": "upstream request failed"}
	var clientErr *protocols.ClientError
	var upstreamErr *protocols.UpstreamError
	switch {
	case errors.As(err, &clientErr):
		status = clientErr.Status
		apiErr = map[string]any{"type": "invalid_request_error", "message": clientErr.Message}
	case errors.As(err, &upstreamErr):
		status = upstreamErr.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		apiErr = map[string]any{"type": "api_error", "message": upstreamErr.Message}
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
		apiErr = map[string]any{"type": "api_error", "message": "upstream response timed out"}
	}
	payload, _ := json.Marshal(map[string]any{"type": "error", "error": apiErr})
	return status, payload
}

func decodeSystem(raw json.RawMessage) (string, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("must be a string or array")
	}
	parts := make([]string, 0, len(blocks))
	for index, block := range blocks {
		var fields map[string]json.RawMessage
		if err := decodeOne(block, &fields); err != nil {
			return "", fmt.Errorf("block %d: %w", index, err)
		}
		blockType, err := optionalString(fields, "type")
		if err != nil {
			return "", err
		}
		if blockType != "" && blockType != "text" {
			return "", fmt.Errorf("unsupported system block type %q", blockType)
		}
		value, err := requiredStringAllowEmpty(fields, "text")
		if err != nil {
			return "", err
		}
		if value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, "\n"), nil
}

func decodeMessages(raw json.RawMessage) ([]protocols.Item, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("must be an array")
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("must not be empty")
	}
	result := make([]protocols.Item, 0, len(values))
	for index, value := range values {
		items, err := decodeMessage(value)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		result = append(result, items...)
	}
	return result, nil
}

func decodeMessage(raw json.RawMessage) ([]protocols.Item, error) {
	var fields map[string]json.RawMessage
	if err := decodeOne(raw, &fields); err != nil {
		return nil, err
	}
	for key := range fields {
		switch key {
		case "role", "content":
		default:
			return nil, fmt.Errorf("unsupported field %q", key)
		}
	}
	role, err := requiredString(fields, "role")
	if err != nil {
		return nil, err
	}
	if role != "user" && role != "assistant" && role != "system" && role != "developer" {
		return nil, fmt.Errorf("unsupported role %q", role)
	}
	contentRaw, ok := fields["content"]
	if !ok {
		return nil, fmt.Errorf("content is required")
	}
	var text string
	if json.Unmarshal(contentRaw, &text) == nil {
		partType := "input_text"
		if role == "assistant" {
			partType = "output_text"
		}
		return []protocols.Item{{Type: "message", Role: role, Content: []protocols.ContentPart{{Type: partType, Text: text}}}}, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(contentRaw, &blocks); err != nil {
		return nil, fmt.Errorf("content must be a string or array")
	}
	var items []protocols.Item
	var messageParts []protocols.ContentPart
	flushMessage := func() {
		if len(messageParts) == 0 {
			return
		}
		items = append(items, protocols.Item{Type: "message", Role: role, Content: messageParts})
		messageParts = nil
	}
	for index, block := range blocks {
		decoded, err := decodeContentBlock(block, role)
		if err != nil {
			return nil, fmt.Errorf("content %d: %w", index, err)
		}
		switch decoded.kind {
		case "part":
			messageParts = append(messageParts, decoded.part)
		case "item":
			flushMessage()
			items = append(items, decoded.item)
		}
	}
	flushMessage()
	if len(items) == 0 {
		return nil, fmt.Errorf("content must not be empty")
	}
	return items, nil
}

type decodedBlock struct {
	kind string
	part protocols.ContentPart
	item protocols.Item
}

func decodeContentBlock(raw json.RawMessage, role string) (decodedBlock, error) {
	var fields map[string]json.RawMessage
	if err := decodeOne(raw, &fields); err != nil {
		return decodedBlock{}, err
	}
	blockType, err := requiredString(fields, "type")
	if err != nil {
		return decodedBlock{}, err
	}
	if (role == "system" || role == "developer") && blockType != "text" {
		return decodedBlock{}, fmt.Errorf("%s messages only support text content", role)
	}
	switch blockType {
	case "text":
		text, err := requiredStringAllowEmpty(fields, "text")
		if err != nil {
			return decodedBlock{}, err
		}
		partType := "input_text"
		if role == "assistant" {
			partType = "output_text"
		}
		return decodedBlock{kind: "part", part: protocols.ContentPart{Type: partType, Text: text}}, nil
	case "image":
		if role == "assistant" {
			return decodedBlock{}, fmt.Errorf("assistant messages do not support image content")
		}
		imageURL, err := decodeImageSource(fields["source"])
		if err != nil {
			return decodedBlock{}, err
		}
		return decodedBlock{kind: "part", part: protocols.ContentPart{Type: "input_image", ImageURL: imageURL}}, nil
	case "document":
		if role == "assistant" {
			return decodedBlock{}, fmt.Errorf("assistant messages do not support document content")
		}
		fileData, filename, err := decodeDocumentSource(fields["source"])
		if err != nil {
			return decodedBlock{}, err
		}
		return decodedBlock{kind: "part", part: protocols.ContentPart{Type: "input_file", FileData: fileData, Filename: filename}}, nil
	case "thinking":
		summary := []protocols.SummaryPart{}
		if thinking, _ := optionalString(fields, "thinking"); thinking != "" {
			summary = append(summary, protocols.SummaryPart{Type: "summary_text", Text: thinking})
		}
		item := protocols.Item{Type: "reasoning", Summary: summary}
		item.EncryptedContent, _ = optionalString(fields, "signature")
		return decodedBlock{kind: "item", item: item}, nil
	case "redacted_thinking":
		data, err := optionalString(fields, "data")
		if err != nil {
			return decodedBlock{}, err
		}
		return decodedBlock{kind: "item", item: protocols.Item{Type: "reasoning", EncryptedContent: data, Summary: []protocols.SummaryPart{}}}, nil
	case "tool_use":
		id, err := requiredString(fields, "id")
		if err != nil {
			return decodedBlock{}, err
		}
		name, err := requiredString(fields, "name")
		if err != nil {
			return decodedBlock{}, err
		}
		inputRaw := fields["input"]
		if len(inputRaw) == 0 {
			inputRaw = []byte("{}")
		}
		return decodedBlock{kind: "item", item: protocols.Item{
			Type: "function_call", ID: id, CallID: id, Name: name, Arguments: string(inputRaw),
		}}, nil
	case "tool_result":
		toolUseID, err := requiredString(fields, "tool_use_id")
		if err != nil {
			return decodedBlock{}, err
		}
		output, outputContent, err := decodeToolResultContent(fields["content"])
		if err != nil {
			return decodedBlock{}, err
		}
		return decodedBlock{kind: "item", item: protocols.Item{
			Type: "function_call_output", CallID: toolUseID, Output: output, OutputContent: outputContent,
		}}, nil
	default:
		return decodedBlock{}, fmt.Errorf("unsupported content type %q", blockType)
	}
}

func decodeImageSource(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("source is required")
	}
	var fields map[string]json.RawMessage
	if err := decodeOne(raw, &fields); err != nil {
		return "", err
	}
	sourceType, err := requiredString(fields, "type")
	if err != nil {
		return "", err
	}
	switch sourceType {
	case "base64":
		mediaType, err := requiredString(fields, "media_type")
		if err != nil {
			return "", err
		}
		data, err := requiredString(fields, "data")
		if err != nil {
			return "", err
		}
		return "data:" + mediaType + ";base64," + data, nil
	case "url":
		return requiredString(fields, "url")
	default:
		return "", fmt.Errorf("unsupported image source type %q", sourceType)
	}
}

func decodeDocumentSource(raw json.RawMessage) (string, string, error) {
	if len(raw) == 0 {
		return "", "", fmt.Errorf("source is required")
	}
	var fields map[string]json.RawMessage
	if err := decodeOne(raw, &fields); err != nil {
		return "", "", err
	}
	sourceType, err := requiredString(fields, "type")
	if err != nil {
		return "", "", err
	}
	if sourceType != "base64" {
		return "", "", fmt.Errorf("unsupported document source type %q", sourceType)
	}
	mediaType, err := requiredString(fields, "media_type")
	if err != nil {
		return "", "", err
	}
	if mediaType != "application/pdf" {
		return "", "", fmt.Errorf("unsupported document media type %q", mediaType)
	}
	data, err := requiredString(fields, "data")
	if err != nil {
		return "", "", err
	}
	return "data:application/pdf;base64," + data, "document.pdf", nil
}

func decodeToolResultContent(raw json.RawMessage) (string, []protocols.ContentPart, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil, nil
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil, fmt.Errorf("content must be a string or array")
	}
	parts := make([]protocols.ContentPart, 0, len(blocks))
	for index, block := range blocks {
		var fields map[string]json.RawMessage
		if err := decodeOne(block, &fields); err != nil {
			return "", nil, fmt.Errorf("content %d: %w", index, err)
		}
		blockType, err := requiredString(fields, "type")
		if err != nil {
			return "", nil, err
		}
		switch blockType {
		case "text":
			value, err := requiredStringAllowEmpty(fields, "text")
			if err != nil {
				return "", nil, err
			}
			parts = append(parts, protocols.ContentPart{Type: "input_text", Text: value})
		case "image":
			imageURL, err := decodeImageSource(fields["source"])
			if err != nil {
				return "", nil, err
			}
			parts = append(parts, protocols.ContentPart{Type: "input_image", ImageURL: imageURL})
		default:
			return "", nil, fmt.Errorf("unsupported tool_result content type %q", blockType)
		}
	}
	return "", parts, nil
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
			case "name", "description", "input_schema", "type", "cache_control":
			default:
				return nil, fmt.Errorf("tool %d: unsupported field %q", index, key)
			}
		}
		name, err := requiredString(fields, "name")
		if err != nil {
			return nil, fmt.Errorf("tool %d: %w", index, err)
		}
		description, err := optionalString(fields, "description")
		if err != nil {
			return nil, fmt.Errorf("tool %d: %w", index, err)
		}
		schema := fields["input_schema"]
		if len(schema) == 0 {
			schema = []byte(`{"type":"object","properties":{}}`)
		} else if !json.Valid(schema) || bytes.TrimSpace(schema)[0] != '{' {
			return nil, fmt.Errorf("tool %d: input_schema must be a JSON object", index)
		}
		result = append(result, protocols.Tool{
			Type: "function", Name: name, Description: description, Parameters: bytes.Clone(schema),
		})
	}
	return result, nil
}

func decodeToolChoice(raw json.RawMessage) (*protocols.ToolChoice, error) {
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		switch mode {
		case "auto", "none", "any", "required":
			if mode == "any" {
				mode = "required"
			}
			return &protocols.ToolChoice{Mode: mode}, nil
		default:
			return nil, fmt.Errorf("unsupported tool_choice %q", mode)
		}
	}
	var fields map[string]json.RawMessage
	if err := decodeOne(raw, &fields); err != nil {
		return nil, err
	}
	choiceType, err := requiredString(fields, "type")
	if err != nil {
		return nil, err
	}
	switch choiceType {
	case "auto", "none", "any":
		mode := choiceType
		if mode == "any" {
			mode = "required"
		}
		return &protocols.ToolChoice{Mode: mode}, nil
	case "tool":
		name, err := requiredString(fields, "name")
		if err != nil {
			return nil, err
		}
		return &protocols.ToolChoice{Mode: "function", Name: name}, nil
	default:
		return nil, fmt.Errorf("unsupported tool_choice type %q", choiceType)
	}
}

func decodeThinking(raw json.RawMessage) (*protocols.ReasoningConfig, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := decodeOne(raw, &fields); err != nil {
		return nil, err
	}
	for key := range fields {
		if key != "type" && key != "budget_tokens" && key != "display" {
			return nil, fmt.Errorf("unsupported thinking field %q", key)
		}
	}
	thinkingType, err := optionalString(fields, "type")
	if err != nil {
		return nil, err
	}
	switch thinkingType {
	case "", "disabled":
		return nil, nil
	case "enabled", "adaptive":
		return &protocols.ReasoningConfig{Effort: "high", Summary: "auto"}, nil
	default:
		return nil, fmt.Errorf("unsupported thinking type %q", thinkingType)
	}
}

func contentBlocksFromOutput(output []protocols.Item) []map[string]any {
	blocks := make([]map[string]any, 0, len(output))
	for _, item := range output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" || part.Type == "input_text" {
					blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
				}
			}
		case "reasoning":
			thinking := ""
			for _, summary := range item.Summary {
				if thinking != "" {
					thinking += "\n\n"
				}
				thinking += summary.Text
			}
			block := map[string]any{"type": "thinking", "thinking": thinking}
			if item.EncryptedContent != "" {
				block["signature"] = item.EncryptedContent
			}
			blocks = append(blocks, block)
		case "function_call":
			var input any = map[string]any{}
			_ = json.Unmarshal([]byte(item.Arguments), &input)
			blocks = append(blocks, map[string]any{
				"type": "tool_use", "id": firstNonEmpty(item.CallID, item.ID), "name": item.Name, "input": input,
			})
		}
	}
	if blocks == nil {
		blocks = []map[string]any{}
	}
	return blocks
}

func stopReason(response protocols.Response) string {
	for _, item := range response.Output {
		if item.Type == "function_call" || item.Type == "custom_tool_call" {
			return "tool_use"
		}
	}
	if response.Status == "incomplete" {
		return "max_tokens"
	}
	return "end_turn"
}

func usageValue(usage *protocols.Usage) map[string]any {
	if usage == nil {
		return map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	return map[string]any{"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens}
}

func estimateTokens(request protocols.Request) int {
	total := estimateText(request.Instructions)
	for _, item := range request.Input {
		total += estimateText(item.Arguments) + estimateText(item.Input) + estimateText(item.Output) + estimateText(item.Name)
		for _, part := range item.Content {
			total += estimateContentPart(part)
		}
		for _, part := range item.OutputContent {
			total += estimateContentPart(part)
		}
		for _, summary := range item.Summary {
			total += estimateText(summary.Text)
		}
	}
	for _, tool := range request.Tools {
		total += estimateText(tool.Name) + estimateText(tool.Description) + estimateText(string(tool.Parameters)) + estimateText(string(tool.Format))
	}
	if total < 1 {
		total = 1
	}
	return total
}

func estimateContentPart(part protocols.ContentPart) int {
	total := estimateText(part.Text)
	switch part.Type {
	case "input_image":
		total += 1100
	case "input_file":
		total += 2000
	case "input_audio":
		total += 500
	}
	return total
}

func estimateText(text string) int {
	if text == "" {
		return 0
	}
	runes := utf8.RuneCountInString(text)
	return (runes + 3) / 4
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
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

func clientError(message, param, code string) error {
	return &protocols.ClientError{Status: http.StatusBadRequest, Message: message, Param: param, Code: code}
}
