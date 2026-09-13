package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mktcz/uniproxy/internal/protocols"
)

const (
	zenModelsURL   = "https://opencode.ai/zen/v1/models"
	goModelsURL    = "https://opencode.ai/zen/go/v1/models"
	catalogURL     = "https://models.opencode.ai/api.json"
	catalogTimeout = 15 * time.Second
	UserAgent      = "opencode/1.18.30"
)

type modelMeta struct {
	ID             string
	Plan           Plan
	Protocol       Protocol
	NPM            string
	AllowAnonymous bool
}

var fallbackModels = []protocols.ModelInfo{
	{ID: "opencode/nemotron-3-ultra-free", OwnedBy: "opencode"},
	{ID: "opencode/mimo-v2.5-free", OwnedBy: "opencode"},
	{ID: "opencode/big-pickle", OwnedBy: "opencode"},
	{ID: "opencode/ling-3.0-flash-fin-free", OwnedBy: "opencode"},
}

func (a *Adapter) Models() []protocols.ModelInfo {
	a.ensureCatalog(context.Background())
	a.catalogMu.RLock()
	defer a.catalogMu.RUnlock()
	if len(a.modelList) == 0 {
		out := make([]protocols.ModelInfo, len(fallbackModels))
		copy(out, fallbackModels)
		return out
	}
	out := make([]protocols.ModelInfo, len(a.modelList))
	copy(out, a.modelList)
	return out
}

func (a *Adapter) ensureCatalog(ctx context.Context) {
	a.catalogMu.RLock()
	ready := a.catalogReady
	a.catalogMu.RUnlock()
	if ready {
		return
	}
	a.catalogOnce.Do(func() {
		_ = a.refreshCatalog(ctx)
	})
}

func (a *Adapter) refreshCatalog(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()

	zen, errZen := a.fetchModelsList(ctx, a.zenModelsURL)
	goList, errGo := a.fetchModelsList(ctx, a.goModelsURL)
	npmByID := a.fetchNPMMap(ctx)

	zenMeta := map[string]modelMeta{}
	goMeta := map[string]modelMeta{}
	list := make([]protocols.ModelInfo, 0, len(zen)+len(goList))

	for _, id := range zen {
		meta := modelMeta{
			ID:             id,
			Plan:           PlanZen,
			AllowAnonymous: isAnonymousModel(id),
			NPM:            npmByID["opencode/"+id],
		}
		if meta.NPM == "" {
			meta.NPM = npmByID[id]
		}
		meta.Protocol = protocolFromNPM(meta.NPM, id)
		if meta.Protocol == "" {
			meta.Protocol = protocolHeuristic(id)
		}
		zenMeta[id] = meta
		list = append(list, protocols.ModelInfo{ID: prefixZen + id, OwnedBy: "opencode"})
	}
	for _, id := range goList {
		meta := modelMeta{
			ID:             id,
			Plan:           PlanGo,
			AllowAnonymous: false,
			NPM:            npmByID["opencode-go/"+id],
		}
		if meta.NPM == "" {
			meta.NPM = npmByID[id]
		}
		meta.Protocol = protocolFromNPM(meta.NPM, id)
		if meta.Protocol == "" {
			meta.Protocol = protocolHeuristic(id)
		}
		goMeta[id] = meta
		list = append(list, protocols.ModelInfo{ID: prefixGo + id, OwnedBy: "opencode-go"})
	}

	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	if len(list) > 0 {
		a.zenModels = zenMeta
		a.goModels = goMeta
		a.modelList = list
		a.catalogReady = true
		return nil
	}
	if errZen != nil {
		return errZen
	}
	return errGo
}

func (a *Adapter) fetchModelsList(ctx context.Context, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("list models %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	var envelope struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode models %s: %w", url, err)
	}
	ids := make([]string, 0, len(envelope.Data))
	for _, item := range envelope.Data {
		id := strings.TrimSpace(item.ID)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (a *Adapter) fetchNPMMap(ctx context.Context) map[string]string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.catalogURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := a.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	var root map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&root); err != nil {
		return nil
	}
	out := map[string]string{}
	for key, raw := range root {
		var entry struct {
			ID       string `json:"id"`
			Provider struct {
				NPM string `json:"npm"`
			} `json:"provider"`
			NPM string `json:"npm"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		npm := strings.TrimSpace(entry.Provider.NPM)
		if npm == "" {
			npm = strings.TrimSpace(entry.NPM)
		}
		if npm == "" {
			continue
		}
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			id = key
		}
		out[id] = npm
		if strings.Contains(id, "/") {
			parts := strings.SplitN(id, "/", 2)
			if len(parts) == 2 {
				out[parts[1]] = npm
			}
		}
	}
	return out
}

func (a *Adapter) seedCatalog(zen, goModels map[string]modelMeta, list []protocols.ModelInfo) {
	a.catalogMu.Lock()
	defer a.catalogMu.Unlock()
	a.zenModels = zen
	a.goModels = goModels
	a.modelList = list
	a.catalogReady = true
}
