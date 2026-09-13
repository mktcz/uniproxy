package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mktcz/uniproxy/internal/protocols"
	"github.com/mktcz/uniproxy/internal/store"
	"github.com/mktcz/uniproxy/internal/utils/randomid"
)

const (
	DefaultBaseURL = "https://chatgpt.com/backend-api/codex"
	UserAgent      = "codex-tui/0.154.0 (Linux; x86_64) (codex-tui; 0.154.0)"
	Originator     = "codex-tui"
	ModelPrefix    = "codex/"
)

var models = []protocols.ModelInfo{
	{ID: ModelPrefix + "gpt-6-astra", OwnedBy: "codex"},
	{ID: ModelPrefix + "gpt-5.6-sol", OwnedBy: "codex"},
	{ID: ModelPrefix + "gpt-5.6-terra", OwnedBy: "codex"},
	{ID: ModelPrefix + "gpt-5.6-luna", OwnedBy: "codex"},
	{ID: ModelPrefix + "gpt-5.5", OwnedBy: "codex"},
}

type Adapter struct {
	creds   *store.Store
	oauth   *OAuthClient
	client  *http.Client
	baseURL string
	mu      sync.Mutex
	cache   *Credential
	now     func() time.Time
}

func New(authFile string, client *http.Client) *Adapter {
	if client == nil {
		client = http.DefaultClient
	}
	return &Adapter{
		creds:   store.New(authFile),
		oauth:   NewOAuthClient(client),
		client:  client,
		baseURL: DefaultBaseURL,
		now:     time.Now,
	}
}

func (a *Adapter) Name() string { return "codex" }

func (a *Adapter) Models() []protocols.ModelInfo {
	out := make([]protocols.ModelInfo, len(models))
	copy(out, models)
	return out
}

func (a *Adapter) Generate(ctx context.Context, request protocols.Request) (protocols.Response, error) {
	response, err := a.open(ctx, request)
	if err != nil {
		return protocols.Response{}, err
	}
	defer response.Body.Close()
	items := map[int]protocols.Item{}
	terminal := false
	var result protocols.Response
	err = readSSE(response.Body, func(event protocols.StreamEvent) error {
		if event.Type == protocols.EventOutputItemDone && event.Item != nil {
			index := len(items)
			if event.OutputIndex != nil {
				index = *event.OutputIndex
			}
			items[index] = *event.Item
		}
		switch event.Type {
		case protocols.EventResponseCompleted, protocols.EventResponseIncomplete:
			terminal = true
			if event.Response == nil {
				return fmt.Errorf("terminal Codex event has no response")
			}
			result = *event.Response
			if len(result.Output) == 0 && len(items) > 0 {
				result.Output = orderedItems(items)
			}
		case protocols.EventResponseFailed, protocols.EventError:
			terminal = true
			apiErr := event.Error
			if apiErr == nil && event.Response != nil {
				apiErr = event.Response.Error
			}
			message := "Codex response failed"
			code := "upstream_stream_error"
			status := http.StatusBadGateway
			if apiErr != nil {
				if apiErr.Message != "" {
					message = apiErr.Message
				}
				if apiErr.Code != "" {
					code = apiErr.Code
				}
				if parsed := statusFromError(apiErr); parsed != 0 {
					status = parsed
				}
			}
			return &protocols.UpstreamError{Status: status, Message: message, Code: code}
		}
		return nil
	})
	if err != nil {
		return protocols.Response{}, err
	}
	if !terminal {
		return protocols.Response{}, &protocols.UpstreamError{Status: http.StatusBadGateway, Message: "Codex stream ended without a terminal response", Code: "incomplete_stream"}
	}
	if result.Object == "" {
		result.Object = "response"
	}
	if result.Model == "" {
		result.Model = request.Model
	}
	return result, nil
}

func (a *Adapter) Stream(ctx context.Context, request protocols.Request) (<-chan protocols.StreamResult, error) {
	response, err := a.open(ctx, request)
	if err != nil {
		return nil, err
	}
	results := make(chan protocols.StreamResult)
	go func() {
		defer close(results)
		defer response.Body.Close()
		terminal := false
		items := map[int]protocols.Item{}
		err := readSSE(response.Body, func(event protocols.StreamEvent) error {
			if event.Type == protocols.EventOutputItemDone && event.Item != nil {
				index := len(items)
				if event.OutputIndex != nil {
					index = *event.OutputIndex
				}
				items[index] = *event.Item
			}
			switch event.Type {
			case protocols.EventResponseCompleted, protocols.EventResponseIncomplete:
				if event.Response != nil && len(event.Response.Output) == 0 && len(items) > 0 {
					event.Response.Output = orderedItems(items)
				}
				terminal = true
			case protocols.EventResponseFailed, protocols.EventError:
				terminal = true
			}
			select {
			case results <- protocols.StreamResult{Event: &event}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			results <- protocols.StreamResult{Err: err}
		} else if !terminal {
			results <- protocols.StreamResult{Err: &protocols.UpstreamError{Status: http.StatusBadGateway, Message: "Codex stream ended without a terminal response", Code: "incomplete_stream"}}
		}
	}()
	return results, nil
}

func (a *Adapter) open(ctx context.Context, request protocols.Request) (*http.Response, error) {
	credential, err := a.token(ctx)
	if err != nil {
		return nil, err
	}
	body, err := buildRequestBody(request)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Codex request: %w", err)
	}
	requestID, err := randomid.UUID()
	if err != nil {
		return nil, fmt.Errorf("generate request ID: %w", err)
	}
	sessionID := request.PromptCacheKey
	if sessionID == "" {
		sessionID = requestID
	}
	httpRequest.Header = http.Header{}
	httpRequest.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	httpRequest.Header.Set("Chatgpt-Account-Id", credential.AccountID)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("Connection", "Keep-Alive")
	httpRequest.Header.Set("User-Agent", UserAgent)
	httpRequest.Header.Set("Originator", Originator)
	httpRequest.Header.Set("Session-Id", sessionID)
	httpRequest.Header.Set("X-Client-Request-Id", requestID)
	response, err := a.client.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("send Codex request: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		message, code := upstreamError(payload)
		return nil, &protocols.UpstreamError{Status: response.StatusCode, Message: message, Code: code}
	}
	return response, nil
}

func buildRequestBody(request protocols.Request) ([]byte, error) {
	input := make([]map[string]any, 0, len(request.Input))
	seenIDs := map[string]struct{}{}
	for _, item := range request.Input {
		if item.Type == "reasoning" && item.EncryptedContent != "" && len([]rune(item.ID)) > 64 {
			continue
		}
		wire, err := encodeInputItem(item, seenIDs)
		if err != nil {
			return nil, err
		}
		input = append(input, wire)
	}
	payload := map[string]any{
		"model": strings.TrimPrefix(strings.TrimSpace(request.Model), ModelPrefix), "instructions": request.Instructions, "input": input,
		"stream": true, "store": false, "parallel_tool_calls": request.ParallelToolCalls,
		"include": []string{"reasoning.encrypted_content"},
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
	return json.Marshal(payload)
}

func encodeInputItem(item protocols.Item, seen map[string]struct{}) (map[string]any, error) {
	wire := map[string]any{"type": item.Type}
	if item.ID != "" {
		wire["id"] = normalizedID(item.Type, item.ID, seen)
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
			encoded, err := encodeContentPart(part)
			if err != nil {
				return nil, err
			}
			content = append(content, encoded)
		}
		wire["role"], wire["content"] = role, content
		if item.Phase != "" {
			wire["phase"] = item.Phase
		}
	case "function_call":
		wire["call_id"], wire["name"], wire["arguments"] = item.CallID, item.Name, item.Arguments
	case "function_call_output":
		wire["call_id"] = item.CallID
		if len(item.OutputContent) > 0 {
			output := make([]map[string]any, 0, len(item.OutputContent))
			for _, part := range item.OutputContent {
				encoded, err := encodeContentPart(part)
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
		return nil, fmt.Errorf("unsupported canonical item type %q", item.Type)
	}
	return wire, nil
}

func encodeContentPart(part protocols.ContentPart) (map[string]any, error) {
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
	case "input_audio":
		return map[string]any{"type": "input_audio", "data": part.AudioData, "format": part.AudioFormat}, nil
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
		return nil, fmt.Errorf("unsupported content part type %q", part.Type)
	}
}

func normalizedID(itemType, original string, seen map[string]struct{}) string {
	prefix := map[string]string{"message": "msg", "reasoning": "rs", "function_call": "fc", "custom_tool_call": "ctc"}[itemType]
	id := original
	if prefix != "" && !strings.HasPrefix(id, prefix) {
		id = prefix + "_" + id
	}
	if len([]rune(id)) > 64 {
		sum := sha256.Sum256([]byte(id))
		suffix := "_" + hex.EncodeToString(sum[:8])
		runes := []rune(id)
		id = string(runes[:64-len(suffix)]) + suffix
	}
	base := id
	for attempt := 0; ; attempt++ {
		if _, exists := seen[id]; !exists {
			break
		}
		sum := sha256.Sum256([]byte(itemType + "\x00" + original + fmt.Sprintf("\x00%d", attempt)))
		suffix := "_" + hex.EncodeToString(sum[:8])
		runes := []rune(base)
		keep := 64 - len(suffix)
		if len(runes) < keep {
			keep = len(runes)
		}
		id = string(runes[:keep]) + suffix
	}
	seen[id] = struct{}{}
	return id
}

func orderedItems(items map[int]protocols.Item) []protocols.Item {
	indices := make([]int, 0, len(items))
	for index := range items {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	result := make([]protocols.Item, 0, len(indices))
	for _, index := range indices {
		result = append(result, items[index])
	}
	return result
}

func upstreamError(payload []byte) (string, string) {
	message := "Codex upstream request failed"
	code := "upstream_error"
	var envelope struct {
		Error struct {
			Message string          `json:"message"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(payload, &envelope) == nil {
		if envelope.Error.Message != "" {
			message = envelope.Error.Message
		} else if envelope.Detail != "" {
			message = envelope.Detail
		}
		if len(envelope.Error.Code) > 0 {
			_ = json.Unmarshal(envelope.Error.Code, &code)
		}
	}
	return message, code
}
