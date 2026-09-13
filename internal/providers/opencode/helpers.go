package opencode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mktcz/uniproxy/internal/utils/randomid"
	"github.com/mktcz/uniproxy/internal/protocols"
)

const maxSSEEventBytes = 16 << 20

func sessionID(request protocols.Request) (string, error) {
	if request.PromptCacheKey != "" {
		return request.PromptCacheKey, nil
	}
	return randomid.UUID()
}

func applyAuthHeaders(header http.Header, protocol Protocol, apiKey string) {
	switch protocol {
	case ProtocolMessages:
		header.Set("x-api-key", apiKey)
		header.Set("anthropic-version", "2023-06-01")
	case ProtocolGemini:
		header.Set("x-goog-api-key", apiKey)
	default:
		header.Set("Authorization", "Bearer "+apiKey)
	}
}

func upstreamError(payload []byte) (string, string) {
	message := "OpenCode upstream request failed"
	code := "upstream_error"
	var envelope struct {
		Error struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
		Type    string `json:"type"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if json.Unmarshal(payload, &envelope) == nil {
		if envelope.Error.Message != "" {
			message = envelope.Error.Message
		} else if envelope.Message != "" {
			message = envelope.Message
		} else if envelope.Detail != "" {
			message = envelope.Detail
		}
		if envelope.Error.Type != "" {
			code = envelope.Error.Type
		} else if envelope.Type != "" {
			code = envelope.Type
		}
		if len(envelope.Error.Code) > 0 {
			_ = json.Unmarshal(envelope.Error.Code, &code)
		}
	}
	return message, code
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

func orderedItems(items map[int]protocols.Item) []protocols.Item {
	if len(items) == 0 {
		return nil
	}
	max := -1
	for index := range items {
		if index > max {
			max = index
		}
	}
	result := make([]protocols.Item, 0, len(items))
	for i := 0; i <= max; i++ {
		if item, ok := items[i]; ok {
			result = append(result, item)
		}
	}
	return result
}

func readRawSSE(reader io.Reader, consume func(eventName string, payload []byte) error) (bool, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxSSEEventBytes+1024)
	var data bytes.Buffer
	eventName := ""
	done := false
	dispatch := func() error {
		if data.Len() == 0 {
			eventName = ""
			return nil
		}
		payload := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
		data.Reset()
		trimmed := bytes.TrimSpace(payload)
		if len(trimmed) == 0 {
			eventName = ""
			return nil
		}
		if bytes.Equal(trimmed, []byte("[DONE]")) {
			done = true
			eventName = ""
			return nil
		}
		if done {
			eventName = ""
			return nil
		}
		name := eventName
		eventName = ""
		return consume(name, append([]byte(nil), payload...))
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				return done, err
			}
			if done {
				return true, nil
			}
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if data.Len()+len(value)+1 > maxSSEEventBytes {
				return done, fmt.Errorf("OpenCode SSE event exceeds %d bytes", maxSSEEventBytes)
			}
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		return done, fmt.Errorf("read OpenCode SSE: %w", err)
	}
	if err := dispatch(); err != nil {
		return done, err
	}
	return done, nil
}

func textFromParts(parts []protocols.ContentPart) string {
	var b strings.Builder
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text":
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func intPtr(v int) *int { return &v }
