package cpa

import (
	"encoding/json"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/pluginapi"
)

type rpcAuthModelRequest struct {
	pluginapi.AuthModelRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func (p *Plugin) handleModelStatic(raw []byte) (any, error) {
	var req pluginapi.StaticModelRequest
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &req)
	}
	p.setHostExcludedModels(req.Host.ExcludedModels)
	models := p.collectCatalogModels()
	return pluginapi.ModelResponse{
		Provider: ProviderID,
		Models:   models,
	}, nil
}

func (p *Plugin) handleModelForAuth(raw []byte) (any, error) {
	var req rpcAuthModelRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	p.setHostExcludedModels(req.Host.ExcludedModels)
	if !isAIStudioProvider(req.AuthProvider) {
		var payload AuthFilePayload
		if len(req.StorageJSON) > 0 {
			_ = json.Unmarshal(req.StorageJSON, &payload)
		}
		if !isAIStudioProvider(payload.Type) && !isAIStudioProvider(payload.Provider) {
			return pluginapi.ModelResponse{}, nil
		}
	}

	var payload AuthFilePayload
	if len(req.StorageJSON) > 0 {
		_ = json.Unmarshal(req.StorageJSON, &payload)
	}
	accID := extractAccountIDFromAuth(req.StorageJSON, req.Metadata, req.Attributes)
	p.setAccountExcludedModels(accID, extractExcludedModelsFromReq(req.StorageJSON, req.Metadata, req.Attributes))

	var authUpdate pluginapi.AuthData
	if state, ok := extractStorageStateFromPayload(req.StorageJSON, payload); ok {
		if rt, err := p.Runtime(); err == nil {
			if acc, err := p.upsertAccountInRuntime(p.ctx, rt, accID, payload, state, false); err == nil && acc.ID != "" {
				accID = strings.ToLower(strings.TrimSpace(acc.ID))
				p.setAccountExcludedModels(accID, extractExcludedModelsFromReq(req.StorageJSON, req.Metadata, req.Attributes))
				curTier, _ := req.Metadata["tier"].(string)
				curState, _ := req.Metadata["state"].(string)
				if strings.TrimSpace(curTier) != string(acc.BenefitTier) || strings.TrimSpace(curState) != string(acc.State) {
					if updated, err := p.buildAuthDataForAccountWithExisting(rt, acc, req.StorageJSON, req.Metadata, req.Attributes); err == nil {
						if req.AuthID != "" {
							updated.ID = req.AuthID
						}
						authUpdate = updated
					}
				}
			}
		}
	}

	models := p.collectCatalogModelsForAccount(accID)
	return pluginapi.ModelResponse{
		Provider:   ProviderID,
		Models:     models,
		AuthUpdate: authUpdate,
	}, nil
}

func (p *Plugin) handleModelRoute(raw []byte) (any, error) {
	var req rpcModelRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	model := strings.TrimSpace(req.RequestedModel)
	if model == "" || p.serviceStopped.Load() {
		return pluginapi.ModelRouteResponse{Handled: false}, nil
	}

	prefix := strings.TrimSpace(p.Config().ModelPrefix)
	if prefix == "" {
		prefix = "aistudio/"
	}

	lowerModel := strings.ToLower(model)
	for _, pfx := range []string{strings.ToLower(prefix), "aistudio/", "aistudio2api/"} {
		if pfx != "" && strings.HasPrefix(lowerModel, pfx) {
			clean := strings.TrimSpace(model[len(pfx):])
			if clean == "" {
				continue
			}
			return pluginapi.ModelRouteResponse{
				Handled:     true,
				TargetKind:  pluginapi.ModelRouteTargetSelf,
				Target:      PluginID,
				TargetModel: clean,
				Reason:      "routed prefixed AI Studio model to aistudio2api",
			}, nil
		}
	}

	if mapped := p.resolveConfiguredModelMapping(model); mapped != "" && !strings.EqualFold(mapped, model) {
		if !p.isModelExcluded(mapped) && !p.isModelExcluded(model) {
			return pluginapi.ModelRouteResponse{
				Handled:     true,
				TargetKind:  pluginapi.ModelRouteTargetSelf,
				Target:      PluginID,
				TargetModel: mapped,
				Reason:      "routed mapped AI Studio model alias to aistudio2api",
			}, nil
		}
	}

	// When CLIProxyAPI's GlobalModelRegistry has already resolved candidate providers
	// for an unprefixed model (via ModelsForAuth after applying excluded_models and
	// oauth.model-alias), let the host route natively across available providers
	// without hijacking other providers or bypassing credential exclusions.
	if len(req.AvailableProviders) > 0 {
		return pluginapi.ModelRouteResponse{Handled: false}, nil
	}

	cfg := p.Config()
	routeUnprefixed := boolOrDefault(cfg.RouteUnprefixedModels, true)
	if routeUnprefixed && p.isKnownModel(model) && !p.isModelExcluded(model) {
		return pluginapi.ModelRouteResponse{
			Handled:     true,
			TargetKind:  pluginapi.ModelRouteTargetSelf,
			Target:      PluginID,
			TargetModel: model,
			Reason:      "routed AI Studio model to aistudio2api",
		}, nil
	}

	return pluginapi.ModelRouteResponse{Handled: false}, nil
}

func (p *Plugin) hasReadyAccount() bool {
	if p.serviceStopped.Load() {
		return false
	}
	rt, err := p.Runtime()
	if err != nil {
		return false
	}
	_, eligible := rt.Pool().EnabledAccounts()
	return eligible > 0
}

func (p *Plugin) isKnownModel(modelID string) bool {
	clean := StripModelPrefix(modelID, p.Config().ModelPrefix)
	clean = strings.TrimPrefix(clean, "models/")
	if clean == "" {
		return false
	}
	p.mu.RLock()
	mappingStr := p.appCfg.ModelMapping
	rt := p.runtime
	p.mu.RUnlock()
	if mappingStr != "" {
		if mappings, err := config.ParseModelMapping(mappingStr); err == nil {
			for _, item := range mappings {
				if strings.EqualFold(item.Alias, clean) && item.Model != "" {
					clean = strings.TrimPrefix(item.Model, "models/")
					break
				}
			}
		}
	}
	clean = strings.TrimSuffix(clean, "-search")
	if base, _, err := aistudio.ApplyModelSuffix(clean, aistudio.GenerationConfig{}); err == nil && base != "" {
		clean = base
	}
	clean = strings.TrimSuffix(clean, "-search")
	clean = strings.TrimSuffix(clean, "-build")
	clean = strings.TrimSuffix(clean, "-search")
	if base, _, err := aistudio.ApplyModelSuffix(clean, aistudio.GenerationConfig{}); err == nil && base != "" {
		clean = base
	}
	if rt != nil {
		if canon := rt.Pool().CanonicalModelID(clean); canon != "" {
			clean = strings.TrimPrefix(canon, "models/")
		}
	}
	for _, m := range p.collectRawCatalogModels("") {
		id := strings.TrimPrefix(strings.TrimSpace(m.ID), "models/")
		if strings.EqualFold(id, clean) || strings.EqualFold(id, strings.TrimSuffix(clean, "-search")) {
			return true
		}
	}
	return false
}

func StripModelPrefix(model string, configuredPrefix string) string {
	trimmed := strings.TrimSpace(model)
	lower := strings.ToLower(trimmed)
	for _, pfx := range []string{strings.ToLower(strings.TrimSpace(configuredPrefix)), "aistudio/", "aistudio2api/"} {
		if pfx != "" && strings.HasPrefix(lower, pfx) {
			return strings.TrimSpace(trimmed[len(pfx):])
		}
	}
	return trimmed
}

func (p *Plugin) collectCatalogModels() []pluginapi.ModelInfo {
	return p.collectCatalogModelsForAccount("")
}

func (p *Plugin) collectCatalogModelsForAccount(accountID string) []pluginapi.ModelInfo {
	rawModels := p.collectRawCatalogModels(accountID)
	searchAliases := boolOrDefault(p.Config().SearchModelAliases, true)

	out := make([]pluginapi.ModelInfo, 0, len(rawModels)*2)
	seen := make(map[string]struct{}, len(rawModels)*2)
	addModel := func(info pluginapi.ModelInfo) {
		key := strings.ToLower(strings.TrimSpace(info.ID))
		if key == "" {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		out = append(out, info)
	}

	for _, m := range rawModels {
		info := convertAIStudioModel(m)
		if info.ID == "" {
			continue
		}
		// Register canonical unprefixed model ID (e.g., "gemini-3.8-flash", "gemini-3.8-flash-build")
		addModel(info)

		// Register -search alias for models supporting Google Search grounding
		if searchAliases && m.Capabilities["search"] && !strings.HasSuffix(info.ID, "-search") {
			searchInfo := info
			searchInfo.ID = info.ID + "-search"
			searchInfo.DisplayName = info.DisplayName + " (Search)"
			addModel(searchInfo)
		}
	}

	p.mu.RLock()
	mappingStr := p.appCfg.ModelMapping
	p.mu.RUnlock()
	if mappingStr != "" {
		if mappings, err := config.ParseModelMapping(mappingStr); err == nil {
			byID := make(map[string]pluginapi.ModelInfo, len(out))
			for _, item := range out {
				byID[strings.ToLower(item.ID)] = item
			}
			for _, m := range mappings {
				alias := strings.TrimSpace(m.Alias)
				target := strings.TrimPrefix(strings.TrimSpace(m.Model), "models/")
				if alias == "" || target == "" {
					continue
				}
				if base, ok := byID[strings.ToLower(target)]; ok {
					aliasInfo := base
					aliasInfo.ID = alias
					aliasInfo.Name = "models/" + alias
					aliasInfo.DisplayName = alias + " -> " + base.DisplayName
					addModel(aliasInfo)
				} else {
					addModel(convertAIStudioModel(aistudio.Model{
						ID:   "models/" + alias,
						Name: alias,
					}))
				}
			}
		}
	}
	return out
}

func (p *Plugin) collectRawCatalogModels(accountID string) []aistudio.Model {
	rt, err := p.Runtime()
	if err == nil {
		if accountID != "" {
			if acc, ok := rt.Pool().AccountByID(accountID); ok && acc != nil && len(acc.Models) > 0 {
				models := acc.Models
				if rt.Pool().BuildEnabled() {
					if cached := rt.CachedModels(); len(cached) > 0 {
						models = mergeRawModels(models, cached)
					}
				}
				return models
			}
		}
		if models, err := rt.Admin.CatalogModels(p.ctx); err == nil && len(models) > 0 {
			return models
		}
		if cached := rt.CachedModels(); len(cached) > 0 {
			return cached
		}
	}
	return defaultFallbackModels()
}

func convertAIStudioModel(m aistudio.Model) pluginapi.ModelInfo {
	id := strings.TrimPrefix(strings.TrimSpace(m.ID), "models/")
	if id == "" {
		return pluginapi.ModelInfo{}
	}
	displayName := resolveAIStudioDisplayName(m, id)
	inLimit := m.InputTokenLimit
	if inLimit <= 0 {
		inLimit = 1048576
	}
	outLimit := m.OutputTokenLimit
	if outLimit <= 0 {
		outLimit = 65536
	}

	methods := append([]string(nil), m.Methods...)
	if len(methods) == 0 {
		methods = []string{"generateContent", "streamGenerateContent", "countTokens"}
	}

	var thinking *pluginapi.ThinkingSupport
	if m.ThinkingBudget != nil || len(m.ThinkingLevels) > 0 {
		thinking = &pluginapi.ThinkingSupport{
			DynamicAllowed: true,
			Levels:         append([]string(nil), m.ThinkingLevels...),
		}
		if m.ThinkingBudget != nil {
			if m.ThinkingBudget.Min != nil {
				thinking.Min = int(*m.ThinkingBudget.Min)
			}
			if m.ThinkingBudget.Max != nil {
				thinking.Max = int(*m.ThinkingBudget.Max)
			} else {
				thinking.Max = 32768
			}
			if m.ThinkingBudget.CanDisable != nil {
				thinking.ZeroAllowed = *m.ThinkingBudget.CanDisable
			}
		}
	}

	inputModalities := []string{"text", "image", "audio", "video", "pdf"}
	outputModalities := []string{"text"}
	if m.Capabilities["image_generation"] || strings.Contains(id, "image") || strings.Contains(id, "imagen") {
		outputModalities = append(outputModalities, "image")
	}
	if m.Capabilities["tts"] || strings.Contains(id, "tts") {
		outputModalities = []string{"audio"}
	}
	if m.Capabilities["video_generation"] || strings.Contains(id, "veo") {
		outputModalities = []string{"video"}
	}

	return pluginapi.ModelInfo{
		ID:                         id,
		Object:                     "model",
		OwnedBy:                    ProviderID,
		Type:                       ProviderID,
		DisplayName:                displayName,
		Name:                       "models/" + id,
		Version:                    m.Version,
		Description:                cleanAIStudioDescription(id, m.Description),
		InputTokenLimit:            inLimit,
		OutputTokenLimit:           outLimit,
		ContextLength:              inLimit,
		MaxCompletionTokens:        outLimit,
		SupportedGenerationMethods: methods,
		SupportedInputModalities:   inputModalities,
		SupportedOutputModalities:  outputModalities,
		Thinking:                   thinking,
		UserDefined:                true,
	}
}

func defaultFallbackModels() []aistudio.Model {
	canDisableTrue := true
	canDisableFalse := false
	minBudget := int64(128)
	maxBudgetPro := int64(32768)
	maxBudgetFlash := int64(24576)

	return []aistudio.Model{
		{
			ID:               "models/gemini-2.5-pro",
			Name:             "Gemini 2.5 Pro",
			Description:      "Google AI Studio Gemini 2.5 Pro with thinking & multimodal reasoning",
			Methods:          []string{"generateContent", "streamGenerateContent", "countTokens"},
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			ThinkingLevels:   []string{"low", "medium", "high"},
			ThinkingBudget:   &aistudio.ThinkingBudget{Min: &minBudget, Max: &maxBudgetPro, CanDisable: &canDisableFalse},
			Capabilities:     map[string]bool{"thinking": true, "search": true, "code_execution": true, "url_context": true, "function_calling": true, "structured_outputs": true},
		},
		{
			ID:               "models/gemini-4-argon",
			Name:             "Gemini 4 Argon",
			Description:      "Google AI Studio Gemini 4 Argon next-generation model",
			Methods:          []string{"generateContent", "streamGenerateContent", "countTokens"},
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			ThinkingLevels:   []string{"low", "medium", "high"},
			ThinkingBudget:   &aistudio.ThinkingBudget{Min: &minBudget, Max: &maxBudgetPro, CanDisable: &canDisableFalse},
			Capabilities:     map[string]bool{"thinking": true, "search": true, "code_execution": true, "url_context": true, "function_calling": true, "structured_outputs": true},
		},
		{
			ID:               "models/gemini-3.8-flash",
			Name:             "Gemini 3.8 Flash",
			Description:      "Google AI Studio Gemini 3.8 Flash hybrid reasoning model",
			Methods:          []string{"generateContent", "streamGenerateContent", "countTokens"},
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			ThinkingLevels:   []string{"minimal", "low", "medium", "high"},
			ThinkingBudget:   &aistudio.ThinkingBudget{Min: &minBudget, Max: &maxBudgetFlash, CanDisable: &canDisableTrue},
			Capabilities:     map[string]bool{"thinking": true, "search": true, "code_execution": true, "url_context": true, "function_calling": true, "structured_outputs": true},
		},
		{
			ID:               "models/gemini-3.6-flash",
			Name:             "Gemini 3.6 Flash",
			Description:      "Google AI Studio Gemini 3.6 Flash efficient model",
			Methods:          []string{"generateContent", "streamGenerateContent", "countTokens"},
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			ThinkingLevels:   []string{"minimal", "low", "medium", "high"},
			ThinkingBudget:   &aistudio.ThinkingBudget{Min: &minBudget, Max: &maxBudgetFlash, CanDisable: &canDisableTrue},
			Capabilities:     map[string]bool{"thinking": true, "search": true, "code_execution": true, "url_context": true, "function_calling": true, "structured_outputs": true},
		},
		{
			ID:               "models/gemini-3.1-pro-preview",
			Name:             "Gemini 3.1 Pro Preview",
			Description:      "Google AI Studio Gemini 3.1 Pro Preview with thinking & multimodal reasoning",
			Methods:          []string{"generateContent", "streamGenerateContent", "countTokens"},
			InputTokenLimit:  1048576,
			OutputTokenLimit: 65536,
			ThinkingLevels:   []string{"low", "medium", "high"},
			ThinkingBudget:   &aistudio.ThinkingBudget{Min: &minBudget, Max: &maxBudgetPro, CanDisable: &canDisableFalse},
			Capabilities:     map[string]bool{"thinking": true, "search": true, "code_execution": true, "url_context": true, "function_calling": true, "structured_outputs": true},
		},
		{
			ID:               "models/gemini-2.5-flash-image",
			Name:             "Gemini 2.5 Flash Image",
			Description:      "Google AI Studio native multimodal image generation & editing",
			Methods:          []string{"generateContent", "streamGenerateContent", "countTokens"},
			InputTokenLimit:  32768,
			OutputTokenLimit: 8192,
			Capabilities:     map[string]bool{"image_generation": true},
		},
		{
			ID:               "models/gemini-3.8-flash-tts",
			Name:             "Gemini 3.8 Flash TTS",
			Description:      "Google AI Studio Gemini 3.8 Flash Text-to-Speech",
			Methods:          []string{"generateContent"},
			InputTokenLimit:  8192,
			OutputTokenLimit: 16384,
			Capabilities:     map[string]bool{"tts": true},
		},
		{
			ID:               "models/gemini-embedding-001",
			Name:             "Gemini Embedding 001",
			Description:      "Google AI Studio text embedding model",
			Methods:          []string{"embedContent", "batchEmbedContents"},
			InputTokenLimit:  8192,
			OutputTokenLimit: 3072,
		},
	}
}

func (p *Plugin) resolveConfiguredModelMapping(modelID string) string {
	clean := strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	if clean == "" {
		return ""
	}
	p.mu.RLock()
	mappingStr := p.appCfg.ModelMapping
	p.mu.RUnlock()
	if mappingStr == "" {
		return ""
	}
	mappings, err := config.ParseModelMapping(mappingStr)
	if err != nil {
		return ""
	}
	for _, item := range mappings {
		if strings.EqualFold(strings.TrimSpace(item.Alias), clean) && strings.TrimSpace(item.Model) != "" {
			return strings.TrimPrefix(strings.TrimSpace(item.Model), "models/")
		}
	}
	return ""
}

func (p *Plugin) setAccountExcludedModels(accountID string, patterns []string) {
	accountID = strings.ToLower(strings.TrimSpace(accountID))
	if accountID == "" {
		return
	}
	cleaned := make([]string, 0, len(patterns))
	for _, pat := range patterns {
		if s := strings.ToLower(strings.TrimSpace(pat)); s != "" {
			cleaned = append(cleaned, s)
		}
	}
	p.excludedMu.Lock()
	if len(cleaned) == 0 {
		delete(p.accountExcluded, accountID)
	} else {
		p.accountExcluded[accountID] = cleaned
	}
	p.excludedMu.Unlock()
}

func extractExcludedModelsFromReq(storageJSON []byte, metadata map[string]any, attributes map[string]string) []string {
	if list := extractExcludedModelsList(metadata, attributes); len(list) > 0 {
		return list
	}
	if len(storageJSON) > 0 {
		var doc map[string]any
		if json.Unmarshal(storageJSON, &doc) == nil {
			return extractExcludedModelsList(doc, nil)
		}
	}
	return nil
}

func extractExcludedModelsList(metadata map[string]any, attributes map[string]string) []string {
	for _, key := range []string{"excluded_models", "excluded-models"} {
		if metadata != nil {
			if raw, ok := metadata[key]; ok && raw != nil {
				switch v := raw.(type) {
				case string:
					parts := strings.Split(v, ",")
					out := make([]string, 0, len(parts))
					for _, part := range parts {
						if s := strings.TrimSpace(part); s != "" {
							out = append(out, s)
						}
					}
					if len(out) > 0 {
						return out
					}
				case []string:
					out := make([]string, 0, len(v))
					for _, item := range v {
						if s := strings.TrimSpace(item); s != "" {
							out = append(out, s)
						}
					}
					if len(out) > 0 {
						return out
					}
				case []any:
					out := make([]string, 0, len(v))
					for _, item := range v {
						if str, ok := item.(string); ok && strings.TrimSpace(str) != "" {
							out = append(out, strings.TrimSpace(str))
						}
					}
					if len(out) > 0 {
						return out
					}
				}
			}
		}
		if attributes != nil {
			if raw := strings.TrimSpace(attributes[key]); raw != "" {
				parts := strings.Split(raw, ",")
				out := make([]string, 0, len(parts))
				for _, part := range parts {
					if s := strings.TrimSpace(part); s != "" {
						out = append(out, s)
					}
				}
				if len(out) > 0 {
					return out
				}
			}
		}
	}
	return nil
}

func (p *Plugin) setHostExcludedModels(hostExcluded map[string][]string) {
	if len(hostExcluded) == 0 {
		return
	}
	cloned := make(map[string][]string, len(hostExcluded))
	for k, v := range hostExcluded {
		cloned[strings.ToLower(strings.TrimSpace(k))] = append([]string(nil), v...)
	}
	p.excludedMu.Lock()
	p.hostExcluded = cloned
	p.excludedMu.Unlock()
}

func (p *Plugin) isModelExcluded(modelID string) bool {
	clean := strings.ToLower(strings.TrimPrefix(StripModelPrefix(modelID, p.Config().ModelPrefix), "models/"))
	if clean == "" {
		return false
	}
	var globalRules []string
	p.excludedMu.RLock()
	for _, key := range []string{ProviderID, "aistudio", "google-aistudio"} {
		if rules, ok := p.hostExcluded[key]; ok && len(rules) > 0 {
			globalRules = append(globalRules, rules...)
		}
	}
	var allAccountRules [][]string
	for _, rules := range p.accountExcluded {
		if len(rules) > 0 {
			allAccountRules = append(allAccountRules, append([]string(nil), rules...))
		}
	}
	p.excludedMu.RUnlock()
	rt, err := p.Runtime()
	if err == nil && rt != nil {
		accounts, aErr := rt.Admin.Accounts(p.ctx)
		if aErr == nil && len(accounts) > 0 {
			enabledCount := 0
			excludedCount := 0
			p.excludedMu.RLock()
			for _, acc := range accounts {
				if !acc.Enabled {
					continue
				}
				enabledCount++
				accID := strings.ToLower(strings.TrimSpace(acc.ID))
				rules := p.accountExcluded[accID]
				if len(rules) == 0 {
					rules = globalRules
				}
				if matchesAnyModelPattern(clean, rules) {
					excludedCount++
				}
			}
			p.excludedMu.RUnlock()
			if enabledCount > 0 {
				return excludedCount == enabledCount
			}
		}
	}
	if len(allAccountRules) > 0 {
		excludedCount := 0
		for _, rules := range allAccountRules {
			if matchesAnyModelPattern(clean, rules) {
				excludedCount++
			}
		}
		return excludedCount == len(allAccountRules)
	}
	return matchesAnyModelPattern(clean, globalRules)
}

func matchesAnyModelPattern(modelID string, patterns []string) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if modelID == "" || len(patterns) == 0 {
		return false
	}
	for _, rawPat := range patterns {
		pat := strings.ToLower(strings.TrimSpace(rawPat))
		if pat == "" {
			continue
		}
		if matchModelWildcard(pat, modelID) {
			return true
		}
	}
	return false
}

func matchModelWildcard(pattern, value string) bool {
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	if prefix := parts[0]; prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = value[len(prefix):]
	}
	if suffix := parts[len(parts)-1]; suffix != "" {
		if !strings.HasSuffix(value, suffix) {
			return false
		}
		value = value[:len(value)-len(suffix)]
	}
	for i := 1; i < len(parts)-1; i++ {
		seg := parts[i]
		if seg == "" {
			continue
		}
		idx := strings.Index(value, seg)
		if idx < 0 {
			return false
		}
		value = value[idx+len(seg):]
	}
	return true
}

func resolveAIStudioDisplayName(m aistudio.Model, id string) string {
	if m.Details != nil {
		alias := strings.TrimSpace(m.Details.Alias)
		if alias != "" && !strings.HasPrefix(strings.ToLower(alias), "points to ") {
			return alias
		}
	}
	return cleanAIStudioDisplayName(id, m.Name)
}

func cleanAIStudioDisplayName(id, rawName string) string {
	cleanID := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(id), "models/"))
	switch cleanID {
	case "gemini-3.1-flash-image":
		return "Gemini 3.1 Flash Image"
	case "gemini-3-pro-image":
		return "Gemini 3 Pro Image"
	case "gemini-3.1-flash-lite-image":
		return "Gemini 3.1 Flash Lite Image"
	case "gemini-nano-banana-2.1":
		return "Gemini Nano Banana 2.1"
	case "gemini-2.5-flash-image":
		return "Gemini 2.5 Flash Image"
	case "gemini-2.5-flash-image-preview":
		return "Gemini 2.5 Flash Image Preview"
	default:
		name := strings.TrimSpace(rawName)
		if name != "" {
			return name
		}
		return id
	}
}

func cleanAIStudioDescription(id, rawDesc string) string {
	cleanID := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(id), "models/"))
	switch cleanID {
	case "gemini-embedding-001":
		return "Google 文本向量嵌入端点，供 RAG 知识库与向量检索计算文本特征使用"
	case "gemini-3.1-flash-image":
		return "Google AI Studio 新一代主力多模态生图引擎 (原内部代号 Nano Banana 2)"
	case "gemini-3-pro-image":
		return "Google AI Studio 旗舰级高质量多模态生图引擎 (原内部代号 Nano Banana Pro)"
	case "gemini-3.1-flash-lite-image":
		return "Google AI Studio 极速轻量多模态生图引擎 (原内部代号 Nano Banana 2 Lite)"
	case "gemini-nano-banana-2.1":
		return "Google AI Studio 多轮角色与风格一致性多模态生图引擎 (原内部代号 Nano Banana 2.1)"
	case "gemini-2.5-flash-image":
		return "Google AI Studio 第一代基线多模态生图引擎 (原内部代号 Nano Banana)"
	case "gemini-2.5-flash-image-preview":
		return "Google AI Studio 早期预览版多模态生图引擎"
	default:
		desc := strings.TrimSpace(rawDesc)
		if desc != "" {
			return desc
		}
		return "Google AI Studio model " + cleanID
	}
}

func mergeRawModels(primary, secondary []aistudio.Model) []aistudio.Model {
	seen := make(map[string]struct{}, len(primary)+len(secondary))
	out := make([]aistudio.Model, 0, len(primary)+len(secondary))
	for _, m := range primary {
		id := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(m.ID), "models/"))
		if id == "" {
			continue
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			out = append(out, m)
		}
	}
	for _, m := range secondary {
		id := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(m.ID), "models/"))
		if id == "" {
			continue
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			out = append(out, m)
		}
	}
	return out
}
