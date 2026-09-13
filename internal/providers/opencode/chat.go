package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mktcz/uniproxy/internal/protocols"
)

func buildChatBody(request protocols.Request, wireModel string) ([]byte, error) {
	messages, err := chatMessagesFromIR(request)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"model":    wireModel,
		"messages": messages,
		"stream":   true,
	}
	if len(request.Tools) > 0 {
		tools := make([]map[string]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			if tool.Type != "" && tool.Type != "function" {
				continue
			}
			fn := map[string]any{"name": tool.Name}
			if tool.Description != "" {
				fn["description"] = tool.Description
			}
			if len(tool.Parameters) > 0 {
				fn["parameters"] = json.RawMessage(tool.Parameters)
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		if len(tools) > 0 {
			payload["tools"] = tools
		}
	}
	if request.ToolChoice != nil {
		switch request.ToolChoice.Mode {
		case "auto", "none", "required":
			payload["tool_choice"] = request.ToolChoice.Mode
		case "function":
			payload["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]string{"name": request.ToolChoice.Name},
			}
		}
	}
	if request.MaxOutputTokens != nil {
		payload["max_tokens"] = *request.MaxOutputTokens
	}
	return json.Marshal(payload)
}

func chatMessagesFromIR(request protocols.Request) ([]map[string]any, error) {
	messages := make([]map[string]any, 0, len(request.Input)+1)
	if request.Instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": request.Instructions})
	}
	pendingCalls := map[string]protocols.Item{}
	for _, item := range request.Input {
		switch item.Type {
		case "message":
			role := item.Role
			if role == "" {
				role = "user"
			}
			if role == "developer" {
				role = "system"
			}
			content, err := chatContentFromParts(item.Content)
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": role, "content": content})
		case "function_call":
			pendingCalls[item.CallID] = item
			messages = append(messages, map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":   item.CallID,
					"type": "function",
					"function": map[string]string{
						"name":      item.Name,
						"arguments": item.Arguments,
					},
				}},
			})
		case "function_call_output":
			content := item.Output
			if content == "" && len(item.OutputContent) > 0 {
				content = textFromParts(item.OutputContent)
			}
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": item.CallID,
				"content":      content,
			})
		case "reasoning", "custom_tool_call", "custom_tool_call_output":
			continue
		default:
			return nil, fmt.Errorf("unsupported chat item type %q", item.Type)
		}
	}
	_ = pendingCalls
	return messages, nil
}

func chatContentFromParts(parts []protocols.ContentPart) (any, error) {
	if len(parts) == 0 {
		return "", nil
	}
	if len(parts) == 1 && (parts[0].Type == "input_text" || parts[0].Type == "output_text") {
		return parts[0].Text, nil
	}
	content := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text":
			content = append(content, map[string]any{"type": "text", "text": part.Text})
		case "input_image":
			url := part.ImageURL
			if url == "" {
				url = part.FileURL
			}
			content = append(content, map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": url,
				},
			})
		default:
			return nil, fmt.Errorf("unsupported chat content part %q", part.Type)
		}
	}
	return content, nil
}

type chatStreamState struct {
	responseID   string
	model        string
	seq          int
	text         strings.Builder
	started      bool
	messageAdded bool
	toolCalls    map[int]*toolCallState
	usage        *protocols.Usage
	createdAt    int64
	finishReason string
}

type toolCallState struct {
	ID        string
	Name      string
	Arguments strings.Builder
	Started   bool
}

func newChatStreamState(model string) *chatStreamState {
	return &chatStreamState{
		responseID: "chatcmpl_opencode",
		model:      model,
		toolCalls:  map[int]*toolCallState{},
		createdAt:  time.Now().Unix(),
	}
}

func (s *chatStreamState) nextSeq() int {
	s.seq++
	return s.seq
}

func (s *chatStreamState) consume(payload []byte, emit func(protocols.StreamEvent) error) error {
	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return fmt.Errorf("decode chat SSE chunk: %w", err)
	}
	if chunk.ID != "" {
		s.responseID = chunk.ID
	}
	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.Usage != nil {
		s.usage = &protocols.Usage{
			InputTokens:  chunk.Usage.PromptTokens,
			OutputTokens: chunk.Usage.CompletionTokens,
			TotalTokens:  chunk.Usage.TotalTokens,
		}
	}
	if !s.started {
		s.started = true
		resp := &protocols.Response{
			ID: s.responseID, Object: "response", CreatedAt: s.createdAt,
			Status: "in_progress", Model: s.model, Output: []protocols.Item{},
		}
		if err := emit(protocols.StreamEvent{Type: protocols.EventResponseCreated, SequenceNumber: s.nextSeq(), Response: resp}); err != nil {
			return err
		}
		if err := emit(protocols.StreamEvent{Type: protocols.EventResponseInProgress, SequenceNumber: s.nextSeq(), Response: resp}); err != nil {
			return err
		}
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.Content != "" {
			if err := s.ensureMessage(emit); err != nil {
				return err
			}
			s.text.WriteString(choice.Delta.Content)
			if err := emit(protocols.StreamEvent{
				Type: protocols.EventTextDelta, SequenceNumber: s.nextSeq(),
				ItemID: "msg_0", OutputIndex: intPtr(0), ContentIndex: intPtr(0), Delta: choice.Delta.Content,
			}); err != nil {
				return err
			}
		}
		if choice.Delta.ReasoningContent != "" {
			if err := emit(protocols.StreamEvent{
				Type: protocols.EventReasoningDelta, SequenceNumber: s.nextSeq(),
				ItemID: "rs_0", OutputIndex: intPtr(0), Delta: choice.Delta.ReasoningContent,
			}); err != nil {
				return err
			}
		}
		for _, call := range choice.Delta.ToolCalls {
			state, ok := s.toolCalls[call.Index]
			if !ok {
				state = &toolCallState{}
				s.toolCalls[call.Index] = state
			}
			if call.ID != "" {
				state.ID = call.ID
			}
			if call.Function.Name != "" {
				state.Name = call.Function.Name
			}
			if call.Function.Arguments != "" {
				state.Arguments.WriteString(call.Function.Arguments)
			}
			if !state.Started && state.ID != "" && state.Name != "" {
				state.Started = true
				idx := call.Index
				if s.messageAdded {
					idx = call.Index + 1
				}
				item := protocols.Item{
					Type: "function_call", ID: state.ID, CallID: state.ID,
					Name: state.Name, Arguments: "", Status: "in_progress",
				}
				if err := emit(protocols.StreamEvent{
					Type: protocols.EventOutputItemAdded, SequenceNumber: s.nextSeq(),
					OutputIndex: intPtr(idx), Item: &item,
				}); err != nil {
					return err
				}
			}
			if call.Function.Arguments != "" {
				if err := emit(protocols.StreamEvent{
					Type: protocols.EventToolArgumentsDelta, SequenceNumber: s.nextSeq(),
					ItemID: state.ID, Delta: call.Function.Arguments, Arguments: call.Function.Arguments,
				}); err != nil {
					return err
				}
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			s.finishReason = *choice.FinishReason
		}
	}
	return nil
}

func (s *chatStreamState) ensureMessage(emit func(protocols.StreamEvent) error) error {
	if s.messageAdded {
		return nil
	}
	s.messageAdded = true
	item := protocols.Item{
		Type: "message", ID: "msg_0", Role: "assistant", Status: "in_progress",
		Content: []protocols.ContentPart{},
	}
	if err := emit(protocols.StreamEvent{
		Type: protocols.EventOutputItemAdded, SequenceNumber: s.nextSeq(),
		OutputIndex: intPtr(0), Item: &item,
	}); err != nil {
		return err
	}
	part := protocols.ContentPart{Type: "output_text", Text: ""}
	return emit(protocols.StreamEvent{
		Type: protocols.EventContentPartAdded, SequenceNumber: s.nextSeq(),
		ItemID: "msg_0", OutputIndex: intPtr(0), ContentIndex: intPtr(0), Part: &part,
	})
}

func (s *chatStreamState) finish(emit func(protocols.StreamEvent) error) error {
	output := make([]protocols.Item, 0, 1+len(s.toolCalls))
	if s.messageAdded || s.text.Len() > 0 {
		if !s.messageAdded {
			if err := s.ensureMessage(emit); err != nil {
				return err
			}
		}
		text := s.text.String()
		if err := emit(protocols.StreamEvent{
			Type: protocols.EventTextDone, SequenceNumber: s.nextSeq(),
			ItemID: "msg_0", OutputIndex: intPtr(0), ContentIndex: intPtr(0), Text: text,
		}); err != nil {
			return err
		}
		part := protocols.ContentPart{Type: "output_text", Text: text}
		if err := emit(protocols.StreamEvent{
			Type: protocols.EventContentPartDone, SequenceNumber: s.nextSeq(),
			ItemID: "msg_0", OutputIndex: intPtr(0), ContentIndex: intPtr(0), Part: &part,
		}); err != nil {
			return err
		}
		item := protocols.Item{
			Type: "message", ID: "msg_0", Role: "assistant", Status: "completed",
			Content: []protocols.ContentPart{{Type: "output_text", Text: text}},
		}
		if err := emit(protocols.StreamEvent{
			Type: protocols.EventOutputItemDone, SequenceNumber: s.nextSeq(),
			OutputIndex: intPtr(0), Item: &item,
		}); err != nil {
			return err
		}
		output = append(output, item)
	}
	for index := 0; index < len(s.toolCalls)+8; index++ {
		state, ok := s.toolCalls[index]
		if !ok {
			continue
		}
		args := state.Arguments.String()
		if err := emit(protocols.StreamEvent{
			Type: protocols.EventToolArgumentsDone, SequenceNumber: s.nextSeq(),
			ItemID: state.ID, Arguments: args,
		}); err != nil {
			return err
		}
		outIndex := index
		if s.messageAdded {
			outIndex = index + 1
		}
		item := protocols.Item{
			Type: "function_call", ID: state.ID, CallID: state.ID,
			Name: state.Name, Arguments: args, Status: "completed",
		}
		if err := emit(protocols.StreamEvent{
			Type: protocols.EventOutputItemDone, SequenceNumber: s.nextSeq(),
			OutputIndex: intPtr(outIndex), Item: &item,
		}); err != nil {
			return err
		}
		output = append(output, item)
	}
	if !s.started {
		s.started = true
		resp := &protocols.Response{
			ID: s.responseID, Object: "response", CreatedAt: s.createdAt,
			Status: "in_progress", Model: s.model, Output: []protocols.Item{},
		}
		if err := emit(protocols.StreamEvent{Type: protocols.EventResponseCreated, SequenceNumber: s.nextSeq(), Response: resp}); err != nil {
			return err
		}
	}
	completed := time.Now().Unix()
	status := "completed"
	eventType := protocols.EventResponseCompleted
	var incomplete *protocols.IncompleteDetails
	if s.finishReason == "length" || s.finishReason == "max_tokens" {
		status = "incomplete"
		eventType = protocols.EventResponseIncomplete
		incomplete = &protocols.IncompleteDetails{Reason: "max_output_tokens"}
	}
	resp := &protocols.Response{
		ID: s.responseID, Object: "response", CreatedAt: s.createdAt, CompletedAt: &completed,
		Status: status, Model: s.model, Output: output, Usage: s.usage, IncompleteDetails: incomplete,
	}
	return emit(protocols.StreamEvent{Type: eventType, SequenceNumber: s.nextSeq(), Response: resp})
}
