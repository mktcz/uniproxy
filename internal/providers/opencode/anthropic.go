package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mktcz/uniproxy/internal/protocols"
)

func buildMessagesBody(request protocols.Request, wireModel string) ([]byte, error) {
	system, messages, err := anthropicMessagesFromIR(request)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"model":      wireModel,
		"messages":   messages,
		"max_tokens": 8192,
		"stream":     true,
	}
	if request.MaxOutputTokens != nil {
		payload["max_tokens"] = *request.MaxOutputTokens
	}
	if system != "" {
		payload["system"] = system
	}
	if len(request.Tools) > 0 {
		tools := make([]map[string]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			entry := map[string]any{
				"name":         tool.Name,
				"description":  tool.Description,
				"input_schema": json.RawMessage(tool.Parameters),
			}
			if len(tool.Parameters) == 0 {
				entry["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, entry)
		}
		payload["tools"] = tools
	}
	if request.ToolChoice != nil {
		switch request.ToolChoice.Mode {
		case "auto":
			payload["tool_choice"] = map[string]string{"type": "auto"}
		case "none":
			payload["tool_choice"] = map[string]string{"type": "none"}
		case "required":
			payload["tool_choice"] = map[string]string{"type": "any"}
		case "function":
			payload["tool_choice"] = map[string]any{"type": "tool", "name": request.ToolChoice.Name}
		}
	}
	return json.Marshal(payload)
}

func anthropicMessagesFromIR(request protocols.Request) (string, []map[string]any, error) {
	system := request.Instructions
	messages := make([]map[string]any, 0, len(request.Input))
	for _, item := range request.Input {
		switch item.Type {
		case "message":
			role := item.Role
			if role == "system" || role == "developer" {
				text := textFromParts(item.Content)
				if system == "" {
					system = text
				} else if text != "" {
					system = system + "\n\n" + text
				}
				continue
			}
			if role == "" {
				role = "user"
			}
			content, err := anthropicContentFromParts(item.Content, role)
			if err != nil {
				return "", nil, err
			}
			messages = append(messages, map[string]any{"role": role, "content": content})
		case "function_call":
			input := json.RawMessage(item.Arguments)
			if !json.Valid(input) {
				raw, _ := json.Marshal(item.Arguments)
				input = raw
			}
			messages = append(messages, map[string]any{
				"role": "assistant",
				"content": []map[string]any{{
					"type":  "tool_use",
					"id":    firstNonEmpty(item.CallID, item.ID),
					"name":  item.Name,
					"input": input,
				}},
			})
		case "function_call_output":
			content := any(item.Output)
			if len(item.OutputContent) > 0 {
				parts := make([]map[string]any, 0, len(item.OutputContent))
				for _, part := range item.OutputContent {
					if part.Type == "input_text" || part.Type == "output_text" {
						parts = append(parts, map[string]any{"type": "text", "text": part.Text})
					}
				}
				content = parts
			}
			messages = append(messages, map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type":        "tool_result",
					"tool_use_id": item.CallID,
					"content":     content,
				}},
			})
		case "reasoning", "custom_tool_call", "custom_tool_call_output":
			continue
		default:
			return "", nil, fmt.Errorf("unsupported anthropic item type %q", item.Type)
		}
	}
	return system, messages, nil
}

func anthropicContentFromParts(parts []protocols.ContentPart, role string) (any, error) {
	if len(parts) == 1 && (parts[0].Type == "input_text" || parts[0].Type == "output_text") {
		return parts[0].Text, nil
	}
	content := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text":
			content = append(content, map[string]any{"type": "text", "text": part.Text})
		case "input_image":
			if role == "assistant" {
				return nil, fmt.Errorf("assistant messages do not support image content")
			}
			url := part.ImageURL
			source := map[string]any{"type": "url", "url": url}
			if strings.HasPrefix(url, "data:") {

				mediaType := "image/png"
				data := url
				if comma := strings.Index(url, ","); comma >= 0 {
					meta := url[5:comma]
					data = url[comma+1:]
					if semi := strings.Index(meta, ";"); semi >= 0 {
						mediaType = meta[:semi]
					} else if meta != "" {
						mediaType = meta
					}
				}
				source = map[string]any{"type": "base64", "media_type": mediaType, "data": data}
			}
			content = append(content, map[string]any{"type": "image", "source": source})
		default:
			return nil, fmt.Errorf("unsupported anthropic content part %q", part.Type)
		}
	}
	return content, nil
}

type anthropicStreamState struct {
	responseID string
	model      string
	seq        int
	createdAt  int64
	started    bool
	blocks     map[int]*anthropicBlock
	usage      *protocols.Usage
	stopReason string
	stopped    bool
}

type anthropicBlock struct {
	Type      string
	ID        string
	Name      string
	Text      strings.Builder
	Thinking  strings.Builder
	Arguments strings.Builder
}

func newAnthropicStreamState(model string) *anthropicStreamState {
	return &anthropicStreamState{
		responseID: "msg_opencode",
		model:      model,
		blocks:     map[int]*anthropicBlock{},
		createdAt:  time.Now().Unix(),
	}
}

func (s *anthropicStreamState) nextSeq() int {
	s.seq++
	return s.seq
}

func (s *anthropicStreamState) consume(payload []byte, emit func(protocols.StreamEvent) error) error {
	var envelope struct {
		Type    string          `json:"type"`
		Index   int             `json:"index"`
		Message json.RawMessage `json:"message"`
		Delta   json.RawMessage `json:"delta"`
		Content json.RawMessage `json:"content_block"`
		Usage   json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode anthropic SSE: %w", err)
	}
	switch envelope.Type {
	case "message_start":
		var msg struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(envelope.Message, &msg)
		if msg.ID != "" {
			s.responseID = msg.ID
		}
		if msg.Model != "" {
			s.model = msg.Model
		}
		if msg.Usage != nil {
			s.usage = &protocols.Usage{InputTokens: msg.Usage.InputTokens}
		}
		s.started = true
		resp := &protocols.Response{
			ID: s.responseID, Object: "response", CreatedAt: s.createdAt,
			Status: "in_progress", Model: s.model, Output: []protocols.Item{},
		}
		if err := emit(protocols.StreamEvent{Type: protocols.EventResponseCreated, SequenceNumber: s.nextSeq(), Response: resp}); err != nil {
			return err
		}
		return emit(protocols.StreamEvent{Type: protocols.EventResponseInProgress, SequenceNumber: s.nextSeq(), Response: resp})
	case "content_block_start":
		var block struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
			Text  string          `json:"text"`
		}
		_ = json.Unmarshal(envelope.Content, &block)
		state := &anthropicBlock{Type: block.Type, ID: block.ID, Name: block.Name}
		s.blocks[envelope.Index] = state
		switch block.Type {
		case "text":
			item := protocols.Item{Type: "message", ID: "msg_" + fmt.Sprint(envelope.Index), Role: "assistant", Status: "in_progress"}
			if err := emit(protocols.StreamEvent{Type: protocols.EventOutputItemAdded, SequenceNumber: s.nextSeq(), OutputIndex: intPtr(envelope.Index), Item: &item}); err != nil {
				return err
			}
			part := protocols.ContentPart{Type: "output_text", Text: ""}
			return emit(protocols.StreamEvent{Type: protocols.EventContentPartAdded, SequenceNumber: s.nextSeq(), ItemID: item.ID, OutputIndex: intPtr(envelope.Index), ContentIndex: intPtr(0), Part: &part})
		case "tool_use":
			item := protocols.Item{Type: "function_call", ID: block.ID, CallID: block.ID, Name: block.Name, Status: "in_progress"}
			return emit(protocols.StreamEvent{Type: protocols.EventOutputItemAdded, SequenceNumber: s.nextSeq(), OutputIndex: intPtr(envelope.Index), Item: &item})
		case "thinking":
			item := protocols.Item{Type: "reasoning", ID: "rs_" + fmt.Sprint(envelope.Index), Status: "in_progress"}
			return emit(protocols.StreamEvent{Type: protocols.EventOutputItemAdded, SequenceNumber: s.nextSeq(), OutputIndex: intPtr(envelope.Index), Item: &item})
		}
	case "content_block_delta":
		var delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
		}
		_ = json.Unmarshal(envelope.Delta, &delta)
		state := s.blocks[envelope.Index]
		if state == nil {
			state = &anthropicBlock{}
			s.blocks[envelope.Index] = state
		}
		switch delta.Type {
		case "text_delta":
			state.Text.WriteString(delta.Text)
			return emit(protocols.StreamEvent{Type: protocols.EventTextDelta, SequenceNumber: s.nextSeq(), ItemID: "msg_" + fmt.Sprint(envelope.Index), OutputIndex: intPtr(envelope.Index), ContentIndex: intPtr(0), Delta: delta.Text})
		case "thinking_delta", "reasoning_delta":
			state.Thinking.WriteString(delta.Thinking)
			if delta.Thinking == "" {
				state.Thinking.WriteString(delta.Text)
			}
			return emit(protocols.StreamEvent{Type: protocols.EventReasoningDelta, SequenceNumber: s.nextSeq(), ItemID: "rs_" + fmt.Sprint(envelope.Index), OutputIndex: intPtr(envelope.Index), Delta: firstNonEmpty(delta.Thinking, delta.Text)})
		case "input_json_delta":
			state.Arguments.WriteString(delta.PartialJSON)
			return emit(protocols.StreamEvent{Type: protocols.EventToolArgumentsDelta, SequenceNumber: s.nextSeq(), ItemID: state.ID, Delta: delta.PartialJSON, Arguments: delta.PartialJSON})
		}
	case "content_block_stop":
		state := s.blocks[envelope.Index]
		if state == nil {
			return nil
		}
		switch state.Type {
		case "text":
			text := state.Text.String()
			if err := emit(protocols.StreamEvent{Type: protocols.EventTextDone, SequenceNumber: s.nextSeq(), ItemID: "msg_" + fmt.Sprint(envelope.Index), OutputIndex: intPtr(envelope.Index), ContentIndex: intPtr(0), Text: text}); err != nil {
				return err
			}
			part := protocols.ContentPart{Type: "output_text", Text: text}
			if err := emit(protocols.StreamEvent{Type: protocols.EventContentPartDone, SequenceNumber: s.nextSeq(), ItemID: "msg_" + fmt.Sprint(envelope.Index), OutputIndex: intPtr(envelope.Index), ContentIndex: intPtr(0), Part: &part}); err != nil {
				return err
			}
			item := protocols.Item{Type: "message", ID: "msg_" + fmt.Sprint(envelope.Index), Role: "assistant", Status: "completed", Content: []protocols.ContentPart{part}}
			return emit(protocols.StreamEvent{Type: protocols.EventOutputItemDone, SequenceNumber: s.nextSeq(), OutputIndex: intPtr(envelope.Index), Item: &item})
		case "tool_use":
			args := state.Arguments.String()
			if err := emit(protocols.StreamEvent{Type: protocols.EventToolArgumentsDone, SequenceNumber: s.nextSeq(), ItemID: state.ID, Arguments: args}); err != nil {
				return err
			}
			item := protocols.Item{Type: "function_call", ID: state.ID, CallID: state.ID, Name: state.Name, Arguments: args, Status: "completed"}
			return emit(protocols.StreamEvent{Type: protocols.EventOutputItemDone, SequenceNumber: s.nextSeq(), OutputIndex: intPtr(envelope.Index), Item: &item})
		case "thinking":
			item := protocols.Item{Type: "reasoning", ID: "rs_" + fmt.Sprint(envelope.Index), Status: "completed", Summary: []protocols.SummaryPart{{Type: "summary_text", Text: state.Thinking.String()}}}
			return emit(protocols.StreamEvent{Type: protocols.EventOutputItemDone, SequenceNumber: s.nextSeq(), OutputIndex: intPtr(envelope.Index), Item: &item})
		}
	case "message_delta":
		var delta struct {
			StopReason string `json:"stop_reason"`
		}
		_ = json.Unmarshal(envelope.Delta, &delta)
		if delta.StopReason != "" {
			s.stopReason = delta.StopReason
		}
		var usage struct {
			OutputTokens int `json:"output_tokens"`
			InputTokens  int `json:"input_tokens"`
		}
		if json.Unmarshal(envelope.Usage, &usage) == nil {
			if s.usage == nil {
				s.usage = &protocols.Usage{}
			}
			if usage.InputTokens > 0 {
				s.usage.InputTokens = usage.InputTokens
			}
			s.usage.OutputTokens = usage.OutputTokens
			s.usage.TotalTokens = s.usage.InputTokens + s.usage.OutputTokens
		}
	case "message_stop":
		s.stopped = true
		return nil
	case "ping":
		return nil
	case "error":
		var apiErr protocols.APIError
		_ = json.Unmarshal(payload, &apiErr)
		if apiErr.Message == "" {
			apiErr.Message = "OpenCode anthropic upstream error"
		}
		return emit(protocols.StreamEvent{Type: protocols.EventError, SequenceNumber: s.nextSeq(), Error: &apiErr})
	}
	return nil
}

func (s *anthropicStreamState) finish(emit func(protocols.StreamEvent) error, items map[int]protocols.Item) error {
	if !s.started {
		resp := &protocols.Response{
			ID: s.responseID, Object: "response", CreatedAt: s.createdAt,
			Status: "in_progress", Model: s.model, Output: []protocols.Item{},
		}
		if err := emit(protocols.StreamEvent{Type: protocols.EventResponseCreated, SequenceNumber: s.nextSeq(), Response: resp}); err != nil {
			return err
		}
	}
	completed := time.Now().Unix()
	resp := &protocols.Response{
		ID: s.responseID, Object: "response", CreatedAt: s.createdAt, CompletedAt: &completed,
		Status: "completed", Model: s.model, Output: orderedItems(items), Usage: s.usage,
	}
	return emit(protocols.StreamEvent{Type: protocols.EventResponseCompleted, SequenceNumber: s.nextSeq(), Response: resp})
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
