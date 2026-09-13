package protocols

import (
	"encoding/json"
	"fmt"
)

type Request struct {
	Model             string
	Instructions      string
	Input             []Item
	Tools             []Tool
	ToolChoice        *ToolChoice
	ParallelToolCalls bool
	Reasoning         *ReasoningConfig
	PromptCacheKey    string
	MaxOutputTokens   *int
}

type Item struct {
	Type             string        `json:"type"`
	ID               string        `json:"id,omitempty"`
	Role             string        `json:"role,omitempty"`
	Phase            string        `json:"phase,omitempty"`
	Status           string        `json:"status,omitempty"`
	Content          []ContentPart `json:"content,omitempty"`
	Summary          []SummaryPart `json:"summary,omitempty"`
	EncryptedContent string        `json:"encrypted_content,omitempty"`
	CallID           string        `json:"call_id,omitempty"`
	Name             string        `json:"name,omitempty"`
	Arguments        string        `json:"arguments,omitempty"`
	Input            string        `json:"input,omitempty"`
	Output           string        `json:"output,omitempty"`
	OutputContent    []ContentPart `json:"-"`
}

type ContentPart struct {
	Type        string            `json:"type"`
	Text        string            `json:"text,omitempty"`
	Annotations []json.RawMessage `json:"annotations,omitempty"`
	ImageURL    string            `json:"image_url,omitempty"`
	Detail      string            `json:"detail,omitempty"`
	FileID      string            `json:"file_id,omitempty"`
	AudioData   string            `json:"audio_data,omitempty"`
	AudioFormat string            `json:"audio_format,omitempty"`
	FileData    string            `json:"file_data,omitempty"`
	FileURL     string            `json:"file_url,omitempty"`
	Filename    string            `json:"filename,omitempty"`
}

type SummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Format      json.RawMessage `json:"format,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ToolChoice struct {
	Mode string
	Name string
}

type ReasoningConfig struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type Response struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	CompletedAt       *int64             `json:"completed_at,omitempty"`
	Status            string             `json:"status"`
	Model             string             `json:"model"`
	Output            []Item             `json:"output"`
	Usage             *Usage             `json:"usage,omitempty"`
	Error             *APIError          `json:"error,omitempty"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`
}

type Usage struct {
	InputTokens         int                 `json:"input_tokens"`
	InputTokensDetails  *InputTokenDetails  `json:"input_tokens_details,omitempty"`
	OutputTokens        int                 `json:"output_tokens"`
	OutputTokensDetails *OutputTokenDetails `json:"output_tokens_details,omitempty"`
	TotalTokens         int                 `json:"total_tokens"`
}

type InputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type OutputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type IncompleteDetails struct {
	Reason string `json:"reason"`
}

type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

type EventType string

const (
	EventResponseCreated       EventType = "response_created"
	EventResponseQueued        EventType = "response_queued"
	EventResponseInProgress    EventType = "response_in_progress"
	EventResponseCompleted     EventType = "response_completed"
	EventResponseIncomplete    EventType = "response_incomplete"
	EventResponseFailed        EventType = "response_failed"
	EventError                 EventType = "error"
	EventOutputItemAdded       EventType = "output_item_added"
	EventOutputItemDone        EventType = "output_item_done"
	EventContentPartAdded      EventType = "content_part_added"
	EventContentPartDone       EventType = "content_part_done"
	EventTextDelta             EventType = "text_delta"
	EventTextDone              EventType = "text_done"
	EventReasoningDelta        EventType = "reasoning_delta"
	EventReasoningDone         EventType = "reasoning_done"
	EventReasoningPartAdded    EventType = "reasoning_part_added"
	EventReasoningPartDone     EventType = "reasoning_part_done"
	EventReasoningSummaryDelta EventType = "reasoning_summary_delta"
	EventReasoningSummaryDone  EventType = "reasoning_summary_done"
	EventToolArgumentsDelta    EventType = "tool_arguments_delta"
	EventToolArgumentsDone     EventType = "tool_arguments_done"
	EventCustomToolInputDelta  EventType = "custom_tool_input_delta"
	EventCustomToolInputDone   EventType = "custom_tool_input_done"
)

type StreamEvent struct {
	Type           EventType
	SequenceNumber int
	Response       *Response
	Item           *Item
	Part           *ContentPart
	OutputIndex    *int
	ContentIndex   *int
	ItemID         string
	Delta          string
	Text           string
	Arguments      string
	Input          string
	Error          *APIError
}

type StreamResult struct {
	Event *StreamEvent
	Err   error
}

type StreamMeta struct {
	Model string
}

type ModelInfo struct {
	ID      string
	OwnedBy string
}

type ClientError struct {
	Status  int
	Message string
	Param   string
	Code    string
}

func (e *ClientError) Error() string { return e.Message }

type UpstreamError struct {
	Status  int
	Message string
	Code    string
}

func (e *UpstreamError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("upstream returned status %d: %s", e.Status, e.Message)
	}
	return e.Message
}
