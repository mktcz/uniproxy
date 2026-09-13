package anthropic

import (
	"encoding/json"
	"time"

	"github.com/mktcz/uniproxy/internal/protocols"
)

type modelList struct {
	Data    []modelEntry `json:"data"`
	HasMore bool         `json:"has_more"`
	FirstID string       `json:"first_id,omitempty"`
	LastID  string       `json:"last_id,omitempty"`
}

type modelEntry struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

func (Codec) EncodeModels(models []protocols.ModelInfo) ([]byte, error) {
	createdAt := time.Now().UTC().Format(time.RFC3339)
	data := make([]modelEntry, 0, len(models))
	for _, model := range models {
		data = append(data, modelEntry{
			Type: "model", ID: model.ID, DisplayName: model.ID, CreatedAt: createdAt,
		})
	}
	list := modelList{Data: data, HasMore: false}
	if len(data) > 0 {
		list.FirstID = data[0].ID
		list.LastID = data[len(data)-1].ID
	}
	return json.Marshal(list)
}
