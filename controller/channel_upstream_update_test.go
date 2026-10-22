package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newAdvancedCustomModelListChannel(baseURL string, key string, upstreamPath string, auth *dto.AdvancedCustomRouteAuth) *model.Channel {
	config := &dto.AdvancedCustomConfig{
		Routes: []dto.AdvancedCustomRoute{
			{
				IncomingPath: dto.AdvancedCustomModelListPath,
				UpstreamPath: upstreamPath,
				Converter:    "none",
				Auth:         auth,
			},
		},
	}
	channel := &model.Channel{
		Type:    constant.ChannelTypeAdvancedCustom,
		Key:     key,
		BaseURL: &baseURL,
	}
	channel.SetOtherSettings(dto.ChannelOtherSettings{AdvancedCustom: config})
	return channel
}

func TestParseOpenAIModelIDsStrictResponseContract(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		want      []string
		wantError string
	}{
		{name: "malformed JSON", body: `{"data":`, wantError: "invalid OpenAI Models response"},
		{name: "missing data", body: `{"object":"list"}`, wantError: "data is required"},
		{name: "null data", body: `{"data":null}`, wantError: "data is required"},
		{name: "empty data", body: `{"data":[]}`, wantError: "no valid model IDs"},
		{name: "all IDs empty", body: `{"data":[{"id":""},{"id":"   "}]}`, wantError: "no valid model IDs"},
		{
			name: "filters empty IDs and normalizes valid IDs",
			body: `{"data":[{"id":" gpt-4.1 "},{"id":""},{"id":"gpt-4.1"},{"id":"o3"}]}`,
			want: []string{"gpt-4.1", "o3"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			models, err := parseOpenAIModelIDs([]byte(test.body))
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
				require.Nil(t, models)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, models)
		})
	}
}

func TestFetchAdvancedCustomModelsAppliesHeaderOverrideAfterRouteAuth(t *testing.T) {
	type receivedRequest struct {
		Headers http.Header
		Host    string
	}
	received := make(chan receivedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- receivedRequest{Headers: r.Header.Clone(), Host: r.Host}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4.1"}]}`))
	}))
	defer server.Close()

	channel := newAdvancedCustomModelListChannel(server.URL, "secret-key", "/provider/models", &dto.AdvancedCustomRouteAuth{
		Type:  dto.AdvancedCustomAuthTypeHeader,
		Name:  "X-Route-Key",
		Value: "route-{api_key}",
	})
	headerOverride := `{
		"X-Route-Key":"global-{api_key}",
		"X-Static":"static-value",
		"X-Client":"{client_header:X-Client}",
		"Host":"models.example.test",
		"*":""
	}`
	channel.HeaderOverride = &headerOverride

	models, err := fetchChannelUpstreamModelIDs(channel)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-4.1"}, models)

	request := <-received
	require.Equal(t, "global-secret-key", request.Headers.Get("X-Route-Key"))
	require.Equal(t, "static-value", request.Headers.Get("X-Static"))
	require.Empty(t, request.Headers.Get("X-Client"))
	require.Equal(t, "models.example.test", request.Host)
}

func TestFetchAdvancedCustomModelsUsesEnabledSavedMultiKey(t *testing.T) {
	authorization := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4.1-mini"}]}`))
	}))
	defer server.Close()

	channel := newAdvancedCustomModelListChannel(server.URL, "disabled-key\nenabled-key", "/v1/models", nil)
	channel.ChannelInfo = model.ChannelInfo{
		IsMultiKey: true,
		MultiKeyStatusList: map[int]int{
			0: common.ChannelStatusManuallyDisabled,
			1: common.ChannelStatusEnabled,
		},
	}

	models, err := fetchChannelUpstreamModelIDs(channel)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-4.1-mini"}, models)
	require.Equal(t, "Bearer enabled-key", <-authorization)
}

func TestFetchAdvancedCustomModelsRejectsNonOKResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"data":[{"id":"must-not-be-used"}]}`))
	}))
	defer server.Close()

	channel := newAdvancedCustomModelListChannel(server.URL, "secret-key", "/v1/models", nil)
	models, err := fetchChannelUpstreamModelIDs(channel)
	require.ErrorContains(t, err, "status code: 502")
	require.Nil(t, models)
}

func TestFetchAdvancedCustomModelsRedactsQueryKeyFromTransportErrors(t *testing.T) {
	const secret = "secret key/+"
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := server.URL
	server.Close()

	channel := newAdvancedCustomModelListChannel(baseURL, secret, "/v1/models", &dto.AdvancedCustomRouteAuth{
		Type:  dto.AdvancedCustomAuthTypeQuery,
		Name:  "custom-token",
		Value: "prefix-{api_key}",
	})

	_, err := fetchChannelUpstreamModelIDs(channel)
	require.Error(t, err)
	require.NotContains(t, err.Error(), secret)
	require.NotContains(t, err.Error(), "custom-token")
	require.NotContains(t, err.Error(), "prefix-")

	direct := sanitizeFetchModelsError(&url.Error{
		Op:  http.MethodGet,
		URL: baseURL + "/v1/models?custom-token=prefix-" + url.QueryEscape(secret),
		Err: errors.New("connection refused"),
	}, secret)
	require.EqualError(t, direct, "connection refused")

	queryValue := "prefix-" + secret
	queryError := sanitizeAdvancedCustomRequestError(
		errors.New("dial "+queryValue+": connection refused"),
		queryValue,
		baseURL+"/v1/models?custom-token="+url.QueryEscape(queryValue),
	)
	require.NotContains(t, queryError.Error(), queryValue)
	require.EqualError(t, queryError, "dial [REDACTED]: connection refused")
}

func TestFetchOrdinaryOpenAIModelsKeepsExistingEmptyDataBehavior(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list"}`))
	}))
	defer server.Close()

	baseURL := server.URL
	channel := &model.Channel{
		Type:    constant.ChannelTypeOpenAI,
		Key:     "ordinary-key",
		BaseURL: &baseURL,
	}
	models, err := fetchChannelUpstreamModelIDs(channel)
	require.NoError(t, err)
	require.Empty(t, models)
}

func TestFetchModelsAdvancedCustomCreatePreview(t *testing.T) {
	receivedAuthorization := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthorization <- r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"preview-model"}]}`))
	}))
	defer server.Close()

	config := dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{
		IncomingPath: dto.AdvancedCustomModelListPath,
		UpstreamPath: "/preview/models",
		Converter:    "none",
	}}}
	configBytes, err := common.Marshal(config)
	require.NoError(t, err)
	rawConfig := string(configBytes)
	baseURL := server.URL
	emptyProxy := ""
	req := fetchModelsRequest{
		BaseURL:        &baseURL,
		Type:           constant.ChannelTypeAdvancedCustom,
		Key:            "create-preview-key",
		AdvancedCustom: &rawConfig,
		Proxy:          &emptyProxy,
	}
	body, err := common.Marshal(req)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/fetch_models", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	FetchModels(ctx)

	var response struct {
		Success bool     `json:"success"`
		Message string   `json:"message"`
		Data    []string `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success, response.Message)
	require.Equal(t, []string{"preview-model"}, response.Data)
	require.Equal(t, "Bearer create-preview-key", <-receivedAuthorization)
}

func TestFetchModelsAdvancedCustomEditPreviewUsesSavedKeyAndExplicitClears(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	receivedHeaders := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders <- r.Header.Clone()
		_, _ = w.Write([]byte(`{"data":[{"id":"edited-preview-model"}]}`))
	}))
	defer server.Close()

	savedChannel := newAdvancedCustomModelListChannel("http://127.0.0.1:1", "disabled-saved-key\nenabled-saved-key", "/saved/models", nil)
	savedChannel.Name = "saved advanced channel"
	savedChannel.Models = "old-model"
	savedChannel.ChannelInfo = model.ChannelInfo{
		IsMultiKey: true,
		MultiKeyStatusList: map[int]int{
			0: common.ChannelStatusManuallyDisabled,
			1: common.ChannelStatusEnabled,
		},
	}
	savedHeaderOverride := `{"X-Saved":"must-not-be-sent"}`
	savedChannel.HeaderOverride = &savedHeaderOverride
	savedChannel.SetSetting(dto.ChannelSettings{Proxy: "http://127.0.0.1:1"})
	require.NoError(t, db.Create(savedChannel).Error)

	preserved, err := buildAdvancedCustomModelPreviewChannel(fetchModelsRequest{ChannelID: savedChannel.Id})
	require.NoError(t, err)
	require.Equal(t, "http://127.0.0.1:1", preserved.GetBaseURL())
	require.Equal(t, savedHeaderOverride, *preserved.HeaderOverride)
	require.Equal(t, "http://127.0.0.1:1", preserved.GetSetting().Proxy)

	previewConfig := dto.AdvancedCustomConfig{Routes: []dto.AdvancedCustomRoute{{
		IncomingPath: dto.AdvancedCustomModelListPath,
		UpstreamPath: "/edited/models",
		Converter:    "none",
	}}}
	configBytes, err := common.Marshal(previewConfig)
	require.NoError(t, err)
	rawConfig := string(configBytes)
	baseURL := server.URL
	explicitEmpty := ""
	req := fetchModelsRequest{
		ChannelID:      savedChannel.Id,
		BaseURL:        &baseURL,
		Type:           constant.ChannelTypeAdvancedCustom,
		Key:            "request-key-must-be-ignored",
		AdvancedCustom: &rawConfig,
		HeaderOverride: &explicitEmpty,
		Proxy:          &explicitEmpty,
	}
	cleared, err := buildAdvancedCustomModelPreviewChannel(fetchModelsRequest{
		ChannelID:      savedChannel.Id,
		BaseURL:        &explicitEmpty,
		AdvancedCustom: &rawConfig,
		HeaderOverride: &explicitEmpty,
		Proxy:          &explicitEmpty,
	})
	require.NoError(t, err)
	require.NotNil(t, cleared.BaseURL)
	require.Empty(t, *cleared.BaseURL)
	require.NotNil(t, cleared.HeaderOverride)
	require.Empty(t, *cleared.HeaderOverride)
	require.Empty(t, cleared.GetSetting().Proxy)

	body, err := common.Marshal(req)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/fetch_models", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	FetchModels(ctx)

	var response struct {
		Success bool     `json:"success"`
		Message string   `json:"message"`
		Data    []string `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success, response.Message)
	require.Equal(t, []string{"edited-preview-model"}, response.Data)
	require.NotContains(t, recorder.Body.String(), "enabled-saved-key")
	require.NotContains(t, recorder.Body.String(), "request-key-must-be-ignored")

	headers := <-receivedHeaders
	require.Equal(t, "Bearer enabled-saved-key", headers.Get("Authorization"))
	require.Empty(t, headers.Get("X-Saved"))
}

func TestFailedAdvancedCustomDetectionDoesNotStageFullRemoval(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()

	channel := newAdvancedCustomModelListChannel(server.URL, "secret-key", "/v1/models", nil)
	channel.Name = "empty discovery response"
	channel.Models = "gpt-4.1,o3"
	settings := channel.GetOtherSettings()
	settings.UpstreamModelUpdateCheckEnabled = true
	settings.UpstreamModelUpdateAutoSyncEnabled = true
	channel.SetOtherSettings(settings)
	require.NoError(t, db.Create(channel).Error)

	modelsChanged, autoAdded, err := checkAndPersistChannelUpstreamModelUpdates(channel, &settings, true, true)
	require.ErrorContains(t, err, "no valid model IDs")
	require.False(t, modelsChanged)
	require.Zero(t, autoAdded)
	require.Empty(t, settings.UpstreamModelUpdateLastDetectedModels)
	require.Empty(t, settings.UpstreamModelUpdateLastRemovedModels)

	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	persistedSettings := reloaded.GetOtherSettings()
	require.Empty(t, persistedSettings.UpstreamModelUpdateLastDetectedModels)
	require.Empty(t, persistedSettings.UpstreamModelUpdateLastRemovedModels)
	require.Equal(t, "gpt-4.1,o3", reloaded.Models)
}

func TestFetchModelsUsesSharedChannelFetchBehavior(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "first-key" {
			t.Errorf("unexpected x-api-key header: %s", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected Authorization header: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":" claude-sonnet "},{"id":"claude-sonnet"}]}`))
	}))
	t.Cleanup(server.Close)

	body, err := common.Marshal(map[string]any{
		"base_url": server.URL,
		"type":     constant.ChannelTypeAnthropic,
		"key":      "first-key\nsecond-key",
	})
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/fetch_models", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	FetchModels(ctx)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"success":true,"message":"","data":["claude-sonnet"]}`, recorder.Body.String())
}

func TestFetchNewAPIModelsUsesOpenAIContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/models", r.URL.Path)
		assert.Equal(t, "Bearer new-api-key", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"data":[{"id":"gpt-5"},{"id":" gpt-5-mini "}]}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	baseURL := server.URL
	channel := &model.Channel{
		Type:    constant.ChannelTypeNewAPI,
		Key:     "new-api-key",
		BaseURL: &baseURL,
	}

	models, err := fetchChannelUpstreamModelIDs(channel)

	require.NoError(t, err)
	require.Equal(t, []string{"gpt-5", "gpt-5-mini"}, models)
}

func TestNormalizeModelNames(t *testing.T) {
	result := normalizeModelNames([]string{
		" gpt-4o ",
		"",
		"gpt-4o",
		"gpt-4.1",
		"   ",
	})

	require.Equal(t, []string{"gpt-4o", "gpt-4.1"}, result)
}

func TestMergeModelNames(t *testing.T) {
	result := mergeModelNames(
		[]string{"gpt-4o", "gpt-4.1"},
		[]string{"gpt-4.1", " gpt-4.1-mini ", "gpt-4o"},
	)

	require.Equal(t, []string{"gpt-4o", "gpt-4.1", "gpt-4.1-mini"}, result)
}

func TestSubtractModelNames(t *testing.T) {
	result := subtractModelNames(
		[]string{"gpt-4o", "gpt-4.1", "gpt-4.1-mini"},
		[]string{"gpt-4.1", "not-exists"},
	)

	require.Equal(t, []string{"gpt-4o", "gpt-4.1-mini"}, result)
}

func TestIntersectModelNames(t *testing.T) {
	result := intersectModelNames(
		[]string{"gpt-4o", "gpt-4.1", "gpt-4.1", "not-exists"},
		[]string{"gpt-4.1", "gpt-4o-mini", "gpt-4o"},
	)

	require.Equal(t, []string{"gpt-4o", "gpt-4.1"}, result)
}

func TestApplySelectedModelChanges(t *testing.T) {
	t.Run("add and remove together", func(t *testing.T) {
		result := applySelectedModelChanges(
			[]string{"gpt-4o", "gpt-4.1", "claude-3"},
			[]string{"gpt-4.1-mini"},
			[]string{"claude-3"},
		)

		require.Equal(t, []string{"gpt-4o", "gpt-4.1", "gpt-4.1-mini"}, result)
	})

	t.Run("add wins when conflict with remove", func(t *testing.T) {
		result := applySelectedModelChanges(
			[]string{"gpt-4o"},
			[]string{"gpt-4.1"},
			[]string{"gpt-4.1"},
		)

		require.Equal(t, []string{"gpt-4o", "gpt-4.1"}, result)
	})
}

func TestCollectPendingApplyUpstreamModelChanges(t *testing.T) {
	settings := dto.ChannelOtherSettings{
		UpstreamModelUpdateLastDetectedModels: []string{" gpt-4o ", "gpt-4o", "gpt-4.1"},
		UpstreamModelUpdateLastRemovedModels:  []string{" old-model ", "", "old-model"},
	}

	pendingAddModels, pendingRemoveModels := collectPendingApplyUpstreamModelChanges(settings)

	require.Equal(t, []string{"gpt-4o", "gpt-4.1"}, pendingAddModels)
	require.Equal(t, []string{"old-model"}, pendingRemoveModels)
}

func TestNormalizeChannelModelMapping(t *testing.T) {
	modelMapping := `{
		" alias-model ": " upstream-model ",
		"": "invalid",
		"invalid-target": ""
	}`
	channel := &model.Channel{
		ModelMapping: &modelMapping,
	}

	result := normalizeChannelModelMapping(channel)
	require.Equal(t, map[string]string{
		"alias-model": "upstream-model",
	}, result)
}

func TestCollectPendingUpstreamModelChangesFromModels_WithModelMapping(t *testing.T) {
	pendingAddModels, pendingRemoveModels := collectPendingUpstreamModelChangesFromModels(
		[]string{"alias-model", "gpt-4o", "stale-model"},
		[]string{"gpt-4o", "gpt-4.1", "mapped-target"},
		[]string{"gpt-4.1"},
		map[string]string{
			"alias-model": "mapped-target",
		},
	)

	require.Equal(t, []string{}, pendingAddModels)
	require.Equal(t, []string{"stale-model"}, pendingRemoveModels)
}

func TestCollectPendingUpstreamModelChangesFromModels_WithIgnoredRegexPatterns(t *testing.T) {
	pendingAddModels, pendingRemoveModels := collectPendingUpstreamModelChangesFromModels(
		[]string{"gpt-4o"},
		[]string{"gpt-4o", "claude-3-5-sonnet", "sora-video", "gpt-4.1"},
		[]string{"regex:^sora-.*$", "gpt-4.1"},
		nil,
	)

	require.Equal(t, []string{"claude-3-5-sonnet"}, pendingAddModels)
	require.Equal(t, []string{}, pendingRemoveModels)
}

func TestBuildUpstreamModelUpdateTaskNotificationContent_OmitOverflowDetails(t *testing.T) {
	channelSummaries := make([]upstreamModelUpdateChannelSummary, 0, 12)
	for i := range 12 {
		channelSummaries = append(channelSummaries, upstreamModelUpdateChannelSummary{
			ChannelName: "channel-" + string(rune('A'+i)),
			AddCount:    i + 1,
			RemoveCount: i,
		})
	}

	require.NoError(t, i18n.Init())
	content := buildUpstreamModelUpdateTaskNotificationContent(
		i18n.LangEn,
		24,
		12,
		56,
		21,
		9,
		[]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
		channelSummaries,
		[]string{
			"gpt-4.1", "gpt-4.1-mini", "o3", "o4-mini", "gemini-2.5-pro", "claude-3.7-sonnet",
			"qwen-max", "deepseek-r1", "llama-3.3-70b", "mistral-large", "command-r-plus", "doubao-pro-32k",
			"hunyuan-large",
		},
		[]string{
			"gpt-3.5-turbo", "claude-2.1", "gemini-1.5-pro", "mixtral-8x7b", "qwen-plus", "glm-4",
			"yi-large", "moonshot-v1", "doubao-lite",
		},
	)

	require.Contains(t, content, i18n.Translate(i18n.LangEn, i18n.MsgChannelUpstreamUpdateMoreChannels, map[string]any{"Count": 4}))
	require.Contains(t, content, i18n.Translate(i18n.LangEn, i18n.MsgChannelUpstreamUpdateMoreOmitted, map[string]any{"Count": 1}))
	require.Contains(t, content, i18n.Translate(i18n.LangEn, i18n.MsgChannelUpstreamUpdateFailedChannels, map[string]any{
		"Shown": 10,
		"Total": 12,
		"Ids":   "1, 2, 3, 4, 5, 6, 7, 8, 9, 10",
	}))
	require.Contains(t, content, i18n.Translate(i18n.LangEn, i18n.MsgChannelUpstreamUpdateMoreOmitted, map[string]any{"Count": 2}))
}

func TestShouldSendUpstreamModelUpdateNotification(t *testing.T) {
	channelUpstreamModelUpdateNotifyState.Lock()
	channelUpstreamModelUpdateNotifyState.lastNotifiedAt = 0
	channelUpstreamModelUpdateNotifyState.lastChangedChannels = 0
	channelUpstreamModelUpdateNotifyState.lastFailedChannels = 0
	channelUpstreamModelUpdateNotifyState.Unlock()

	baseTime := int64(2000000)

	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime, 6, 0))
	require.False(t, shouldSendUpstreamModelUpdateNotification(baseTime+3600, 6, 0))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+3600, 7, 0))
	require.False(t, shouldSendUpstreamModelUpdateNotification(baseTime+7200, 7, 0))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+8000, 0, 3))
	require.False(t, shouldSendUpstreamModelUpdateNotification(baseTime+9000, 0, 3))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+10000, 0, 4))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+90000, 7, 0))
	require.True(t, shouldSendUpstreamModelUpdateNotification(baseTime+90001, 0, 0))
}

func TestDetectAllChannelUpstreamModelUpdatesRejectsExistingActiveTask(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))

	existing, err := model.CreateSystemTask(model.SystemTaskTypeModelUpdate, nil, nil)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/upstream-models/detect-all", nil)

	DetectAllChannelUpstreamModelUpdates(ctx)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), existing.TaskID)
	require.Contains(t, recorder.Body.String(), "A model update task is already running or queued")
}

func TestOpenRouterEndpointURLPreservesRoutingSuffixAndRejectsPathInjection(t *testing.T) {
	got, err := openRouterEndpointURL("https://openrouter.ai/api", "openai/gpt-4.1-nano:nitro")
	require.NoError(t, err)
	require.Equal(t, "https://openrouter.ai/api/v1/models/openai/gpt-4.1-nano:nitro/endpoints", got)

	got, err = openRouterEndpointURL("https://proxy.example/custom", "openai/gpt-4.1")
	require.NoError(t, err)
	require.Equal(t, "https://proxy.example/custom/v1/models/openai/gpt-4.1/endpoints", got)
	got, err = openRouterEndpointURL("https://openrouter.ai/api", "~openai/gpt-astra-latest")
	require.NoError(t, err)
	assert.Equal(t, "https://openrouter.ai/api/v1/models/~openai/gpt-astra-latest/endpoints", got)

	for _, modelID := range []string{"openai/gpt-4.1/other", "openai/../admin", "openai/gpt-4.1?key=secret", "openai/gpt-4.1#fragment"} {
		_, err := openRouterEndpointURL("https://openrouter.ai/api", modelID)
		require.Error(t, err, modelID)
	}
	for _, baseURL := range []string{"https://user:pass@openrouter.ai/api", "https://openrouter.ai/api?token=secret", "https://openrouter.ai/../admin"} {
		_, err := openRouterEndpointURL(baseURL, "openai/gpt-4.1")
		require.Error(t, err, baseURL)
	}
}

func TestValidOpenRouterEndpointTagPreservesVariantsAndRejectsUnsafeSegments(t *testing.T) {
	const exactVariant = "baseten/fp4/us/priority"
	require.True(t, validOpenRouterEndpointTag(exactVariant))

	for _, tag := range []string{
		"/deepinfra/turbo",
		"deepinfra/turbo/",
		"deepinfra//turbo",
		"deepinfra/../turbo",
		"deepinfra/./turbo",
		"deepinfra/tu rbo",
		"deepinfra/tu:rbo",
		"one/two/three/four/five",
		"deepinfra/" + strings.Repeat("a", 129),
	} {
		require.False(t, validOpenRouterEndpointTag(tag), tag)
	}
}

func TestFetchOpenRouterEndpointsUsesSavedAndPreviewKeysWithoutPersistingPreview(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/v1/models/openai/gpt-4.1-nano:nitro/endpoints", r.URL.Path)
		var tag, name string
		switch r.Header.Get("Authorization") {
		case "Bearer saved-secret":
			tag, name = "saved-provider", "Saved provider"
		case "Bearer preview-secret":
			tag, name = "baseten/fp4/us/priority", "Preview provider"
		default:
			t.Errorf("unexpected authorization header: %q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"endpoints":[{"tag":"` + tag + `","name":"` + name + `","provider_name":"Test Provider"}]}}`))
	}))
	defer server.Close()

	channel := &model.Channel{Type: constant.ChannelTypeOpenRouter, Key: "saved-secret", Name: "OpenRouter test", BaseURL: &server.URL}
	require.NoError(t, db.Create(channel).Error)

	requestOpenRouterEndpointDiscovery(t, map[string]any{
		"channel_id": channel.Id,
		"model":      "openai/gpt-4.1-nano:nitro",
	}, http.StatusOK, "saved-provider")
	requestOpenRouterEndpointDiscovery(t, map[string]any{
		"channel_id": channel.Id,
		"key":        "preview-secret",
		"model":      "openai/gpt-4.1-nano:nitro",
	}, http.StatusOK, "baseten/fp4/us/priority")

	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	require.Equal(t, "saved-secret", reloaded.Key)
	require.Equal(t, 2, calls)
}

func TestFetchOpenRouterEndpointsKeepsEmptyEndpointsAsArray(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"endpoints":[]}}`))
	}))
	defer server.Close()

	recorder := requestOpenRouterEndpointDiscovery(t, map[string]any{
		"type":     constant.ChannelTypeOpenRouter,
		"base_url": server.URL,
		"key":      "empty-endpoint-key",
		"model":    "openai/gpt-4.1-nano",
	}, http.StatusOK, "")
	var response struct {
		Data struct {
			Endpoints []map[string]string `json:"endpoints"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.NotNil(t, response.Data.Endpoints)
	require.Empty(t, response.Data.Endpoints)
}

func TestFetchOpenRouterEndpointsRejectsMalformedOversizedAndLeaksNoCredentials(t *testing.T) {
	secret := "endpoint-secret"
	for _, test := range []struct {
		name   string
		write  func(http.ResponseWriter)
		key    string
		status int
	}{
		{
			name: "malformed response",
			write: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(`{"data":{"endpoints":[{"name":"OpenAI","provider_name":"OpenAI"}]}}`))
			},
			key:    secret,
			status: http.StatusBadGateway,
		},
		{
			name: "oversized response",
			write: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(strings.Repeat("x", openRouterEndpointResponseMaxBytes+1)))
			},
			key:    secret + "-large",
			status: http.StatusBadGateway,
		},
		{
			name: "upstream error is sanitized",
			write: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte("upstream echoed " + secret))
			},
			key:    secret + "-error",
			status: http.StatusBadGateway,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				test.write(w)
			}))
			defer server.Close()

			recorder := requestOpenRouterEndpointDiscovery(t, map[string]any{
				"type":     constant.ChannelTypeOpenRouter,
				"base_url": server.URL,
				"key":      test.key,
				"model":    "openai/gpt-4.1-nano",
			}, test.status, "")
			require.Contains(t, recorder.Body.String(), "Failed to fetch OpenRouter endpoints")
			require.NotContains(t, recorder.Body.String(), test.key)
			require.NotContains(t, recorder.Body.String(), secret)
			require.NotContains(t, recorder.Body.String(), "upstream echoed")
		})
	}
}

func TestFetchOpenRouterEndpointRowsDoesNotFollowCredentialRedirects(t *testing.T) {
	var redirected bool
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true
	}))
	defer redirectTarget.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	channel := &model.Channel{Type: constant.ChannelTypeOpenRouter, Key: "redirect-secret"}
	_, err := fetchOpenRouterEndpointRows(context.Background(), channel, "redirect-secret", redirector.URL, "openai/gpt-4.1")
	require.Error(t, err)
	require.False(t, redirected)
}

func requestOpenRouterEndpointDiscovery(t *testing.T, request map[string]any, wantStatus int, wantID string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := common.Marshal(request)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/openrouter/endpoints", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	FetchOpenRouterEndpoints(ctx)
	require.Equal(t, wantStatus, recorder.Code, recorder.Body.String())
	if wantID != "" {
		var response struct {
			Success bool `json:"success"`
			Data    struct {
				Model     string                       `json:"model"`
				Endpoints []service.OpenRouterEndpoint `json:"endpoints"`
				FetchedAt int64                        `json:"fetched_at"`
				Stale     bool                         `json:"stale"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		require.True(t, response.Success)
		require.Equal(t, "openai/gpt-4.1-nano:nitro", response.Data.Model)
		require.NotZero(t, response.Data.FetchedAt)
		require.False(t, response.Data.Stale)
		require.Len(t, response.Data.Endpoints, 1)
		require.Equal(t, wantID, response.Data.Endpoints[0].ID)
		require.Equal(t, "Test Provider", response.Data.Endpoints[0].ProviderName)
		wantName := "Saved provider"
		if strings.HasPrefix(wantID, "baseten/") {
			wantName = "Preview provider"
		}
		require.Equal(t, wantName, response.Data.Endpoints[0].Name)
	}
	return recorder
}

func TestFetchOpenRouterCatalogRetainsMetadataAndSeparatesAccountPreferences(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	raw := `{"id":"~openai/catalog-test","architecture":{"input_modalities":["text","image"],"output_modalities":["text"]},"context_length":null,"top_provider":{"max_completion_tokens":32768},"supported_parameters":["tools","temperature"],"pricing":{"prompt":"0.000000125","completion":"0","future_price":null},"capabilities":{"future":false}}`
	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.RequestURI()
		assert.Equal(t, "Bearer saved-catalog-secret", r.Header.Get("Authorization"))
		if r.URL.Path == "/v1/models/user" {
			assert.Equal(t, "all", r.URL.Query().Get("output_modalities"))
			_, _ = w.Write([]byte(`{"data":[],"links":{"next":null},"total_count":0}`))
			return
		}
		assert.Equal(t, "/v1/models", r.URL.Path)
		_, _ = w.Write([]byte(`{"data":[` + raw + `],"links":{"next":null},"total_count":1}`))
	}))
	defer server.Close()
	channel := &model.Channel{Type: constant.ChannelTypeOpenRouter, Key: "saved-catalog-secret", Name: "catalog", BaseURL: &server.URL, Models: "unchanged-local-model"}
	require.NoError(t, db.Create(channel).Error)

	for _, scope := range []string{"public", "public", "account"} {
		body, err := common.Marshal(map[string]any{"channel_id": channel.Id, "scope": scope})
		require.NoError(t, err)
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/openrouter/catalog", bytes.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		FetchOpenRouterCatalog(ctx)
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		var response struct {
			Success bool                           `json:"success"`
			Data    service.OpenRouterModelCatalog `json:"data"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		assert.True(t, response.Success)
		assert.Equal(t, scope, response.Data.Scope)
		assert.NotZero(t, response.Data.FetchedAt)
		assert.False(t, response.Data.Stale)
		assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
		if scope == "account" {
			assert.NotNil(t, response.Data.Models)
			assert.Empty(t, response.Data.Models)
		} else {
			require.Len(t, response.Data.Models, 1)
			assert.JSONEq(t, raw, string(response.Data.Models[0].Metadata))
		}
		assert.NotContains(t, recorder.Body.String(), "saved-catalog-secret")
	}
	assert.Len(t, requests, 2)
	reloaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "unchanged-local-model", reloaded.Models)
	assert.Equal(t, "saved-catalog-secret", reloaded.Key)
	var abilities int64
	require.NoError(t, db.Model(&model.Ability{}).Count(&abilities).Error)
	assert.Zero(t, abilities)

	// Existing upstream discovery still returns only model IDs.
	ids, err := fetchChannelUpstreamModelIDs(channel)
	require.NoError(t, err)
	assert.Equal(t, []string{"~openai/catalog-test"}, ids)
	require.Len(t, requests, 3)
	for _, want := range []string{
		"/v1/models?output_modalities=all",
		"/v1/models/user?output_modalities=all",
		"/v1/models",
	} {
		assert.Equal(t, want, <-requests)
	}
}

func TestFetchOpenRouterCatalogRejectsInvalidAndPartialResponses(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "missing data", body: `{}`},
		{name: "null data", body: `{"data":null}`},
		{name: "invalid model", body: `{"data":[{"id":"../private"}]}`},
		{name: "duplicate IDs", body: `{"data":[{"id":"openai/a"},{"id":"openai/a"}]}`},
		{name: "too many models", body: `{"data":[` + strings.Repeat(`{"id":"openai/a"},`, openRouterModelResponseMaxRows) + `{"id":"openai/b"}]}`},
		{name: "partial page", body: `{"data":[],"links":{"next":"https://untrusted.invalid/?key=secret"}}`},
		{name: "incorrect total", body: `{"data":[],"total_count":1}`},
		{name: "trailing JSON", body: `{"data":[]} {}`},
		{name: "oversized response", body: strings.Repeat("x", openRouterModelResponseMaxBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			_, err := fetchOpenRouterModelRows(context.Background(), &model.Channel{Type: constant.ChannelTypeOpenRouter}, "secret", server.URL, "public")
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "secret")
			assert.NotContains(t, err.Error(), "untrusted.invalid")
		})
	}
}

func TestFetchOpenRouterCatalogValidatesRequestsAndSanitizesFailures(t *testing.T) {
	for _, body := range []string{
		`{"scope":"invalid"}`, `{"scope":"account"}`, `{"type":1}`, `{"channel_id":-1}`,
		`{"base_url":"http://example.invalid/?key=secret"}`, `{"header_override":"invalid"}`,
		strings.Repeat("x", openRouterEndpointRequestMaxBytes+1),
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/openrouter/catalog", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		FetchOpenRouterCatalog(ctx)
		assert.GreaterOrEqual(t, recorder.Code, http.StatusBadRequest)
		assert.Less(t, recorder.Code, http.StatusInternalServerError)
		assert.NotContains(t, recorder.Body.String(), "secret")
	}
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"?key=secret", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	body, err := common.Marshal(map[string]any{"base_url": server.URL, "key": "secret", "scope": "account"})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/openrouter/catalog", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	FetchOpenRouterCatalog(ctx)
	assert.Equal(t, http.StatusBadGateway, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "secret")
	assert.NotContains(t, recorder.Body.String(), target.URL)
	assert.False(t, redirected)
}

func TestOpenRouterMetadataRechecksLocalModelAccessWithCachedCatalog(t *testing.T) {
	withSelfUseModeDisabled(t)
	withTieredBillingConfig(t, map[string]string{
		"openai/catalog-visible": "tiered_expr", "openai/catalog-token-denied": "tiered_expr", "openai/catalog-vip": "tiered_expr",
	}, map[string]string{
		"openai/catalog-visible": `tier("base", p * 1)`, "openai/catalog-token-denied": `tier("base", p * 1)`, "openai/catalog-vip": `tier("base", p * 1)`,
	})
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.Create(&[]model.Ability{
		{Group: "default", Model: "openai/catalog-visible", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "openai/catalog-token-denied", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "openai/catalog-unpriced", ChannelId: 1, Enabled: true},
		{Group: "default", Model: "openai/catalog-disabled", ChannelId: 1, Enabled: false},
		{Group: "vip", Model: "openai/catalog-vip", ChannelId: 1, Enabled: true},
	}).Error)
	models := make([]service.OpenRouterCatalogModel, 0)
	for _, name := range []string{"visible", "token-denied", "unpriced", "disabled", "vip", "upstream-only"} {
		id := "openai/catalog-" + name
		raw, err := common.Marshal(map[string]any{"id": id, "pricing": map[string]string{"prompt": "0"}})
		require.NoError(t, err)
		models = append(models, service.OpenRouterCatalogModel{ID: id, Metadata: raw})
	}
	_, err := service.GetOpenRouterModelCatalog(context.Background(), service.OpenRouterCatalogIdentity{
		BaseURL: constant.GetChannelBaseURL(constant.ChannelTypeOpenRouter),
	}, "public", func(context.Context) ([]service.OpenRouterCatalogModel, error) { return models, nil })
	require.NoError(t, err)

	for _, test := range []struct {
		name    string
		group   string
		limited bool
		allowed map[string]bool
		want    []string
	}{
		{name: "group and billing filter", group: "default", want: []string{"openai/catalog-visible", "openai/catalog-token-denied"}},
		{name: "token restriction", group: "default", limited: true, allowed: map[string]bool{"openai/catalog-visible": true}, want: []string{"openai/catalog-visible"}},
		{name: "empty allowlist denies all", group: "default", limited: true, want: []string{}},
		{name: "group changes use current authorization", group: "vip", want: []string{"openai/catalog-vip"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/models/metadata/openrouter?base_url=http://untrusted.invalid", nil)
			common.SetContextKey(ctx, constant.ContextKeyUserGroup, test.group)
			common.SetContextKey(ctx, constant.ContextKeyTokenModelLimitEnabled, test.limited)
			if test.allowed != nil {
				common.SetContextKey(ctx, constant.ContextKeyTokenModelLimit, test.allowed)
			}
			ListOpenRouterModelMetadata(ctx)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			var response struct {
				Data service.OpenRouterModelCatalog `json:"data"`
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			ids := make([]string, 0)
			for _, row := range response.Data.Models {
				ids = append(ids, row.ID)
			}
			assert.ElementsMatch(t, test.want, ids)
			assert.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
		})
	}
}

func TestOpenRouterEndpointMetadataPreservesUnknownMetrics(t *testing.T) {
	raw := `{"tag":"openai","name":"OpenAI","provider_name":"OpenAI","pricing":{"prompt":"0.000001"},"latency_last_30m":null,"throughput_last_30m":{"p50":0},"uptime_last_30m":null,"max_completion_tokens":null,"supports_tool_choice":false}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"endpoints":[` + raw + `]}}`))
	}))
	defer server.Close()
	rows, err := fetchOpenRouterEndpointRows(context.Background(), &model.Channel{Type: constant.ChannelTypeOpenRouter}, "secret", server.URL, "openai/catalog-visible")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.JSONEq(t, raw, string(rows[0].Metadata))
}

func TestOpenRouterPublicCatalogLive(t *testing.T) {
	if os.Getenv("TEST_OPENROUTER_PUBLIC_CATALOG") != "1" {
		t.Skip("set TEST_OPENROUTER_PUBLIC_CATALOG=1 for read-only live public discovery")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	channel := &model.Channel{Type: constant.ChannelTypeOpenRouter}
	baseURL := constant.GetChannelBaseURL(constant.ChannelTypeOpenRouter)
	rows, err := fetchOpenRouterModelRows(ctx, channel, "", baseURL, "public")
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	endpoints, err := fetchOpenRouterEndpointRows(ctx, channel, "", baseURL, rows[0].ID)
	require.NoError(t, err)
	t.Logf("Read-only public discovery: %d models, %d endpoints for %s", len(rows), len(endpoints), rows[0].ID)
}

func TestOpenRouterAuthenticatedCatalogLive(t *testing.T) {
	keyPath := os.Getenv("TEST_OPENROUTER_KEY_FILE")
	if keyPath == "" {
		t.Skip("set TEST_OPENROUTER_KEY_FILE for read-only authenticated discovery")
	}
	file, err := os.Open(keyPath)
	if err != nil {
		t.Fatal("cannot open OpenRouter test key file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		t.Fatal("OpenRouter test key must be in a private regular file (permissions 600)")
	}
	keyBytes, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(keyBytes) > 4096 {
		t.Fatal("cannot read a valid bounded OpenRouter test key")
	}
	key := strings.TrimSpace(string(keyBytes))
	if key == "" || strings.ContainsAny(key, " \t\r\n\"'") {
		t.Fatal("OpenRouter test key file must contain only the API key")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	channel := &model.Channel{Type: constant.ChannelTypeOpenRouter}
	baseURL := constant.GetChannelBaseURL(constant.ChannelTypeOpenRouter)
	keyURL, err := openRouterDiscoveryURL(baseURL, "/key")
	require.NoError(t, err)
	client, err := service.GetHttpClientWithProxySettings("", channel.GetSetting())
	require.NoError(t, err)
	keyClient := *client
	keyClient.Timeout = 10 * time.Second
	keyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, test := range []struct {
		name   string
		key    string
		status int
	}{
		{name: "real", key: key, status: http.StatusOK},
		{name: "invalid control", key: "invalid-openrouter-discovery-test-key", status: http.StatusUnauthorized},
	} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, keyURL, nil)
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+test.key)
		response, err := keyClient.Do(request)
		if err != nil {
			t.Fatal("OpenRouter key validation request failed before a response")
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
		require.Equal(t, test.status, response.StatusCode, "%s key validation status", test.name)
		t.Logf("%s key validation: HTTP %d", test.name, response.StatusCode)
	}
	modelID := ""
	for _, scope := range []string{"public", "account"} {
		rows, err := fetchOpenRouterModelRows(ctx, channel, key, baseURL, scope)
		require.NoError(t, err, "authenticated %s discovery failed", scope)
		require.NotNil(t, rows)
		if scope == "public" {
			require.NotEmpty(t, rows)
		}
		withPricing, withArchitecture := 0, 0
		for _, row := range rows {
			if bytes.Contains(row.Metadata, []byte(key)) {
				t.Fatal("upstream metadata unexpectedly contains the test key")
			}
			var metadata struct {
				Pricing      common.RawMessage `json:"pricing"`
				Architecture common.RawMessage `json:"architecture"`
			}
			require.NoError(t, common.Unmarshal(row.Metadata, &metadata))
			if len(metadata.Pricing) > 0 {
				withPricing++
			}
			if len(metadata.Architecture) > 0 {
				withArchitecture++
			}
			if modelID == "" && !strings.HasPrefix(row.ID, "~") && !strings.HasPrefix(row.ID, "openrouter/") {
				modelID = row.ID
			}
		}
		t.Logf("Authenticated %s discovery: %d models, %d pricing records, %d architecture records", scope, len(rows), withPricing, withArchitecture)
	}
	require.NotEmpty(t, modelID, "no concrete public model available for endpoint discovery")
	endpoints, err := fetchOpenRouterEndpointRows(ctx, channel, key, baseURL, modelID)
	require.NoError(t, err)
	for _, endpoint := range endpoints {
		if bytes.Contains(endpoint.Metadata, []byte(key)) {
			t.Fatal("upstream endpoint metadata unexpectedly contains the test key")
		}
	}
	t.Logf("Authenticated provider discovery: %d endpoints for %s", len(endpoints), modelID)

	t.Log("No inference requests were sent")
}
