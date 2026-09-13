package protocols

import "fmt"

type Protocol interface {
	Name() string
	DecodeRequest([]byte) (Request, bool, error)
	EncodeResponse(Response) ([]byte, error)
	EncodeModels([]ModelInfo) ([]byte, error)
	NewStreamEncoder(StreamMeta) StreamEncoder
	EncodeError(error) (int, []byte)
}

type StreamEncoder interface {
	Encode(StreamEvent) ([][]byte, error)
}

type Registry struct {
	byName map[string]Protocol
}

func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Protocol)}
}

func (r *Registry) Register(p Protocol) {
	if p == nil {
		panic("protocols: Register nil Protocol")
	}
	name := p.Name()
	if name == "" {
		panic("protocols: Register Protocol with empty name")
	}
	r.byName[name] = p
}

func (r *Registry) Get(name string) (Protocol, bool) {
	p, ok := r.byName[name]
	return p, ok
}

func (r *Registry) MustGet(name string) Protocol {
	p, ok := r.Get(name)
	if !ok {
		panic(fmt.Sprintf("protocols: unknown protocol %q", name))
	}
	return p
}
