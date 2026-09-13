package providers

import (
	"context"
	"strings"

	"github.com/mktcz/uniproxy/internal/protocols"
)

type Provider interface {
	Name() string
	Models() []protocols.ModelInfo
	Generate(context.Context, protocols.Request) (protocols.Response, error)
	Stream(context.Context, protocols.Request) (<-chan protocols.StreamResult, error)
}

type Registry struct {
	byName map[string]Provider
	order  []string
}

func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Provider)}
}

func (r *Registry) Register(p Provider) {
	if p == nil {
		panic("providers: Register nil Provider")
	}
	name := p.Name()
	if name == "" {
		panic("providers: Register Provider with empty name")
	}
	if _, exists := r.byName[name]; !exists {
		r.order = append(r.order, name)
	}
	r.byName[name] = p
}

func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.byName[name]
	return p, ok
}

func (r *Registry) Resolve(model string) (Provider, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, false
	}
	for _, name := range r.order {
		provider := r.byName[name]
		for _, candidate := range provider.Models() {
			if candidate.ID == model {
				return provider, true
			}
		}
	}
	for _, name := range r.order {
		provider := r.byName[name]
		prefix := name + "/"
		for _, candidate := range provider.Models() {
			if strings.TrimPrefix(candidate.ID, prefix) == model {
				return provider, true
			}
		}
	}
	if providerName, _, ok := splitProviderModel(model); ok {
		if provider, exists := r.byName[providerName]; exists {
			return provider, true
		}
	}
	return nil, false
}

func splitProviderModel(model string) (provider, wire string, ok bool) {
	provider, wire, found := strings.Cut(model, "/")
	if !found || provider == "" || wire == "" {
		return "", "", false
	}
	return provider, wire, true
}

func (r *Registry) Names() []string {
	return append([]string(nil), r.order...)
}

func (r *Registry) Models() []protocols.ModelInfo {
	seen := make(map[string]struct{})
	var models []protocols.ModelInfo
	for _, name := range r.order {
		for _, model := range r.byName[name].Models() {
			if _, exists := seen[model.ID]; exists {
				continue
			}
			seen[model.ID] = struct{}{}
			models = append(models, model)
		}
	}
	return models
}
