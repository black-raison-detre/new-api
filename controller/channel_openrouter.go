package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

const (
	openRouterEndpointRequestMaxBytes  = 128 << 10
	openRouterEndpointResponseMaxBytes = 2 << 20
	openRouterEndpointResponseMaxRows  = 512
	openRouterModelResponseMaxBytes    = 16 << 20
	openRouterModelResponseMaxRows     = 8192
)

type openRouterEndpointDiscoveryRequest struct {
	ChannelID      *int    `json:"channel_id"`
	Type           *int    `json:"type"`
	Key            *string `json:"key"`
	BaseURL        *string `json:"base_url"`
	Proxy          *string `json:"proxy"`
	HeaderOverride *string `json:"header_override"`
	Model          string  `json:"model"`
}

type openRouterEndpointAPIResponse struct {
	Data *struct {
		Endpoints *[]common.RawMessage `json:"endpoints"`
	} `json:"data"`
}

type openRouterCatalogDiscoveryRequest struct {
	openRouterEndpointDiscoveryRequest
	Scope string `json:"scope"`
}

// FetchOpenRouterCatalog previews discovery without changing channel models or
// local prices. Account scope applies upstream preferences, not local access.
func FetchOpenRouterCatalog(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, openRouterEndpointRequestMaxBytes)
	var req openRouterCatalogDiscoveryRequest
	if err := c.ShouldBindJSON(&req); err != nil || !validOpenRouterDiscoveryConnection(req.openRouterEndpointDiscoveryRequest) {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request"))
		return
	}
	if req.Scope == "" {
		req.Scope = "public"
	}
	if (req.Scope != "public" && req.Scope != "account") || (req.Type != nil && *req.Type != constant.ChannelTypeOpenRouter) {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request"))
		return
	}
	channel, err := buildOpenRouterEndpointPreviewChannel(req.openRouterEndpointDiscoveryRequest)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errOpenRouterChannelNotFound) {
			status = http.StatusNotFound
		}
		common.ApiErrorStatus(c, status, common.NewMessage("Invalid request"))
		return
	}
	key := ""
	if req.Scope == "account" || req.Key != nil || req.ChannelID != nil {
		key, err = openRouterEndpointAPIKey(channel, req.Key)
		if err != nil || (req.Scope == "account" && key == "") {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request"))
			return
		}
	}
	baseURL := strings.TrimSpace(channel.GetBaseURL())
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(constant.ChannelTypeOpenRouter)
	}
	if _, err := openRouterDiscoveryURL(baseURL, "/models"); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request"))
		return
	}
	headerOverride := ""
	if channel.HeaderOverride != nil {
		headerOverride = *channel.HeaderOverride
	}
	catalog, err := service.GetOpenRouterModelCatalog(c.Request.Context(), service.OpenRouterCatalogIdentity{
		BaseURL: baseURL, Key: key, Proxy: strings.TrimSpace(channel.GetSetting().Proxy), HeaderOverride: headerOverride,
	}, req.Scope, func(ctx context.Context) ([]service.OpenRouterCatalogModel, error) {
		return fetchOpenRouterModelRows(ctx, channel, key, baseURL, req.Scope)
	})
	if err != nil {
		common.ApiErrorStatus(c, http.StatusBadGateway, common.NewMessage("Failed to fetch models"))
		return
	}
	common.ApiSuccess(c, catalog)
}

// ListOpenRouterModelMetadata returns only exact catalog matches for models the
// caller can already list locally. The public catalog is never an authorization
// source, and this route never accepts upstream URLs or credentials.
func ListOpenRouterModelMetadata(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	names, _, err := accessibleModelNames(c)
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Failed to fetch models"))
		return
	}
	catalog := service.OpenRouterModelCatalog{Source: "openrouter", Scope: "public", Models: []service.OpenRouterCatalogModel{}}
	if len(names) == 0 {
		common.ApiSuccess(c, catalog)
		return
	}
	baseURL := constant.GetChannelBaseURL(constant.ChannelTypeOpenRouter)
	catalog, err = service.GetOpenRouterModelCatalog(c.Request.Context(), service.OpenRouterCatalogIdentity{BaseURL: baseURL}, "public",
		func(ctx context.Context) ([]service.OpenRouterCatalogModel, error) {
			return fetchOpenRouterModelRows(ctx, &model.Channel{Type: constant.ChannelTypeOpenRouter}, "", baseURL, "public")
		})
	if err != nil {
		common.ApiErrorStatus(c, http.StatusBadGateway, common.NewMessage("Failed to fetch models"))
		return
	}
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	filtered := make([]service.OpenRouterCatalogModel, 0)
	for _, row := range catalog.Models {
		if allowed[row.ID] {
			filtered = append(filtered, row)
		}
	}
	catalog.Models = filtered
	common.ApiSuccess(c, catalog)
}

func FetchOpenRouterEndpoints(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, openRouterEndpointRequestMaxBytes)
	var req openRouterEndpointDiscoveryRequest
	if err := c.ShouldBindJSON(&req); err != nil || !validOpenRouterEndpointRequest(req) {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid OpenRouter endpoint request"))
		return
	}
	if req.Type != nil && *req.Type != constant.ChannelTypeOpenRouter {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid OpenRouter endpoint request"))
		return
	}

	channel, err := buildOpenRouterEndpointPreviewChannel(req)
	if err != nil {
		status := http.StatusBadRequest
		message := "Invalid OpenRouter endpoint request"
		if errors.Is(err, errOpenRouterChannelNotFound) {
			status = http.StatusNotFound
			message = "Channel not found"
		} else if errors.Is(err, errOpenRouterChannelTypeMismatch) {
			message = "Channel type does not match OpenRouter endpoint discovery"
		}
		common.ApiErrorStatus(c, status, common.NewMessage(message))
		return
	}

	key, err := openRouterEndpointAPIKey(channel, req.Key)
	if err != nil || key == "" {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid OpenRouter endpoint request"))
		return
	}
	baseURL := strings.TrimSpace(channel.GetBaseURL())
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(constant.ChannelTypeOpenRouter)
	}
	proxy := strings.TrimSpace(channel.GetSetting().Proxy)
	headerOverride := ""
	if channel.HeaderOverride != nil {
		headerOverride = *channel.HeaderOverride
	}

	catalog, err := service.GetOpenRouterEndpointCatalog(
		c.Request.Context(),
		service.OpenRouterEndpointCatalogIdentity{
			BaseURL:        baseURL,
			Key:            key,
			Proxy:          proxy,
			HeaderOverride: headerOverride,
		},
		req.Model,
		func(ctx context.Context) ([]service.OpenRouterEndpoint, error) {
			return fetchOpenRouterEndpointRows(ctx, channel, key, baseURL, req.Model)
		},
	)
	if err != nil {
		common.ApiErrorStatus(c, http.StatusBadGateway, common.NewMessage("Failed to fetch OpenRouter endpoints"))
		return
	}
	common.ApiSuccess(c, catalog)
}

var (
	errOpenRouterChannelNotFound     = errors.New("OpenRouter channel not found")
	errOpenRouterChannelTypeMismatch = errors.New("channel type does not match OpenRouter")
)

func buildOpenRouterEndpointPreviewChannel(req openRouterEndpointDiscoveryRequest) (*model.Channel, error) {
	var channel *model.Channel
	if req.ChannelID != nil {
		if *req.ChannelID <= 0 {
			return nil, errOpenRouterChannelNotFound
		}
		var err error
		channel, err = model.GetChannelById(*req.ChannelID, true)
		if err != nil || channel == nil {
			return nil, errOpenRouterChannelNotFound
		}
		if channel.Type != constant.ChannelTypeOpenRouter {
			return nil, errOpenRouterChannelTypeMismatch
		}
	} else {
		channel = &model.Channel{Type: constant.ChannelTypeOpenRouter}
	}

	preview := *channel
	if req.Key != nil {
		key := strings.TrimSpace(*req.Key)
		key, _, _ = strings.Cut(key, "\n")
		preview.Key = key
		preview.ChannelInfo = model.ChannelInfo{}
	}
	if req.BaseURL != nil {
		baseURL := strings.TrimSpace(*req.BaseURL)
		preview.BaseURL = &baseURL
	}
	if req.Proxy != nil {
		settings := preview.GetSetting()
		settings.Proxy = strings.TrimSpace(*req.Proxy)
		preview.SetSetting(settings)
	}
	if req.HeaderOverride != nil {
		raw := strings.TrimSpace(*req.HeaderOverride)
		if raw != "" {
			var headers map[string]any
			if err := common.UnmarshalJsonStr(raw, &headers); err != nil || headers == nil {
				return nil, errors.New("invalid OpenRouter channel header override")
			}
		}
		preview.HeaderOverride = &raw
	}
	return &preview, nil
}

func openRouterEndpointAPIKey(channel *model.Channel, requestKey *string) (string, error) {
	if requestKey != nil {
		key := strings.TrimSpace(*requestKey)
		key, _, _ = strings.Cut(key, "\n")
		return key, nil
	}
	key, _, apiErr := channel.GetNextEnabledKey()
	if apiErr != nil {
		return "", apiErr
	}
	key = strings.TrimSpace(key)
	key, _, _ = strings.Cut(key, "\n")
	return key, nil
}

func validOpenRouterEndpointRequest(req openRouterEndpointDiscoveryRequest) bool {
	return validOpenRouterModelID(req.Model) && validOpenRouterDiscoveryConnection(req)
}

func validOpenRouterDiscoveryConnection(req openRouterEndpointDiscoveryRequest) bool {
	return (req.Key == nil || len(*req.Key) <= 4096) &&
		(req.BaseURL == nil || len(*req.BaseURL) <= 2048) &&
		(req.Proxy == nil || len(*req.Proxy) <= 2048) &&
		(req.HeaderOverride == nil || len(*req.HeaderOverride) <= 64<<10)
}

func validOpenRouterModelID(modelID string) bool {
	if modelID == "" || len(modelID) > 512 || modelID != strings.TrimSpace(modelID) {
		return false
	}
	author, slug, found := strings.Cut(modelID, "/")
	if !found || author == "" || slug == "" || strings.Contains(slug, "/") || author == "." || author == ".." || slug == "." || slug == ".." {
		return false
	}
	for _, part := range []string{author, slug} {
		for _, char := range part {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._:~-", char)) {
				return false
			}
		}
	}
	return true
}

func openRouterEndpointURL(baseURL, modelID string) (string, error) {
	if !validOpenRouterModelID(modelID) {
		return "", errors.New("invalid OpenRouter model ID")
	}
	return openRouterDiscoveryURL(baseURL, "/models/"+modelID+"/endpoints")
}

func openRouterDiscoveryURL(baseURL, apiSuffix string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawPath != "" || len(parsed.Path) > 1024 {
		return "", errors.New("invalid OpenRouter base URL")
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("invalid OpenRouter base URL")
		}
	}
	apiPath := strings.TrimRight(parsed.Path, "/")
	if apiPath != "/v1" && !strings.HasSuffix(apiPath, "/v1") {
		apiPath += "/v1"
	}
	parsed.Path = apiPath + apiSuffix
	return parsed.String(), nil
}

func fetchOpenRouterEndpointRows(ctx context.Context, channel *model.Channel, key, baseURL, modelID string) ([]service.OpenRouterEndpoint, error) {
	requestURL, err := openRouterEndpointURL(baseURL, modelID)
	if err != nil {
		return nil, err
	}
	body, err := fetchOpenRouterDiscoveryJSON(ctx, channel, key, requestURL, openRouterEndpointResponseMaxBytes)
	if err != nil {
		return nil, err
	}
	var result openRouterEndpointAPIResponse
	if err := common.Unmarshal(body, &result); err != nil || result.Data == nil || result.Data.Endpoints == nil || len(*result.Data.Endpoints) > openRouterEndpointResponseMaxRows {
		return nil, errors.New("invalid OpenRouter endpoint response")
	}
	endpoints := make([]service.OpenRouterEndpoint, 0, len(*result.Data.Endpoints))
	for _, raw := range *result.Data.Endpoints {
		var row struct {
			Name         string `json:"name"`
			ProviderName string `json:"provider_name"`
			Tag          string `json:"tag"`
		}
		if err := common.Unmarshal(raw, &row); err != nil || !validOpenRouterEndpointTag(row.Tag) || !validOpenRouterEndpointText(row.Name, 512) || !validOpenRouterEndpointText(row.ProviderName, 128) {
			return nil, errors.New("invalid OpenRouter endpoint response")
		}
		endpoints = append(endpoints, service.OpenRouterEndpoint{
			ID:           row.Tag,
			Name:         strings.TrimSpace(row.Name),
			ProviderName: strings.TrimSpace(row.ProviderName),
			Metadata:     raw,
		})
	}
	return endpoints, nil
}

func fetchOpenRouterModelRows(ctx context.Context, channel *model.Channel, key, baseURL, scope string) ([]service.OpenRouterCatalogModel, error) {
	suffix := "/models"
	if scope == "account" {
		suffix += "/user"
	} else if scope != "public" {
		return nil, errors.New("invalid OpenRouter catalog scope")
	}
	requestURL, err := openRouterDiscoveryURL(baseURL, suffix)
	if err != nil {
		return nil, err
	}
	// Both upstream lists default to text output. The rich catalog must also
	// retain image, audio, embedding, and other model modalities.
	requestURL += "?output_modalities=all"
	body, err := fetchOpenRouterDiscoveryJSON(ctx, channel, key, requestURL, openRouterModelResponseMaxBytes)
	if err != nil {
		return nil, err
	}
	var result struct {
		Data  *[]common.RawMessage `json:"data"`
		Links struct {
			Next *string `json:"next"`
		} `json:"links"`
		TotalCount *int `json:"total_count"`
	}
	if err := common.Unmarshal(body, &result); err != nil || result.Data == nil || len(*result.Data) > openRouterModelResponseMaxRows {
		return nil, errors.New("invalid OpenRouter model catalog")
	}
	// Request the complete list without pagination. Never follow upstream links
	// with channel credentials or publish a partial list as the full catalog.
	if (result.Links.Next != nil && *result.Links.Next != "") || (result.TotalCount != nil && *result.TotalCount != len(*result.Data)) {
		return nil, errors.New("incomplete OpenRouter model catalog")
	}
	models := make([]service.OpenRouterCatalogModel, 0, len(*result.Data))
	seen := make(map[string]bool, len(*result.Data))
	for _, raw := range *result.Data {
		var row struct {
			ID string `json:"id"`
		}
		if err := common.Unmarshal(raw, &row); err != nil || !validOpenRouterModelID(row.ID) || seen[row.ID] {
			return nil, errors.New("invalid OpenRouter model catalog")
		}
		seen[row.ID] = true
		models = append(models, service.OpenRouterCatalogModel{ID: row.ID, Metadata: raw})
	}
	return models, nil
}

func fetchOpenRouterDiscoveryJSON(ctx context.Context, channel *model.Channel, key, requestURL string, maxBytes int64) ([]byte, error) {
	headers, err := buildFetchModelsHeaders(channel, key)
	if err != nil {
		return nil, errors.New("invalid OpenRouter channel headers")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, errors.New("invalid OpenRouter endpoint request")
	}
	request.Header = headers.Clone()
	request.Header.Set("Accept", "application/json")
	if key == "" && strings.TrimSpace(request.Header.Get("Authorization")) == "Bearer" {
		request.Header.Del("Authorization")
	}
	request.Host = headers.Get("Host")

	settings := channel.GetSetting()
	client, err := service.GetHttpClientWithProxySettings(settings.Proxy, settings)
	if err != nil {
		return nil, errors.New("invalid OpenRouter channel proxy")
	}
	// Keep the configured channel transport and proxy while bounding this
	// administrative lookup and preventing credentials from following redirects.
	endpointClient := *client
	endpointClient.Timeout = 10 * time.Second
	endpointClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := endpointClient.Do(request)
	if err != nil {
		return nil, errors.New("OpenRouter endpoint request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("OpenRouter endpoint request failed")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil || int64(len(body)) > maxBytes {
		return nil, errors.New("invalid OpenRouter endpoint response")
	}
	return body, nil
}

func validOpenRouterEndpointTag(tag string) bool {
	if tag == "" || len(tag) > 256 || tag != strings.TrimSpace(tag) {
		return false
	}
	segmentCount := 0
	for segment := range strings.SplitSeq(tag, "/") {
		segmentCount++
		if segmentCount > 4 || len(segment) == 0 || len(segment) > 128 || segment == "." || segment == ".." {
			return false
		}
		for index, char := range segment {
			alphanumeric := (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9')
			if index == 0 {
				if !alphanumeric {
					return false
				}
				continue
			}
			if !alphanumeric && !strings.ContainsRune("._-", char) {
				return false
			}
		}
	}
	return true
}

func validOpenRouterEndpointText(value string, maxLength int) bool {
	if strings.TrimSpace(value) == "" || len(value) > maxLength {
		return false
	}
	return !strings.ContainsFunc(value, unicode.IsControl)
}
