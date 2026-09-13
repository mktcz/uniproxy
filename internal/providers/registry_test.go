package providers

import (
	"context"
	"testing"

	"github.com/mktcz/uniproxy/internal/protocols"
)

type registryTestProvider struct {
	name   string
	models []protocols.ModelInfo
}

func (p registryTestProvider) Name() string                  { return p.name }
func (p registryTestProvider) Models() []protocols.ModelInfo { return p.models }
func (p registryTestProvider) Generate(context.Context, protocols.Request) (protocols.Response, error) {
	return protocols.Response{}, nil
}
func (p registryTestProvider) Stream(context.Context, protocols.Request) (<-chan protocols.StreamResult, error) {
	return nil, nil
}

func TestRegistryResolvesModelsAcrossAdapters(t *testing.T) {
	registry := NewRegistry()
	registry.Register(registryTestProvider{name: "codex", models: []protocols.ModelInfo{
		{ID: "codex/gpt-5.6-luna", OwnedBy: "codex"},
	}})
	registry.Register(registryTestProvider{name: "opencode", models: []protocols.ModelInfo{
		{ID: "opencode/free-model", OwnedBy: "opencode"},
		{ID: "codex/gpt-5.6-luna", OwnedBy: "duplicate"},
	}})
	registry.Register(registryTestProvider{name: "cursor", models: nil})

	codex, ok := registry.Resolve("codex/gpt-5.6-luna")
	if !ok || codex.Name() != "codex" {
		t.Fatalf("codex resolution = %v, %t", codex, ok)
	}
	if unprefixed, ok := registry.Resolve("gpt-5.6-luna"); !ok || unprefixed.Name() != "codex" {
		t.Fatalf("unprefixed codex resolution = %v, %t", unprefixed, ok)
	}
	opencode, ok := registry.Resolve("opencode/free-model")
	if !ok || opencode.Name() != "opencode" {
		t.Fatalf("opencode resolution = %v, %t", opencode, ok)
	}
	cursor, ok := registry.Resolve("cursor/composer-1")
	if !ok || cursor.Name() != "cursor" {
		t.Fatalf("cursor prefix resolution = %v, %t", cursor, ok)
	}
	if _, ok := registry.Resolve("missing"); ok {
		t.Fatal("unexpected resolution for missing model")
	}

	models := registry.Models()
	if len(models) != 2 || models[0].ID != "codex/gpt-5.6-luna" || models[1].ID != "opencode/free-model" {
		t.Fatalf("combined models = %#v", models)
	}
	wantNames := []string{"codex", "opencode", "cursor"}
	for index, name := range registry.Names() {
		if name != wantNames[index] {
			t.Fatalf("names = %#v", registry.Names())
		}
	}
}
