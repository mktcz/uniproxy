package cursor

import (
	"context"
	"fmt"

	"github.com/mktcz/uniproxy/internal/protocols"
)

const ModelPrefix = "cursor/"

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (a *Adapter) Name() string { return "cursor" }

func (a *Adapter) Models() []protocols.ModelInfo { return nil }

func (a *Adapter) Generate(context.Context, protocols.Request) (protocols.Response, error) {
	return protocols.Response{}, fmt.Errorf("cursor provider is not implemented")
}

func (a *Adapter) Stream(context.Context, protocols.Request) (<-chan protocols.StreamResult, error) {
	return nil, fmt.Errorf("cursor provider is not implemented")
}
