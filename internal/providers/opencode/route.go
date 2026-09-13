package opencode

import (
	"strings"
)

type Plan string

const (
	PlanZen Plan = "zen"
	PlanGo  Plan = "go"
)

type Protocol string

const (
	ProtocolChat      Protocol = "chat"
	ProtocolResponses Protocol = "responses"
	ProtocolMessages  Protocol = "messages"
	ProtocolGemini    Protocol = "gemini"
)

type Route struct {
	Plan           Plan
	Protocol       Protocol
	WireModel      string
	ClientModel    string
	AllowAnonymous bool
	NPM            string
}

const (
	prefixZen = "opencode/"
	prefixGo  = "opencode-go/"
)

func parseModelID(model string) (planHint Plan, wire string) {
	model = strings.TrimSpace(model)
	switch {
	case strings.HasPrefix(model, prefixGo):
		return PlanGo, strings.TrimPrefix(model, prefixGo)
	case strings.HasPrefix(model, prefixZen):
		return PlanZen, strings.TrimPrefix(model, prefixZen)
	default:
		return "", model
	}
}

func isAnonymousModel(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "big-pickle" {
		return true
	}
	return strings.HasSuffix(id, "-free")
}

func protocolFromNPM(npm, wireModel string) Protocol {
	switch strings.TrimSpace(npm) {
	case "@ai-sdk/anthropic":
		return ProtocolMessages
	case "@ai-sdk/google":
		return ProtocolGemini
	case "@ai-sdk/openai-compatible":
		return ProtocolChat
	case "@ai-sdk/openai":
		if responsesFamily(wireModel) {
			return ProtocolResponses
		}
		return ProtocolChat
	default:
		return ""
	}
}

func protocolHeuristic(wireModel string) Protocol {
	lower := strings.ToLower(wireModel)
	switch {
	case strings.HasPrefix(lower, "claude"), strings.HasPrefix(lower, "qwen"):
		return ProtocolMessages
	case strings.HasPrefix(lower, "gemini"):
		return ProtocolGemini
	case responsesFamily(lower):
		return ProtocolResponses
	default:
		return ProtocolChat
	}
}

func responsesFamily(wireModel string) bool {
	lower := strings.ToLower(wireModel)
	return strings.HasPrefix(lower, "gpt-5") ||
		strings.HasPrefix(lower, "gpt-4.1") ||
		strings.HasPrefix(lower, "grok") ||
		strings.HasPrefix(lower, "muse")
}

func (a *Adapter) resolveRoute(model string) Route {
	planHint, wire := parseModelID(model)
	meta := a.lookupModel(planHint, wire)

	plan := planHint
	if plan == "" {
		if meta != nil {
			plan = meta.Plan
		} else if a.goOnlyModel(wire) {
			plan = PlanGo
		} else {
			plan = PlanZen
		}
	}

	npm := ""
	allowAnonymous := isAnonymousModel(wire)
	if meta != nil {
		npm = meta.NPM
		if meta.AllowAnonymous {
			allowAnonymous = true
		}
		if meta.Protocol != "" {
			return Route{
				Plan:           plan,
				Protocol:       meta.Protocol,
				WireModel:      wire,
				ClientModel:    model,
				AllowAnonymous: allowAnonymous,
				NPM:            npm,
			}
		}
	}

	protocol := protocolFromNPM(npm, wire)
	if protocol == "" {
		protocol = protocolHeuristic(wire)
	}

	return Route{
		Plan:           plan,
		Protocol:       protocol,
		WireModel:      wire,
		ClientModel:    model,
		AllowAnonymous: allowAnonymous,
		NPM:            npm,
	}
}

func (a *Adapter) goOnlyModel(wire string) bool {
	a.catalogMu.RLock()
	defer a.catalogMu.RUnlock()
	_, inZen := a.zenModels[wire]
	_, inGo := a.goModels[wire]
	return inGo && !inZen
}

func (a *Adapter) lookupModel(planHint Plan, wire string) *modelMeta {
	a.catalogMu.RLock()
	defer a.catalogMu.RUnlock()
	switch planHint {
	case PlanGo:
		if m, ok := a.goModels[wire]; ok {
			return &m
		}
	case PlanZen:
		if m, ok := a.zenModels[wire]; ok {
			return &m
		}
	default:
		if m, ok := a.zenModels[wire]; ok {
			return &m
		}
		if m, ok := a.goModels[wire]; ok {
			return &m
		}
	}
	return nil
}
