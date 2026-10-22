package openrouter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	rootcommon "github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
)

type routedOpenRouterBody struct {
	body      []byte
	sessionID string
}

type originalOpenRouterFields struct {
	Provider    json.RawMessage `json:"provider"`
	Model       json.RawMessage `json:"model"`
	Models      json.RawMessage `json:"models"`
	Fallbacks   json.RawMessage `json:"fallbacks"`
	Preset      json.RawMessage `json:"preset"`
	Route       json.RawMessage `json:"route"`
	Router      json.RawMessage `json:"router"`
	ModelRouter json.RawMessage `json:"model_router"`
}

// ValidateRequestModelRouting rejects opaque model selection before any quota
// reservation. Provider failover within one model remains supported; cross-model
// routing needs authorization and pricing for every possible model first.
func ValidateRequestModelRouting(c *gin.Context, info *relaycommon.RelayInfo) error {
	if info == nil || info.ChannelMeta == nil || info.ChannelType != constant.ChannelTypeOpenRouter ||
		info.ChannelOtherSettings.IsOpenRouterEnterprise() || !supportsOpenRouterTextJSONRequest(info) {
		return nil
	}
	body, err := readOriginalRequestBody(c)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	var fields originalOpenRouterFields
	if err := rootcommon.Unmarshal(body, &fields); err != nil {
		return errors.New("invalid original OpenRouter request body")
	}
	if err := validateOpenRouterModelRoutingFields(fields); err != nil {
		return err
	}
	return validateOpenRouterRoutedModel(fields.Model, false)
}

func (a *Adaptor) routeConfiguredRequest(c *gin.Context, info *relaycommon.RelayInfo, requestBody io.Reader) (routedOpenRouterBody, error) {
	policy, configured, err := info.ResolveOpenRouterModelRouting(c)
	if err != nil {
		return routedOpenRouterBody{}, err
	}
	if info == nil || info.ChannelType != constant.ChannelTypeOpenRouter || info.ChannelOtherSettings.IsOpenRouterEnterprise() {
		return routedOpenRouterBody{}, nil
	}
	info.OpenRouterSessionAffinityEnabled = false
	supportedJSONRequest := supportsOpenRouterTextJSONRequest(info)
	if !configured && !supportedJSONRequest {
		return routedOpenRouterBody{}, nil
	}
	if configured && !supportedJSONRequest {
		return routedOpenRouterBody{}, errors.New("OpenRouter model routing is unsupported for this request format")
	}
	if c == nil || c.Request == nil {
		if !configured {
			return routedOpenRouterBody{}, nil
		}
		return routedOpenRouterBody{}, errors.New("OpenRouter model routing requires a JSON HTTP request")
	}
	if requestBody == nil {
		if !configured {
			return routedOpenRouterBody{}, nil
		}
		return routedOpenRouterBody{}, errors.New("OpenRouter model routing requires a JSON HTTP request")
	}

	originalBody, err := readOriginalRequestBody(c)
	if err != nil {
		return routedOpenRouterBody{}, err
	}
	var original originalOpenRouterFields
	if len(originalBody) > 0 {
		if err := rootcommon.Unmarshal(originalBody, &original); err != nil {
			return routedOpenRouterBody{}, errors.New("invalid original OpenRouter request body")
		}
		if err := validateOpenRouterModelRoutingFields(original); err != nil {
			return routedOpenRouterBody{}, err
		}
		if err := validateOpenRouterRoutedModel(original.Model, configured); err != nil {
			return routedOpenRouterBody{}, err
		}
	}

	finalBody, err := io.ReadAll(requestBody)
	if err != nil {
		return routedOpenRouterBody{}, errors.New("failed to read OpenRouter request body")
	}
	if len(finalBody) == 0 && !configured {
		return routedOpenRouterBody{}, nil
	}
	fields := make(map[string]json.RawMessage)
	if len(finalBody) > 0 {
		if err := rootcommon.Unmarshal(finalBody, &fields); err != nil || fields == nil {
			return routedOpenRouterBody{}, errors.New("OpenRouter model routing requires a JSON object body")
		}
	}
	if err := validateOpenRouterModelRoutingFields(originalOpenRouterFieldsFromMap(fields)); err != nil {
		return routedOpenRouterBody{}, err
	}
	if err := validateOpenRouterRoutedModel(fields["model"], configured); err != nil {
		return routedOpenRouterBody{}, err
	}
	if configured {
		modelName := info.UpstreamModelName
		if modelName == "" {
			modelName = info.OriginModelName
		}
		modelName = removeOpenRouterRoutingSuffix(modelName)
		modelRaw, err := rootcommon.Marshal(modelName)
		if err != nil {
			return routedOpenRouterBody{}, errors.New("failed to encode OpenRouter model routing policy")
		}
		if err := validateOpenRouterRoutedModel(modelRaw, true); err != nil {
			return routedOpenRouterBody{}, err
		}
		switch policy.Mode {
		case dto.OpenRouterModelRoutingNitro:
			modelName += ":nitro"
		case dto.OpenRouterModelRoutingFloor:
			modelName += ":floor"
		case dto.OpenRouterModelRoutingStandard, dto.OpenRouterModelRoutingLocked:
		default:
			return routedOpenRouterBody{}, errors.New("invalid OpenRouter model routing policy")
		}
		if modelName == "" {
			return routedOpenRouterBody{}, errors.New("OpenRouter model routing has no mapped model")
		}
		modelRaw, err = rootcommon.Marshal(modelName)
		if err != nil {
			return routedOpenRouterBody{}, errors.New("failed to encode OpenRouter model routing policy")
		}
		fields["model"] = modelRaw
	}

	providerRaw, err := mergeOpenRouterProvider(original.Provider, fields["provider"], policy)
	if err != nil {
		return routedOpenRouterBody{}, err
	}
	if len(providerRaw) == 0 {
		delete(fields, "provider")
	} else {
		fields["provider"] = providerRaw
	}

	conversationKey, err := relaycommon.OpenRouterConversationKey(originalBody, c.Request.Header.Get("x-session-id"))
	if err != nil {
		return routedOpenRouterBody{}, err
	}
	var sessionID string
	if conversationKey != "" {
		accountScope := relaycommon.OpenRouterAccountScope(info.ChannelBaseUrl, info.ApiKey)
		policyScope := "native"
		if configured {
			policyScope = string(policy.Mode) + "\x00" + policy.Provider
		}
		sessionID = relaycommon.OpenRouterSessionID(
			info.UserId,
			info.OriginModelName,
			info.UsingGroup,
			policyScope,
			accountScope,
			conversationKey,
			info.ApiKey,
		)
	}
	if sessionID == "" && !configured && len(original.Provider) == 0 {
		return routedOpenRouterBody{body: finalBody}, nil
	}
	if sessionID != "" {
		sessionIDRaw, err := rootcommon.Marshal(sessionID)
		if err != nil {
			return routedOpenRouterBody{}, errors.New("failed to encode OpenRouter session key")
		}
		fields["session_id"] = sessionIDRaw
		info.OpenRouterSessionAffinityEnabled = true
	}
	encoded, err := rootcommon.Marshal(fields)
	if err != nil {
		return routedOpenRouterBody{}, errors.New("failed to encode OpenRouter routed request")
	}
	return routedOpenRouterBody{body: encoded, sessionID: sessionID}, nil
}

func validateOpenRouterRoutedModel(raw json.RawMessage, configured bool) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var modelName string
	if err := rootcommon.Unmarshal(raw, &modelName); err != nil {
		return errors.New("OpenRouter routing model must be a string")
	}
	normalized := strings.ToLower(modelName)
	if strings.HasPrefix(normalized, "openrouter/") || strings.Contains(normalized, "@preset/") {
		return errors.New("OpenRouter model routing cannot target a remote router or preset model")
	}
	for _, variant := range strings.Split(normalized, ":")[1:] {
		if configured && variant == "exacto" {
			return errors.New("OpenRouter exacto routing is incompatible with the configured model policy")
		}
	}
	return nil
}

func supportsOpenRouterTextJSONRequest(info *relaycommon.RelayInfo) bool {
	if info == nil {
		return false
	}
	switch info.RelayFormat {
	case types.RelayFormatOpenAI:
		return info.RelayMode == relayconstant.RelayModeChatCompletions || info.RelayMode == relayconstant.RelayModeCompletions || info.RelayMode == relayconstant.RelayModeResponses
	case types.RelayFormatOpenAIResponses:
		return info.RelayMode == relayconstant.RelayModeResponses
	case types.RelayFormatClaude:
		return info.RelayMode == relayconstant.RelayModeUnknown ||
			info.RelayMode == relayconstant.RelayModeChatCompletions ||
			info.RelayMode == relayconstant.RelayModeResponses
	case types.RelayFormatGemini:
		return info.RelayMode == relayconstant.RelayModeGemini
	default:
		return false
	}
}

func readOriginalRequestBody(c *gin.Context) ([]byte, error) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return nil, nil
	}
	storage, err := rootcommon.GetBodyStorage(c)
	if err != nil {
		return nil, errors.New("failed to read original OpenRouter request body")
	}
	reader, err := storage.NewReader()
	if err != nil {
		return nil, errors.New("failed to read original OpenRouter request body")
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, errors.New("failed to read original OpenRouter request body")
	}
	return body, nil
}

func originalOpenRouterFieldsFromMap(fields map[string]json.RawMessage) originalOpenRouterFields {
	return originalOpenRouterFields{
		Provider:    fields["provider"],
		Model:       fields["model"],
		Models:      fields["models"],
		Fallbacks:   fields["fallbacks"],
		Preset:      fields["preset"],
		Route:       fields["route"],
		Router:      fields["router"],
		ModelRouter: fields["model_router"],
	}
}

func validateOpenRouterModelRoutingFields(fields originalOpenRouterFields) error {
	for _, value := range []json.RawMessage{fields.Models, fields.Fallbacks, fields.Preset, fields.Route, fields.Router, fields.ModelRouter} {
		if len(value) > 0 && string(value) != "null" {
			return errors.New("OpenRouter model routing does not allow request model fallbacks or remote routers")
		}
	}
	return nil
}

func removeOpenRouterRoutingSuffix(modelName string) string {
	parts := strings.Split(modelName, ":")
	filtered := parts[:1]
	for _, variant := range parts[1:] {
		if strings.EqualFold(variant, "nitro") || strings.EqualFold(variant, "floor") {
			continue
		}
		filtered = append(filtered, variant)
	}
	return strings.Join(filtered, ":")
}

func mergeOpenRouterProvider(originalRaw, finalRaw json.RawMessage, policy dto.OpenRouterModelRoutingPolicy) (json.RawMessage, error) {
	provider := make(map[string]json.RawMessage)
	if len(originalRaw) > 0 && string(originalRaw) != "null" {
		if err := rootcommon.Unmarshal(originalRaw, &provider); err != nil || provider == nil {
			return nil, errors.New("invalid OpenRouter provider constraints")
		}
	}
	var finalProvider map[string]json.RawMessage
	if len(finalRaw) > 0 && string(finalRaw) != "null" {
		if err := rootcommon.Unmarshal(finalRaw, &finalProvider); err != nil || finalProvider == nil {
			return nil, errors.New("invalid OpenRouter provider constraints")
		}
	}
	for key, value := range finalProvider {
		provider[key] = value
	}
	if err := retainOpenRouterProviderRestrictions(provider, originalRaw, finalRaw); err != nil {
		return nil, err
	}
	if onlyRaw, constrained := provider["only"]; constrained && len(onlyRaw) > 0 && string(onlyRaw) != "null" {
		only, err := openRouterProviderList(onlyRaw)
		if err != nil {
			return nil, err
		}
		if len(only) == 0 {
			return nil, errors.New("OpenRouter provider constraints allow no endpoints")
		}
	}

	if policy.Mode != "" {
		delete(provider, "order")
		delete(provider, "sort")
	}
	if policy.Mode == dto.OpenRouterModelRoutingLocked {
		only, err := openRouterProviderList(provider["only"])
		if err != nil {
			return nil, err
		}
		onlyRaw, constrained := provider["only"]
		if constrained && len(onlyRaw) > 0 && string(onlyRaw) != "null" && !openRouterEndpointAllowed(policy.Provider, only) {
			return nil, errors.New("locked OpenRouter provider conflicts with client only constraint")
		}
		ignored, err := openRouterProviderList(provider["ignore"])
		if err != nil {
			return nil, err
		}
		if openRouterEndpointAllowed(policy.Provider, ignored) {
			return nil, errors.New("locked OpenRouter provider conflicts with client ignore constraint")
		}
		orderRaw, err := rootcommon.Marshal([]string{policy.Provider})
		if err != nil {
			return nil, errors.New("failed to encode locked OpenRouter provider")
		}
		provider["order"] = orderRaw
		provider["only"] = orderRaw
		fallbackRaw, err := rootcommon.Marshal(false)
		if err != nil {
			return nil, errors.New("failed to encode locked OpenRouter provider")
		}
		provider["allow_fallbacks"] = fallbackRaw
	} else if fallbackRaw := provider["allow_fallbacks"]; policy.Mode != "" && len(fallbackRaw) > 0 {
		var allowFallbacks bool
		if err := rootcommon.Unmarshal(fallbackRaw, &allowFallbacks); err != nil {
			return nil, errors.New("invalid OpenRouter allow_fallbacks constraint")
		}
		if allowFallbacks {
			delete(provider, "allow_fallbacks")
		}
	}
	if len(provider) == 0 {
		return nil, nil
	}
	encoded, err := rootcommon.Marshal(provider)
	if err != nil {
		return nil, errors.New("failed to encode OpenRouter provider constraints")
	}
	return encoded, nil
}

func retainOpenRouterProviderRestrictions(provider map[string]json.RawMessage, originalRaw, finalRaw json.RawMessage) error {
	var original map[string]json.RawMessage
	var final map[string]json.RawMessage
	if len(originalRaw) > 0 && string(originalRaw) != "null" {
		if err := rootcommon.Unmarshal(originalRaw, &original); err != nil {
			return errors.New("invalid OpenRouter provider constraints")
		}
	}
	if len(finalRaw) > 0 && string(finalRaw) != "null" {
		if err := rootcommon.Unmarshal(finalRaw, &final); err != nil {
			return errors.New("invalid OpenRouter provider constraints")
		}
	}
	for key, originalValue := range original {
		finalValue, hasFinal := final[key]
		if key == "order" || key == "sort" || !hasFinal {
			continue
		}
		if len(originalValue) == 0 || string(originalValue) == "null" {
			continue
		}
		if len(finalValue) == 0 || string(finalValue) == "null" {
			provider[key] = originalValue
			continue
		}
		switch key {
		case "data_collection":
			var originalSetting, finalSetting string
			if rootcommon.Unmarshal(originalValue, &originalSetting) != nil || rootcommon.Unmarshal(finalValue, &finalSetting) != nil {
				return errors.New("invalid OpenRouter data_collection constraint")
			}
			if originalSetting == "deny" || finalSetting == "deny" {
				provider[key], _ = rootcommon.Marshal("deny")
			} else if originalSetting != "allow" || finalSetting != "allow" {
				return errors.New("conflicting OpenRouter data_collection constraints")
			}
		case "zdr", "require_parameters":
			var originalSetting, finalSetting bool
			if rootcommon.Unmarshal(originalValue, &originalSetting) != nil || rootcommon.Unmarshal(finalValue, &finalSetting) != nil {
				return fmt.Errorf("invalid OpenRouter %s constraint", key)
			}
			if originalSetting {
				provider[key], _ = rootcommon.Marshal(true)
			}
		case "allow_fallbacks":
			var originalSetting, finalSetting bool
			if rootcommon.Unmarshal(originalValue, &originalSetting) != nil || rootcommon.Unmarshal(finalValue, &finalSetting) != nil {
				return errors.New("invalid OpenRouter allow_fallbacks constraint")
			}
			if !originalSetting || !finalSetting {
				provider[key], _ = rootcommon.Marshal(false)
			}
		case "only":
			originalList, err := openRouterProviderList(originalValue)
			if err != nil {
				return err
			}
			finalList, err := openRouterProviderList(finalValue)
			if err != nil {
				return err
			}
			intersection := make([]string, 0, min(len(originalList), len(finalList)))
			for _, originalID := range originalList {
				for _, finalID := range finalList {
					if endpoint, intersects := intersectOpenRouterEndpointRules(originalID, finalID); intersects && !slices.Contains(intersection, endpoint) {
						intersection = append(intersection, endpoint)
					}
				}
			}
			provider[key], _ = rootcommon.Marshal(intersection)
		case "ignore":
			originalList, err := openRouterProviderList(originalValue)
			if err != nil {
				return err
			}
			finalList, err := openRouterProviderList(finalValue)
			if err != nil {
				return err
			}
			combined := slices.Clone(originalList)
			for _, providerID := range finalList {
				if !slices.Contains(combined, providerID) {
					combined = append(combined, providerID)
				}
			}
			provider[key], _ = rootcommon.Marshal(combined)
		default:
			if !openRouterJSONEqual(originalValue, finalValue) {
				return errors.New("conflicting OpenRouter provider constraints")
			}
		}
	}
	return nil
}

func openRouterJSONEqual(left, right json.RawMessage) bool {
	var leftValue, rightValue any
	if rootcommon.Unmarshal(left, &leftValue) != nil || rootcommon.Unmarshal(right, &rightValue) != nil {
		return false
	}
	leftJSON, leftErr := rootcommon.Marshal(leftValue)
	rightJSON, rightErr := rootcommon.Marshal(rightValue)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func openRouterProviderList(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var values []string
	if err := rootcommon.Unmarshal(raw, &values); err != nil {
		return nil, errors.New("invalid OpenRouter provider list constraint")
	}
	return values, nil
}

func intersectOpenRouterEndpointRules(left, right string) (string, bool) {
	if openRouterEndpointRuleMatches(left, right) {
		return left, true
	}
	if openRouterEndpointRuleMatches(right, left) {
		return right, true
	}
	return "", false
}

func openRouterEndpointAllowed(endpoint string, rules []string) bool {
	for _, rule := range rules {
		if openRouterEndpointRuleMatches(endpoint, rule) {
			return true
		}
	}
	return false
}

func openRouterEndpointRuleMatches(endpoint, rule string) bool {
	return endpoint == rule || strings.HasPrefix(endpoint, rule+"/")
}

func applyOpenRouterSessionHeader(info *relaycommon.RelayInfo, sessionID string) func() {
	if info == nil || sessionID == "" {
		return func() {}
	}
	if info.UseRuntimeHeadersOverride {
		previous := info.RuntimeHeadersOverride
		info.RuntimeHeadersOverride = withOpenRouterSessionHeader(previous, sessionID)
		return func() { info.RuntimeHeadersOverride = previous }
	}
	previous := info.HeadersOverride
	info.HeadersOverride = withOpenRouterSessionHeader(previous, sessionID)
	return func() { info.HeadersOverride = previous }
}

func withOpenRouterSessionHeader(previous map[string]any, sessionID string) map[string]any {
	updated := make(map[string]any, len(previous)+1)
	for key, value := range previous {
		if !strings.EqualFold(key, "x-session-id") {
			updated[key] = value
		}
	}
	updated["x-session-id"] = sessionID
	return updated
}

func routedBodyReader(body routedOpenRouterBody, original io.Reader) io.Reader {
	if body.body == nil {
		return original
	}
	return bytes.NewReader(body.body)
}
