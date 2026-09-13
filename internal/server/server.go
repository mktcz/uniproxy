package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mktcz/uniproxy/internal/protocols"
	"github.com/mktcz/uniproxy/internal/providers"
)

const maxRequestBytes = 16 << 20

const (
	protocolOpenAI    = "openai"
	protocolAnthropic = "anthropic"
)

type tokenCounter interface {
	CountTokens([]byte) (int, error)
}

type Server struct {
	http      *http.Server
	protocols *protocols.Registry
	providers *providers.Registry
	logger    *slog.Logger
}

func New(address string, readHeaderTimeout, idleTimeout time.Duration, providerRegistry *providers.Registry, protocolRegistry *protocols.Registry, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	server := &Server{
		protocols: protocolRegistry,
		providers: providerRegistry,
		logger:    logger,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", server.handleResponses)
	mux.HandleFunc("/v1/models", server.handleModels)
	mux.HandleFunc("/v1/messages", server.handleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", server.handleCountTokens)
	server.http = &http.Server{
		Addr: address, Handler: mux, ReadHeaderTimeout: readHeaderTimeout, IdleTimeout: idleTimeout,
	}
	return server
}

func (s *Server) ListenAndServe() error { return s.http.ListenAndServe() }

func (s *Server) Serve(listener net.Listener) error { return s.http.Serve(listener) }

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) Handler() http.Handler { return s.http.Handler }

func (s *Server) protocol(name string) protocols.Protocol {
	return s.protocols.MustGet(name)
}

func (s *Server) provider(model string) (providers.Provider, error) {
	p, ok := s.providers.Resolve(model)
	if !ok {
		return nil, &protocols.ClientError{
			Status: http.StatusBadRequest, Message: fmt.Sprintf("unsupported model %q", model),
			Param: "model", Code: "model_not_found",
		}
	}
	return p, nil
}

func (s *Server) generate(ctx context.Context, request protocols.Request) (protocols.Response, error) {
	p, err := s.provider(request.Model)
	if err != nil {
		return protocols.Response{}, err
	}
	return p.Generate(ctx, request)
}

func (s *Server) stream(ctx context.Context, request protocols.Request) (<-chan protocols.StreamResult, error) {
	p, err := s.provider(request.Model)
	if err != nil {
		return nil, err
	}
	return p.Stream(ctx, request)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID, _ := requestID()
	w.Header().Set("X-Request-Id", requestID)
	status := http.StatusOK
	defer func() {
		s.logger.Info("request", "request_id", requestID, "path", "/v1/models", "status", status,
			"latency_ms", time.Since(started).Milliseconds())
	}()
	if r.Method != http.MethodGet {
		status = http.StatusMethodNotAllowed
		w.Header().Set("Allow", http.MethodGet)
		s.writeProtocolError(w, protocolOpenAI, status, &protocols.ClientError{Status: status, Message: "method not allowed", Code: "method_not_allowed"})
		return
	}
	models := s.providers.Models()
	protoName := protocolOpenAI
	if isAnthropicModelsRequest(r) {
		protoName = protocolAnthropic
	}
	body, err := s.protocol(protoName).EncodeModels(models)
	if err != nil {
		status = http.StatusInternalServerError
		s.writeProtocolError(w, protocolOpenAI, status, fmt.Errorf("encode models: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.handleProtocol(w, r, "/v1/responses", protocolOpenAI)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handleProtocol(w, r, "/v1/messages", protocolAnthropic)
}

func (s *Server) handleProtocol(w http.ResponseWriter, r *http.Request, path, protocolName string) {
	started := time.Now()
	requestID, _ := requestID()
	w.Header().Set("X-Request-Id", requestID)
	status := http.StatusOK
	model := ""
	streaming := false
	proto := s.protocol(protocolName)
	defer func() {
		s.logger.Info("request", "request_id", requestID, "path", path, "model", model, "stream", streaming,
			"status", status, "latency_ms", time.Since(started).Milliseconds())
	}()
	if r.Method != http.MethodPost {
		status = http.StatusMethodNotAllowed
		w.Header().Set("Allow", http.MethodPost)
		s.writeProtocolError(w, protocolName, status, &protocols.ClientError{Status: status, Message: "method not allowed", Code: "method_not_allowed"})
		return
	}
	body, err := s.readJSONBody(r, &status)
	if err != nil {
		s.writeProtocolError(w, protocolName, status, err)
		return
	}
	request, stream, err := proto.DecodeRequest(body)
	if err != nil {
		status, _ = proto.EncodeError(err)
		s.writeProtocolError(w, protocolName, status, err)
		return
	}
	model, streaming = request.Model, stream
	if stream {
		results, err := s.stream(r.Context(), request)
		if err != nil {
			status, _ = proto.EncodeError(err)
			s.writeProtocolError(w, protocolName, status, err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		encoder := proto.NewStreamEncoder(protocols.StreamMeta{Model: request.Model})
		for result := range results {
			if result.Err != nil {
				if errors.Is(result.Err, context.Canceled) || r.Context().Err() != nil {
					return
				}
				errorStatus, errorBody := proto.EncodeError(result.Err)
				status = errorStatus
				s.logger.Warn("stream failed", "request_id", requestID, "model", model, "error", result.Err)
				apiErr := decodeAPIError(protocolName, errorBody)
				frames, encodeErr := encoder.Encode(protocols.StreamEvent{Type: protocols.EventError, Error: apiErr})
				if encodeErr != nil {
					return
				}
				for _, frame := range frames {
					_, _ = w.Write(frame)
					if flusher != nil {
						flusher.Flush()
					}
				}
				return
			}
			if result.Event == nil {
				continue
			}
			frames, err := encoder.Encode(*result.Event)
			if err != nil {
				return
			}
			for _, frame := range frames {
				if _, err := w.Write(frame); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		return
	}
	response, err := s.generate(r.Context(), request)
	if err != nil {
		status, _ = proto.EncodeError(err)
		s.writeProtocolError(w, protocolName, status, err)
		return
	}
	encoded, err := proto.EncodeResponse(response)
	if err != nil {
		status = http.StatusInternalServerError
		s.writeProtocolError(w, protocolName, status, fmt.Errorf("encode response: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func decodeAPIError(protocolName string, errorBody []byte) *protocols.APIError {
	switch protocolName {
	case protocolOpenAI:
		var envelope struct {
			Error protocols.APIError `json:"error"`
		}
		_ = json.Unmarshal(errorBody, &envelope)
		return &envelope.Error
	case protocolAnthropic:
		var envelope struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(errorBody, &envelope)
		return &protocols.APIError{Type: envelope.Error.Type, Message: envelope.Error.Message}
	default:
		return &protocols.APIError{Message: string(errorBody)}
	}
}

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	requestID, _ := requestID()
	w.Header().Set("X-Request-Id", requestID)
	status := http.StatusOK
	defer func() {
		s.logger.Info("request", "request_id", requestID, "path", "/v1/messages/count_tokens", "status", status,
			"latency_ms", time.Since(started).Milliseconds())
	}()
	if r.Method != http.MethodPost {
		status = http.StatusMethodNotAllowed
		w.Header().Set("Allow", http.MethodPost)
		s.writeProtocolError(w, protocolAnthropic, status, &protocols.ClientError{Status: status, Message: "method not allowed", Code: "method_not_allowed"})
		return
	}
	body, err := s.readJSONBody(r, &status)
	if err != nil {
		s.writeProtocolError(w, protocolAnthropic, status, err)
		return
	}
	proto := s.protocol(protocolAnthropic)
	counter, ok := proto.(tokenCounter)
	if !ok {
		status = http.StatusNotImplemented
		s.writeProtocolError(w, protocolAnthropic, status, &protocols.ClientError{Status: status, Message: "token counting not supported", Code: "not_implemented"})
		return
	}
	count, err := counter.CountTokens(body)
	if err != nil {
		status, _ = proto.EncodeError(err)
		s.writeProtocolError(w, protocolAnthropic, status, err)
		return
	}
	encoded, err := json.Marshal(map[string]int{"input_tokens": count})
	if err != nil {
		status = http.StatusInternalServerError
		s.writeProtocolError(w, protocolAnthropic, status, fmt.Errorf("encode token count: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func (s *Server) readJSONBody(r *http.Request, status *int) ([]byte, error) {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if mediaType != "application/json" {
		*status = http.StatusUnsupportedMediaType
		return nil, &protocols.ClientError{Status: *status, Message: "Content-Type must be application/json", Code: "unsupported_media_type"}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil {
		*status = http.StatusBadRequest
		return nil, &protocols.ClientError{Status: *status, Message: "could not read request body", Code: "invalid_request"}
	}
	if len(body) > maxRequestBytes {
		*status = http.StatusRequestEntityTooLarge
		return nil, &protocols.ClientError{Status: *status, Message: "request body exceeds 16 MiB", Code: "request_too_large"}
	}
	return body, nil
}

func (s *Server) writeProtocolError(w http.ResponseWriter, protocolName string, fallbackStatus int, err error) {
	status, body := s.protocol(protocolName).EncodeError(err)
	if status == 0 {
		status = fallbackStatus
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func isAnthropicModelsRequest(r *http.Request) bool {
	if strings.TrimSpace(r.Header.Get("Anthropic-Version")) != "" {
		return true
	}
	return strings.HasPrefix(strings.ToLower(r.UserAgent()), "claude-cli")
}

func requestID() (string, error) {
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return fmt.Sprintf("req_%x", value), nil
}
