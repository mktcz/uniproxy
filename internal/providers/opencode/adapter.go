package opencode

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/mktcz/uniproxy/internal/protocols"
	"github.com/mktcz/uniproxy/internal/store"
)

const (
	DefaultZenBaseURL = "https://opencode.ai/zen/v1"
	DefaultGoBaseURL  = "https://opencode.ai/zen/go/v1"
	DefaultGoRootURL  = "https://opencode.ai/zen/go"
)

type Adapter struct {
	creds   *store.Store
	client  *http.Client
	zenBase string
	goBase  string
	goRoot  string

	zenModelsURL string
	goModelsURL  string
	catalogURL   string

	mu    sync.Mutex
	cache *Credential

	catalogMu    sync.RWMutex
	catalogOnce  sync.Once
	catalogReady bool
	zenModels    map[string]modelMeta
	goModels     map[string]modelMeta
	modelList    []protocols.ModelInfo
}

func New(authFile string, client *http.Client) *Adapter {
	if client == nil {
		client = http.DefaultClient
	}
	return &Adapter{
		creds:        store.New(authFile),
		client:       client,
		zenBase:      DefaultZenBaseURL,
		goBase:       DefaultGoBaseURL,
		goRoot:       DefaultGoRootURL,
		zenModelsURL: zenModelsURL,
		goModelsURL:  goModelsURL,
		catalogURL:   catalogURL,
		zenModels:    map[string]modelMeta{},
		goModels:     map[string]modelMeta{},
	}
}

func (a *Adapter) Name() string { return "opencode" }

func (a *Adapter) Generate(ctx context.Context, request protocols.Request) (protocols.Response, error) {
	response, route, err := a.open(ctx, request)
	if err != nil {
		return protocols.Response{}, err
	}
	defer response.Body.Close()

	items := map[int]protocols.Item{}
	terminal := false
	var result protocols.Response
	err = a.consumeStream(response.Body, route, request.Model, func(event protocols.StreamEvent) error {
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
				return fmt.Errorf("terminal OpenCode event has no response")
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
			message := "OpenCode response failed"
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
		return protocols.Response{}, &protocols.UpstreamError{
			Status: http.StatusBadGateway, Message: "OpenCode stream ended without a terminal response", Code: "incomplete_stream",
		}
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
	response, route, err := a.open(ctx, request)
	if err != nil {
		return nil, err
	}
	results := make(chan protocols.StreamResult)
	go func() {
		defer close(results)
		defer response.Body.Close()
		terminal := false
		items := map[int]protocols.Item{}
		err := a.consumeStream(response.Body, route, request.Model, func(event protocols.StreamEvent) error {
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
			results <- protocols.StreamResult{Err: &protocols.UpstreamError{
				Status: http.StatusBadGateway, Message: "OpenCode stream ended without a terminal response", Code: "incomplete_stream",
			}}
		}
	}()
	return results, nil
}

func (a *Adapter) open(ctx context.Context, request protocols.Request) (*http.Response, Route, error) {
	a.ensureCatalog(ctx)
	route := a.resolveRoute(request.Model)
	apiKey, err := a.resolveAPIKey(route)
	if err != nil {
		return nil, Route{}, err
	}
	session, err := sessionID(request)
	if err != nil {
		return nil, Route{}, fmt.Errorf("generate OpenCode session id: %w", err)
	}

	var (
		body []byte
		url  string
	)
	switch route.Protocol {
	case ProtocolChat:
		body, err = buildChatBody(request, route.WireModel)
		url = strings.TrimRight(a.baseFor(route), "/") + "/chat/completions"
	case ProtocolResponses:
		body, err = buildResponsesBody(request, route.WireModel)
		url = strings.TrimRight(a.baseFor(route), "/") + "/responses"
	case ProtocolMessages:
		body, err = buildMessagesBody(request, route.WireModel)
		url = strings.TrimRight(a.messagesBaseFor(route), "/") + "/v1/messages"
	case ProtocolGemini:
		body, err = buildGeminiBody(request)
		url = strings.TrimRight(a.baseFor(route), "/") + "/models/" + route.WireModel + ":streamGenerateContent?alt=sse"
	default:
		return nil, Route{}, fmt.Errorf("unsupported OpenCode protocol %q", route.Protocol)
	}
	if err != nil {
		return nil, Route{}, err
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, Route{}, fmt.Errorf("create OpenCode request: %w", err)
	}
	httpRequest.Header = http.Header{}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("User-Agent", UserAgent)
	httpRequest.Header.Set("x-opencode-session", session)
	applyAuthHeaders(httpRequest.Header, route.Protocol, apiKey)

	response, err := a.client.Do(httpRequest)
	if err != nil {
		return nil, Route{}, fmt.Errorf("send OpenCode request: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		message, code := upstreamError(payload)
		return nil, Route{}, &protocols.UpstreamError{Status: response.StatusCode, Message: message, Code: code}
	}
	return response, route, nil
}

func (a *Adapter) baseFor(route Route) string {
	if route.Plan == PlanGo {
		return a.goBase
	}
	return a.zenBase
}

func (a *Adapter) messagesBaseFor(route Route) string {
	if route.Plan == PlanGo {
		return a.goRoot
	}
	return strings.TrimSuffix(a.zenBase, "/v1")
}

func (a *Adapter) consumeStream(body io.Reader, route Route, clientModel string, emit func(protocols.StreamEvent) error) error {
	switch route.Protocol {
	case ProtocolChat:
		state := newChatStreamState(clientModel)
		done, err := readRawSSE(body, func(_ string, payload []byte) error {
			return state.consume(payload, emit)
		})
		if err != nil {
			return err
		}
		if !done {
			return &protocols.UpstreamError{
				Status: http.StatusBadGateway, Message: "OpenCode chat stream ended before [DONE]", Code: "incomplete_stream",
			}
		}
		return state.finish(emit)
	case ProtocolResponses:
		terminal := false
		_, err := readRawSSE(body, func(eventName string, payload []byte) error {
			event, err := decodeResponsesEvent(payload, eventName)
			if err != nil {
				return err
			}
			if event.Type == "" {
				return nil
			}
			switch event.Type {
			case protocols.EventResponseCompleted, protocols.EventResponseIncomplete, protocols.EventResponseFailed, protocols.EventError:
				terminal = true
			}
			return emit(event)
		})
		if err != nil {
			return err
		}
		if !terminal {
			return &protocols.UpstreamError{
				Status: http.StatusBadGateway, Message: "OpenCode responses stream ended without a terminal response", Code: "incomplete_stream",
			}
		}
		return nil
	case ProtocolMessages:
		state := newAnthropicStreamState(clientModel)
		items := map[int]protocols.Item{}
		_, err := readRawSSE(body, func(_ string, payload []byte) error {
			return state.consume(payload, func(event protocols.StreamEvent) error {
				if event.Type == protocols.EventOutputItemDone && event.Item != nil {
					index := 0
					if event.OutputIndex != nil {
						index = *event.OutputIndex
					}
					items[index] = *event.Item
				}
				return emit(event)
			})
		})
		if err != nil {
			return err
		}
		if !state.stopped {
			return &protocols.UpstreamError{
				Status: http.StatusBadGateway, Message: "OpenCode messages stream ended before message_stop", Code: "incomplete_stream",
			}
		}
		return state.finish(emit, items)
	case ProtocolGemini:
		state := newGeminiStreamState(clientModel)
		_, err := readRawSSE(body, func(_ string, payload []byte) error {
			return state.consume(payload, emit)
		})
		if err != nil {
			return err
		}
		return state.finish(emit)
	default:
		return fmt.Errorf("unsupported OpenCode protocol %q", route.Protocol)
	}
}

func (a *Adapter) SetBases(zenBase, goBase, goRoot string) {
	if zenBase != "" {
		a.zenBase = strings.TrimRight(zenBase, "/")
		a.zenModelsURL = a.zenBase + "/models"
	}
	if goBase != "" {
		a.goBase = strings.TrimRight(goBase, "/")
		a.goModelsURL = a.goBase + "/models"
	}
	if goRoot != "" {
		a.goRoot = strings.TrimRight(goRoot, "/")
	}
}
