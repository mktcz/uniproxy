package openai

import (
	"encoding/json"
	"time"

	"github.com/mktcz/uniproxy/internal/protocols"
)

type modelList struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (Codec) EncodeModels(models []protocols.ModelInfo) ([]byte, error) {
	created := time.Now().UTC().Unix()
	data := make([]modelEntry, 0, len(models))
	for _, model := range models {
		data = append(data, modelEntry{
			ID: model.ID, Object: "model", Created: created, OwnedBy: model.OwnedBy,
		})
	}
	return json.Marshal(modelList{Object: "list", Data: data})
}
