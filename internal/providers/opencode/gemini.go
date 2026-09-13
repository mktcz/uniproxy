package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mktcz/uniproxy/internal/protocols"
)

func buildGeminiBody(request protocols.Request) ([]byte, error) {
	contents, system, err := geminiContentsFromIR(request)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{
		"contents": contents,
	}
	if system != "" {
		payload["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system}},
		}
	}
	if len(request.Tools) > 0 {
		decls := make([]map[string]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			entry := map[string]any{"name": tool.Name}
			if tool.Description != "" {
				entry["description"] = tool.Description
			}
			if len(tool.Parameters) > 0 {
				entry["parameters"] = json.RawMessage(tool.Parameters)
			}
			decls = append(decls, entry)
		}
		payload["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}
	return json.Marshal(payload)
}

func geminiContentsFromIR(request protocols.Request) ([]map[string]any, string, error) {
	system := request.Instructions
	contents := make([]map[string]any, 0, len(request.Input))
	for _, item := range request.Input {
		switch item.Type {
		case "message":
			role := item.Role
			if role == "system" || role == "developer" {
				text := textFromParts(item.Content)
				if system == "" {
					system = text
				} else if text != "" {
					system += "\n\n" + text
				}
				continue
			}
			geminiRole := "user"
			if role == "assistant" {
				geminiRole = "model"
			}
			parts, err := geminiPartsFromContent(item.Content)
			if err != nil {
				return nil, "", err
			}
			contents = append(contents, map[string]any{"role": geminiRole, "parts": parts})
		case "function_call":
			var args any = map[string]any{}
			if item.Arguments != "" {
				_ = json.Unmarshal([]byte(item.Arguments), &args)
			}
			contents = append(contents, map[string]any{
				"role": "model",
				"parts": []map[string]any{{
					"functionCall": map[string]any{"name": item.Name, "args": args},
				}},
			})
		case "function_call_output":
			var response any = item.Output
			if json.Valid([]byte(item.Output)) {
				_ = json.Unmarshal([]byte(item.Output), &response)
			}
			contents = append(contents, map[string]any{
				"role": "user",
				"parts": []map[string]any{{
					"functionResponse": map[string]any{
						"name":     item.Name,
						"response": map[string]any{"content": response},
					},
				}},
			})
		case "reasoning", "custom_tool_call", "custom_tool_call_output":
			continue
		default:
			return nil, "", fmt.Errorf("unsupported gemini item type %q", item.Type)
		}
	}
	return contents, system, nil
}

func geminiPartsFromContent(parts []protocols.ContentPart) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text":
			out = append(out, map[string]any{"text": part.Text})
		case "input_image":
			url := part.ImageURL
			if strings.HasPrefix(url, "data:") {
				mediaType := "image/png"
				data := url
				if comma := strings.Index(url, ","); comma >= 0 {
					meta := url[5:comma]
					data = url[comma+1:]
					if semi := strings.Index(meta, ";"); semi >= 0 {
						mediaType = meta[:semi]
					}
				}
				out = append(out, map[string]any{
					"inlineData": map[string]any{"mimeType": mediaType, "data": data},
				})
			} else {
				out = append(out, map[string]any{
					"fileData": map[string]any{"fileUri": url},
				})
			}
		default:
			return nil, fmt.Errorf("unsupported gemini content part %q", part.Type)
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"text": ""})
	}
	return out, nil
}

type geminiStreamState struct {
	responseID string
	model      string
	seq        int
	createdAt  int64
	started    bool
	text       strings.Builder
	toolName   string
	toolArgs   strings.Builder
	usage      *protocols.Usage
}

func newGeminiStreamState(model string) *geminiStreamState {
	return &geminiStreamState{
		responseID: "gemini_opencode",
		model:      model,
		createdAt:  time.Now().Unix(),
	}
}

func (s *geminiStreamState) nextSeq() int {
	s.seq++
	return s.seq
}

func (s *geminiStreamState) consume(payload []byte, emit func(protocols.StreamEvent) error) error {
	var chunk struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text         string `json:"text"`
					FunctionCall *struct {
						Name string          `json:"name"`
						Args json.RawMessage `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata *struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
		ResponseID string `json:"responseId"`
		Model      string `json:"modelVersion"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return fmt.Errorf("decode gemini chunk: %w", err)
	}
	if chunk.ResponseID != "" {
		s.responseID = chunk.ResponseID
	}
	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.UsageMetadata != nil {
		s.usage = &protocols.Usage{
			InputTokens:  chunk.UsageMetadata.PromptTokenCount,
			OutputTokens: chunk.UsageMetadata.CandidatesTokenCount,
			TotalTokens:  chunk.UsageMetadata.TotalTokenCount,
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
	for _, cand := range chunk.Candidates {
		for _, part := range cand.Content.Parts {
			if part.Text != "" {
				if s.text.Len() == 0 {
					item := protocols.Item{Type: "message", ID: "msg_0", Role: "assistant", Status: "in_progress"}
					if err := emit(protocols.StreamEvent{Type: protocols.EventOutputItemAdded, SequenceNumber: s.nextSeq(), OutputIndex: intPtr(0), Item: &item}); err != nil {
						return err
					}
					cp := protocols.ContentPart{Type: "output_text", Text: ""}
					if err := emit(protocols.StreamEvent{Type: protocols.EventContentPartAdded, SequenceNumber: s.nextSeq(), ItemID: "msg_0", OutputIndex: intPtr(0), ContentIndex: intPtr(0), Part: &cp}); err != nil {
						return err
					}
				}
				s.text.WriteString(part.Text)
				if err := emit(protocols.StreamEvent{Type: protocols.EventTextDelta, SequenceNumber: s.nextSeq(), ItemID: "msg_0", OutputIndex: intPtr(0), ContentIndex: intPtr(0), Delta: part.Text}); err != nil {
					return err
				}
			}
			if part.FunctionCall != nil {
				s.toolName = part.FunctionCall.Name
				s.toolArgs.Write(part.FunctionCall.Args)
				item := protocols.Item{Type: "function_call", ID: "fc_0", CallID: "fc_0", Name: s.toolName, Status: "in_progress"}
				if err := emit(protocols.StreamEvent{Type: protocols.EventOutputItemAdded, SequenceNumber: s.nextSeq(), OutputIndex: intPtr(1), Item: &item}); err != nil {
					return err
				}
				args := string(part.FunctionCall.Args)
				if err := emit(protocols.StreamEvent{Type: protocols.EventToolArgumentsDelta, SequenceNumber: s.nextSeq(), ItemID: "fc_0", Delta: args, Arguments: args}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *geminiStreamState) finish(emit func(protocols.StreamEvent) error) error {
	output := []protocols.Item{}
	if s.text.Len() > 0 {
		text := s.text.String()
		if err := emit(protocols.StreamEvent{Type: protocols.EventTextDone, SequenceNumber: s.nextSeq(), ItemID: "msg_0", OutputIndex: intPtr(0), ContentIndex: intPtr(0), Text: text}); err != nil {
			return err
		}
		part := protocols.ContentPart{Type: "output_text", Text: text}
		if err := emit(protocols.StreamEvent{Type: protocols.EventContentPartDone, SequenceNumber: s.nextSeq(), ItemID: "msg_0", OutputIndex: intPtr(0), ContentIndex: intPtr(0), Part: &part}); err != nil {
			return err
		}
		item := protocols.Item{Type: "message", ID: "msg_0", Role: "assistant", Status: "completed", Content: []protocols.ContentPart{part}}
		if err := emit(protocols.StreamEvent{Type: protocols.EventOutputItemDone, SequenceNumber: s.nextSeq(), OutputIndex: intPtr(0), Item: &item}); err != nil {
			return err
		}
		output = append(output, item)
	}
	if s.toolName != "" {
		args := s.toolArgs.String()
		if err := emit(protocols.StreamEvent{Type: protocols.EventToolArgumentsDone, SequenceNumber: s.nextSeq(), ItemID: "fc_0", Arguments: args}); err != nil {
			return err
		}
		item := protocols.Item{Type: "function_call", ID: "fc_0", CallID: "fc_0", Name: s.toolName, Arguments: args, Status: "completed"}
		if err := emit(protocols.StreamEvent{Type: protocols.EventOutputItemDone, SequenceNumber: s.nextSeq(), OutputIndex: intPtr(1), Item: &item}); err != nil {
			return err
		}
		output = append(output, item)
	}
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
		Status: "completed", Model: s.model, Output: output, Usage: s.usage,
	}
	return emit(protocols.StreamEvent{Type: protocols.EventResponseCompleted, SequenceNumber: s.nextSeq(), Response: resp})
}
