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
		Endpoints *[]struct {
			Name         string `json:"name"`
			ProviderName string `json:"provider_name"`
			Tag          string `json:"tag"`
		} `json:"endpoints"`
	} `json:"data"`
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
	if !validOpenRouterModelID(req.Model) {
		return false
	}
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
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._:-", char)) {
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
	parsed.Path = apiPath + "/models/" + modelID + "/endpoints"
	return parsed.String(), nil
}

func fetchOpenRouterEndpointRows(ctx context.Context, channel *model.Channel, key, baseURL, modelID string) ([]service.OpenRouterEndpoint, error) {
	requestURL, err := openRouterEndpointURL(baseURL, modelID)
	if err != nil {
		return nil, err
	}
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
	body, err := io.ReadAll(io.LimitReader(response.Body, openRouterEndpointResponseMaxBytes+1))
	if err != nil || len(body) > openRouterEndpointResponseMaxBytes {
		return nil, errors.New("invalid OpenRouter endpoint response")
	}
	var result openRouterEndpointAPIResponse
	if err := common.Unmarshal(body, &result); err != nil || result.Data == nil || result.Data.Endpoints == nil || len(*result.Data.Endpoints) > openRouterEndpointResponseMaxRows {
		return nil, errors.New("invalid OpenRouter endpoint response")
	}
	endpoints := make([]service.OpenRouterEndpoint, 0, len(*result.Data.Endpoints))
	for _, row := range *result.Data.Endpoints {
		if !validOpenRouterEndpointTag(row.Tag) || !validOpenRouterEndpointText(row.Name, 512) || !validOpenRouterEndpointText(row.ProviderName, 128) {
			return nil, errors.New("invalid OpenRouter endpoint response")
		}
		endpoints = append(endpoints, service.OpenRouterEndpoint{
			ID:           row.Tag,
			Name:         strings.TrimSpace(row.Name),
			ProviderName: strings.TrimSpace(row.ProviderName),
		})
	}
	return endpoints, nil
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
