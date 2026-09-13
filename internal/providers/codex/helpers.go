package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/mktcz/uniproxy/internal/protocols"
)

const maxSSEEventBytes = 16 << 20

func readSSE(reader io.Reader, consume func(protocols.StreamEvent) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxSSEEventBytes+1024)
	var data bytes.Buffer
	eventName := ""
	dispatch := func() error {
		if data.Len() == 0 {
			eventName = ""
			return nil
		}
		payload := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
		data.Reset()
		if len(bytes.TrimSpace(payload)) == 0 {
			eventName = ""
			return nil
		}
		if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
			eventName = ""
			return nil
		}
		event, err := decodeEvent(payload, eventName)
		eventName = ""
		if err != nil {
			return err
		}
		return consume(event)
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "":
			if dispatchErr := dispatch(); dispatchErr != nil {
				return dispatchErr
			}
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if data.Len()+len(value)+1 > maxSSEEventBytes {
				return fmt.Errorf("Codex SSE event exceeds %d bytes", maxSSEEventBytes)
			}
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Codex SSE: %w", err)
	}
	return dispatch()
}

func decodeEvent(payload []byte, eventName string) (protocols.StreamEvent, error) {
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
		return protocols.StreamEvent{}, fmt.Errorf("decode Codex SSE event: %w", err)
	}
	if wire.Type == "" {
		wire.Type = eventName
	}
	if wire.Type == "" {
		return protocols.StreamEvent{}, fmt.Errorf("Codex SSE event has no type")
	}
	if !supportedEventType(wire.Type) {
		return protocols.StreamEvent{}, fmt.Errorf("unsupported Codex SSE event type %q", wire.Type)
	}
	event := protocols.StreamEvent{
		Type: eventTypeFromWire(wire.Type), SequenceNumber: wire.SequenceNumber, OutputIndex: wire.OutputIndex,
		ContentIndex: wire.ContentIndex, ItemID: wire.ItemID, Delta: wire.Delta,
		Text: wire.Text, Arguments: wire.Arguments, Input: wire.Input,
	}
	if len(wire.Response) > 0 && !bytes.Equal(wire.Response, []byte("null")) {
		var response protocols.Response
		if err := json.Unmarshal(wire.Response, &response); err != nil {
			return protocols.StreamEvent{}, fmt.Errorf("decode Codex response event: %w", err)
		}
		if err := validateOutputItems(response.Output); err != nil {
			return protocols.StreamEvent{}, err
		}
		event.Response = &response
	}
	if len(wire.Item) > 0 && !bytes.Equal(wire.Item, []byte("null")) {
		var item protocols.Item
		if err := json.Unmarshal(wire.Item, &item); err != nil {
			return protocols.StreamEvent{}, fmt.Errorf("decode Codex output item: %w", err)
		}
		if err := validateOutputItems([]protocols.Item{item}); err != nil {
			return protocols.StreamEvent{}, err
		}
		event.Item = &item
	}
	if len(wire.Part) > 0 && !bytes.Equal(wire.Part, []byte("null")) {
		var part protocols.ContentPart
		if err := json.Unmarshal(wire.Part, &part); err != nil {
			return protocols.StreamEvent{}, fmt.Errorf("decode Codex content part: %w", err)
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
			if err := json.Unmarshal(wire.Code, &code); err != nil {
				var number json.Number
				if json.Unmarshal(wire.Code, &number) == nil {
					code = number.String()
				}
			}
		}
		event.Error = &protocols.APIError{Message: wire.Message, Type: "api_error", Param: wire.Param, Code: code}
	}
	return event, nil
}

func validateOutputItems(items []protocols.Item) error {
	for _, item := range items {
		switch item.Type {
		case "message":
			for _, content := range item.Content {
				if content.Type != "output_text" {
					return fmt.Errorf("unsupported Codex output content type %q", content.Type)
				}
			}
		case "reasoning":
			for _, summary := range item.Summary {
				if summary.Type != "summary_text" {
					return fmt.Errorf("unsupported Codex reasoning summary type %q", summary.Type)
				}
			}
		case "function_call", "custom_tool_call":
		default:
			return fmt.Errorf("unsupported Codex output item type %q", item.Type)
		}
	}
	return nil
}

func supportedEventType(eventType string) bool {
	switch eventType {
	case "response.created", "response.queued", "response.in_progress",
		"response.completed", "response.incomplete", "response.failed", "error",
		"response.output_item.added", "response.output_item.done",
		"response.content_part.added", "response.content_part.done",
		"response.output_text.delta", "response.output_text.done",
		"response.reasoning_text.delta", "response.reasoning_text.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return true
	default:
		return false
	}
}

func eventTypeFromWire(eventType string) protocols.EventType {
	return map[string]protocols.EventType{
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
	}[eventType]
}

func statusFromError(apiErr *protocols.APIError) int {
	if apiErr == nil {
		return 0
	}
	if status, err := strconv.Atoi(apiErr.Code); err == nil && status >= 400 && status <= 599 {
		return status
	}
	return 0
}
