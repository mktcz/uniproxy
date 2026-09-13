package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/mktcz/uniproxy/internal/protocols"
)

type StreamEncoder struct {
	model        string
	messageID    string
	started      bool
	closed       bool
	blockIndex   int
	textOpen     bool
	thinkingOpen bool
	toolOpen     bool
	stopReason   string
	inputTokens  int
	outputTokens int
}

func NewStreamEncoder(model string) *StreamEncoder {
	return &StreamEncoder{model: model, stopReason: "end_turn"}
}

func (e *StreamEncoder) Encode(event protocols.StreamEvent) ([][]byte, error) {
	if e.closed {
		return nil, nil
	}
	var frames [][]byte
	ensureStart := func() error {
		if e.started {
			return nil
		}
		e.started = true
		e.messageID = "msg_uniproxy"
		if event.Response != nil && event.Response.ID != "" {
			e.messageID = event.Response.ID
		}
		frame, err := EncodeSSE("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": e.messageID, "type": "message", "role": "assistant", "model": e.model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
		if err != nil {
			return err
		}
		frames = append(frames, frame)
		return nil
	}

	switch event.Type {
	case protocols.EventResponseCreated, protocols.EventResponseQueued, protocols.EventResponseInProgress:
		if err := ensureStart(); err != nil {
			return nil, err
		}
		if event.Response != nil {
			if event.Response.ID != "" {
				e.messageID = event.Response.ID
			}
			if event.Response.Model != "" {
				e.model = event.Response.Model
			}
		}
	case protocols.EventTextDelta:
		if err := ensureStart(); err != nil {
			return nil, err
		}
		opened, err := e.openText(&frames)
		if err != nil {
			return nil, err
		}
		_ = opened
		frame, err := EncodeSSE("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": e.blockIndex,
			"delta": map[string]any{"type": "text_delta", "text": event.Delta},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
	case protocols.EventTextDone:
		if err := ensureStart(); err != nil {
			return nil, err
		}
		if !e.textOpen {
			if _, err := e.openText(&frames); err != nil {
				return nil, err
			}
			if event.Text != "" {
				frame, err := EncodeSSE("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": e.blockIndex,
					"delta": map[string]any{"type": "text_delta", "text": event.Text},
				})
				if err != nil {
					return nil, err
				}
				frames = append(frames, frame)
			}
		}
		if err := e.closeBlock(&frames); err != nil {
			return nil, err
		}
	case protocols.EventReasoningDelta, protocols.EventReasoningSummaryDelta:
		if err := ensureStart(); err != nil {
			return nil, err
		}
		if _, err := e.openThinking(&frames); err != nil {
			return nil, err
		}
		frame, err := EncodeSSE("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": e.blockIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": event.Delta},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
	case protocols.EventReasoningDone, protocols.EventReasoningSummaryDone, protocols.EventReasoningPartDone:
		if err := ensureStart(); err != nil {
			return nil, err
		}
		if e.thinkingOpen {
			if event.Text != "" && event.Type != protocols.EventReasoningPartDone {
				frame, err := EncodeSSE("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": e.blockIndex,
					"delta": map[string]any{"type": "thinking_delta", "thinking": event.Text},
				})
				if err != nil {
					return nil, err
				}
				frames = append(frames, frame)
			}
			if err := e.closeBlock(&frames); err != nil {
				return nil, err
			}
		}
	case protocols.EventOutputItemAdded:
		if event.Item == nil {
			return frames, nil
		}
		if err := ensureStart(); err != nil {
			return nil, err
		}
		switch event.Item.Type {
		case "function_call":
			if err := e.closeOpenBlocks(&frames); err != nil {
				return nil, err
			}
			e.toolOpen = true
			e.stopReason = "tool_use"
			var input any = map[string]any{}
			frame, err := EncodeSSE("content_block_start", map[string]any{
				"type": "content_block_start", "index": e.blockIndex,
				"content_block": map[string]any{
					"type": "tool_use", "id": firstNonEmpty(event.Item.CallID, event.Item.ID),
					"name": event.Item.Name, "input": input,
				},
			})
			if err != nil {
				return nil, err
			}
			frames = append(frames, frame)
		case "message":
			if _, err := e.openText(&frames); err != nil {
				return nil, err
			}
		case "reasoning":
			if _, err := e.openThinking(&frames); err != nil {
				return nil, err
			}
		}
	case protocols.EventToolArgumentsDelta:
		if err := ensureStart(); err != nil {
			return nil, err
		}
		if !e.toolOpen {
			return frames, nil
		}
		frame, err := EncodeSSE("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": e.blockIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": event.Delta},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
	case protocols.EventToolArgumentsDone, protocols.EventOutputItemDone:
		if event.Type == protocols.EventOutputItemDone && event.Item != nil && event.Item.Type == "function_call" {
			e.stopReason = "tool_use"
		}
		if e.toolOpen {
			if err := e.closeBlock(&frames); err != nil {
				return nil, err
			}
		}
	case protocols.EventResponseCompleted, protocols.EventResponseIncomplete, protocols.EventResponseFailed:
		if err := ensureStart(); err != nil {
			return nil, err
		}
		if err := e.closeOpenBlocks(&frames); err != nil {
			return nil, err
		}
		if event.Response != nil {
			if event.Response.Model != "" {
				e.model = event.Response.Model
			}
			if event.Response.Usage != nil {
				e.inputTokens = event.Response.Usage.InputTokens
				e.outputTokens = event.Response.Usage.OutputTokens
			}
			e.stopReason = stopReason(*event.Response)
		}
		delta, err := EncodeSSE("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": e.stopReason, "stop_sequence": nil},
			"usage": map[string]any{"input_tokens": e.inputTokens, "output_tokens": e.outputTokens},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, delta)
		stop, err := EncodeSSE("message_stop", map[string]any{"type": "message_stop"})
		if err != nil {
			return nil, err
		}
		frames = append(frames, stop)
		e.closed = true
	case protocols.EventError:
		if err := ensureStart(); err != nil {
			return nil, err
		}
		message := "upstream request failed"
		if event.Error != nil && event.Error.Message != "" {
			message = event.Error.Message
		}
		frame, err := EncodeSSE("error", map[string]any{
			"type": "error", "error": map[string]any{"type": "api_error", "message": message},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
		e.closed = true
	}
	return frames, nil
}

func (e *StreamEncoder) openText(frames *[][]byte) (bool, error) {
	if e.textOpen {
		return false, nil
	}
	if err := e.closeOpenBlocks(frames); err != nil {
		return false, err
	}
	e.textOpen = true
	frame, err := EncodeSSE("content_block_start", map[string]any{
		"type": "content_block_start", "index": e.blockIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	if err != nil {
		return false, err
	}
	*frames = append(*frames, frame)
	return true, nil
}

func (e *StreamEncoder) openThinking(frames *[][]byte) (bool, error) {
	if e.thinkingOpen {
		return false, nil
	}
	if err := e.closeOpenBlocks(frames); err != nil {
		return false, err
	}
	e.thinkingOpen = true
	frame, err := EncodeSSE("content_block_start", map[string]any{
		"type": "content_block_start", "index": e.blockIndex,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	})
	if err != nil {
		return false, err
	}
	*frames = append(*frames, frame)
	return true, nil
}

func (e *StreamEncoder) closeOpenBlocks(frames *[][]byte) error {
	if e.textOpen || e.thinkingOpen || e.toolOpen {
		return e.closeBlock(frames)
	}
	return nil
}

func (e *StreamEncoder) closeBlock(frames *[][]byte) error {
	if !e.textOpen && !e.thinkingOpen && !e.toolOpen {
		return nil
	}
	frame, err := EncodeSSE("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": e.blockIndex,
	})
	if err != nil {
		return err
	}
	*frames = append(*frames, frame)
	e.textOpen, e.thinkingOpen, e.toolOpen = false, false, false
	e.blockIndex++
	return nil
}

func EncodeSSE(event string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "event: %s\ndata: %s\n\n", event, data), nil
}
