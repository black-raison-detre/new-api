package openrouter_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/relay/channel/openrouter"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdaptorPreservesExistingRequestRoutes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format types.RelayFormat
		mode   int
		path   string
		want   string
	}{
		{"chat", types.RelayFormatOpenAI, relayconstant.RelayModeChatCompletions, "/v1/chat/completions", "/v1/chat/completions"},
		{"responses", types.RelayFormatOpenAIResponses, relayconstant.RelayModeResponses, "/v1/responses?trace=test", "/v1/responses?trace=test"},
		{"claude conversion", types.RelayFormatClaude, relayconstant.RelayModeChatCompletions, "/v1/messages", "/v1/chat/completions"},
		{"gemini conversion", types.RelayFormatGemini, relayconstant.RelayModeGemini, "/v1beta/models/test:generateContent", "/v1/chat/completions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, baseURL := range []string{constant.GetChannelBaseURL(constant.ChannelTypeOpenRouter), "https://gateway.example/custom"} {
				info := &relaycommon.RelayInfo{
					RelayFormat: tc.format, RelayMode: tc.mode, RequestURLPath: tc.path,
					ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenRouter, ChannelBaseUrl: baseURL},
				}
				adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
				require.IsType(t, &openrouter.Adaptor{}, adaptor)
				adaptor.Init(info)
				url, err := adaptor.GetRequestURL(info)
				require.NoError(t, err)
				assert.Equal(t, baseURL+tc.want, url)
				assert.Equal(t, "openrouter", adaptor.GetChannelName())
				assert.Empty(t, adaptor.GetModelList())
			}
		})
	}
}

func TestNativeMessagesTransportAndOptIn(t *testing.T) {
	service.InitHttpClient()
	for _, tc := range []struct {
		name                string
		enabled, enterprise bool
		format              types.RelayFormat
		mode                int
		wantPath            string
	}{
		{"existing channel", false, false, types.RelayFormatClaude, relayconstant.RelayModeChatCompletions, "/v1/chat/completions"},
		{"native messages", true, false, types.RelayFormatClaude, relayconstant.RelayModeChatCompletions, "/v1/messages"},
		{"enterprise compatibility", true, true, types.RelayFormatClaude, relayconstant.RelayModeChatCompletions, "/v1/chat/completions"},
		{"chat unchanged", true, false, types.RelayFormatOpenAI, relayconstant.RelayModeChatCompletions, "/v1/chat/completions"},
		{"responses unchanged", true, false, types.RelayFormatOpenAIResponses, relayconstant.RelayModeResponses, "/v1/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotHeader http.Header
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotHeader = r.URL.Path, r.Header.Clone()
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Generation-Id", "gen-transport-test")
				_, _ = io.WriteString(w, `{}`)
			}))
			defer server.Close()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			path := "/v1/chat/completions"
			if tc.format == types.RelayFormatClaude {
				path = "/v1/messages"
			}
			if tc.format == types.RelayFormatOpenAIResponses {
				path = "/v1/responses"
			}
			c.Request = httptest.NewRequest(http.MethodPost, path, nil)
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("anthropic-version", "2023-06-01")
			c.Request.Header.Add("anthropic-beta", "first-feature")
			c.Request.Header.Add("anthropic-beta", "second-feature")
			info := &relaycommon.RelayInfo{
				RelayFormat: tc.format, RelayMode: tc.mode, RequestURLPath: path,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType: constant.ChannelTypeOpenRouter, ChannelBaseUrl: server.URL,
					ApiKey: "channel-key", UpstreamModelName: "vendor/mapped-model",
					ChannelOtherSettings: dto.ChannelOtherSettings{OpenRouterNativeMessages: tc.enabled, OpenRouterEnterprise: &tc.enterprise},
					HeadersOverride:      map[string]any{"X-OpenRouter-Title": "Configured app", "anthropic-version": "configured-version"},
				},
			}
			adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
			adaptor.Init(info)
			body := []byte(`{"raw":"passthrough"}`)
			if tc.format == types.RelayFormatClaude {
				request := &dto.ClaudeRequest{}
				require.NoError(t, common.UnmarshalJsonStr(`{"model":"vendor/mapped-model","max_tokens":64,"temperature":0,"stream":false,"system":[{"type":"text","text":"cached system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hello"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"thinking":{"type":"disabled"}}`, request))
				info.Request = request
				converted, err := adaptor.ConvertClaudeRequest(c, info, request)
				require.NoError(t, err)
				if tc.wantPath == "/v1/messages" {
					require.IsType(t, &dto.ClaudeRequest{}, converted)
					assert.Same(t, request, converted)
				}
				body, err = common.Marshal(converted)
				require.NoError(t, err)
			}
			response, err := adaptor.DoRequest(c, info, strings.NewReader(string(body)))
			// DoRequest itself must use the outer adaptor, including when conversion
			// is skipped for raw passthrough.
			require.NoError(t, err)
			require.NoError(t, response.(*http.Response).Body.Close())
			assert.Equal(t, tc.wantPath, gotPath)
			var actualBody map[string]json.RawMessage
			require.NoError(t, common.Unmarshal(gotBody, &actualBody))
			sessionID := actualBody["session_id"]
			delete(actualBody, "session_id")
			withoutAffinity, marshalErr := common.Marshal(actualBody)
			require.NoError(t, marshalErr)
			assert.JSONEq(t, string(body), string(withoutAffinity))
			if len(sessionID) > 0 {
				var decodedSessionID string
				require.NoError(t, common.Unmarshal(sessionID, &decodedSessionID))
				assert.Equal(t, decodedSessionID, gotHeader.Get("x-session-id"))
			}
			assert.Equal(t, "Bearer channel-key", gotHeader.Get("Authorization"))
			assert.Equal(t, "gen-transport-test", info.OpenRouterGenerationID)
			assert.Empty(t, gotHeader.Get("x-api-key"))
			assert.Equal(t, "Configured app", gotHeader.Get("X-OpenRouter-Title"))
			assert.Equal(t, "configured-version", gotHeader.Get("anthropic-version"))
			if tc.wantPath == "/v1/messages" {
				assert.Equal(t, "first-feature,second-feature", gotHeader.Get("anthropic-beta"))
				var native dto.ClaudeRequest
				require.NoError(t, common.Unmarshal(gotBody, &native))
				require.NotNil(t, native.Temperature)
				assert.Zero(t, *native.Temperature)
				assert.Equal(t, "vendor/mapped-model", native.Model)
				assert.NotNil(t, native.Tools)
				assert.Contains(t, string(gotBody), "cache_control")
				assert.Contains(t, string(gotBody), `"thinking":{"type":"disabled"}`)
			}
			if tc.name == "native messages" {
				response, err = adaptor.DoRequest(c, info, strings.NewReader(`{"raw":"passthrough"}`))
				require.NoError(t, err)
				require.NoError(t, response.(*http.Response).Body.Close())
				assert.Equal(t, "/v1/messages", gotPath)
			}
		})
	}
}

func TestConfiguredOpenRouterRoutingRewritesFinalProtocolBodies(t *testing.T) {
	require.NoError(t, i18n.Init())
	service.InitHttpClient()
	for _, tc := range []struct {
		name      string
		format    types.RelayFormat
		mode      int
		path      string
		finalBody string
		wantPath  string
	}{
		{
			name:      "Chat Completions",
			format:    types.RelayFormatOpenAI,
			mode:      relayconstant.RelayModeChatCompletions,
			path:      "/v1/chat/completions",
			finalBody: `{"model":"client-override","messages":[{"role":"user","content":"hello"}],"temperature":0,"stream":false}`,
			wantPath:  "/v1/chat/completions",
		},
		{
			name:      "Responses",
			format:    types.RelayFormatOpenAIResponses,
			mode:      relayconstant.RelayModeResponses,
			path:      "/v1/responses",
			finalBody: `{"model":"client-override","input":"hello","temperature":0,"stream":false}`,
			wantPath:  "/v1/responses",
		},
		{
			name:      "Claude converted to Chat",
			format:    types.RelayFormatClaude,
			mode:      relayconstant.RelayModeChatCompletions,
			path:      "/v1/messages",
			finalBody: `{"model":"client-override","messages":[{"role":"user","content":"hello"}]}`,
			wantPath:  "/v1/chat/completions",
		},
		{
			name:      "native Claude Messages",
			format:    types.RelayFormatClaude,
			mode:      relayconstant.RelayModeChatCompletions,
			path:      "/v1/messages",
			finalBody: `{"model":"client-override","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`,
			wantPath:  "/v1/messages",
		},
		{
			name:      "Gemini converted to Chat",
			format:    types.RelayFormatGemini,
			mode:      relayconstant.RelayModeGemini,
			path:      "/v1beta/models/alias:generateContent",
			finalBody: `{"model":"client-override","messages":[{"role":"user","content":"hello"}]}`,
			wantPath:  "/v1/chat/completions",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			var gotHeader http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotHeader = r.Header.Clone()
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			}))
			defer server.Close()

			originalBody := `{"model":"alias","session_id":"caller-conversation","messages":[{"role":"user","content":"hello"}]}`
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(originalBody))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("x-session-id", "client-header-session")
			info := &relaycommon.RelayInfo{
				RelayFormat: tc.format, RelayMode: tc.mode, RequestURLPath: tc.path,
				OriginModelName: "alias", UserId: 42, UsingGroup: "default",
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType: constant.ChannelTypeOpenRouter, ChannelBaseUrl: server.URL,
					ApiKey: "channel-key", UpstreamModelName: "vendor/model:free:nitro",
					HeadersOverride: map[string]any{"x-session-id": "admin-unscoped"},
					ChannelOtherSettings: dto.ChannelOtherSettings{
						OpenRouterNativeMessages: tc.name == "native Claude Messages",
						OpenRouterModelRouting: map[string]dto.OpenRouterModelRoutingPolicy{
							"alias": {Mode: dto.OpenRouterModelRoutingFloor},
						},
					},
				},
			}
			adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
			adaptor.Init(info)
			response, err := adaptor.DoRequest(c, info, strings.NewReader(tc.finalBody))
			require.NoError(t, err)
			require.NoError(t, response.(*http.Response).Body.Close())

			assert.Equal(t, tc.wantPath, gotPath)
			var outbound map[string]any
			require.NoError(t, common.Unmarshal(gotBody, &outbound))
			assert.Equal(t, "vendor/model:free:floor", outbound["model"], "policy uses mapped model while preserving catalog variants")
			assert.Equal(t, "vendor/model:free:nitro", info.UpstreamModelName, "routing must not change billing/auth model identity")
			sessionID, ok := outbound["session_id"].(string)
			require.True(t, ok)
			assert.NotContains(t, sessionID, "caller-conversation")
			assert.Equal(t, sessionID, gotHeader.Get("x-session-id"))
			assert.NotEqual(t, "admin-unscoped", gotHeader.Get("x-session-id"))
			if tc.name == "Chat Completions" || tc.name == "Responses" {
				assert.Equal(t, float64(0), outbound["temperature"])
				assert.Equal(t, false, outbound["stream"])
			}
		})
	}
}

func TestOpenRouterStandardModeRemovesOnlyRoutingSuffix(t *testing.T) {
	require.NoError(t, i18n.Init())
	service.InitHttpClient()
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hello"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions,
		RequestURLPath: "/v1/chat/completions", OriginModelName: "alias", UserId: 18,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType: constant.ChannelTypeOpenRouter, ChannelBaseUrl: server.URL, ApiKey: "channel-key",
			UpstreamModelName: "vendor/model:free:nitro:floor",
			ChannelOtherSettings: dto.ChannelOtherSettings{OpenRouterModelRouting: map[string]dto.OpenRouterModelRoutingPolicy{
				"alias": {Mode: dto.OpenRouterModelRoutingStandard},
			}},
		},
	}
	adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
	adaptor.Init(info)
	response, err := adaptor.DoRequest(c, info, strings.NewReader(`{"model":"parameter-override","messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	require.NoError(t, response.(*http.Response).Body.Close())
	var outbound map[string]any
	require.NoError(t, common.Unmarshal(gotBody, &outbound))
	assert.Equal(t, "vendor/model:free", outbound["model"])
}

func TestOpenRouterSessionAffinityAppliesWithoutModelPolicyAndKeepsEnterpriseUntouched(t *testing.T) {
	service.InitHttpClient()
	t.Run("standard channel", func(t *testing.T) {
		var gotBody []byte
		var gotHeader http.Header
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeader = r.Header.Clone()
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		}))
		defer server.Close()

		var sessionIDs []string
		for _, userID := range []int{42, 43} {
			originalBody := `{"model":"alias","session_id":"caller-conversation","messages":[{"role":"user","content":"hello"}]}`
			finalBody := `{"model":"vendor/model:free:nitro","provider":{"order":["client-provider"]},"messages":[{"role":"user","content":"hello"}]}`
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(originalBody))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("x-session-id", "ignored-header")
			info := &relaycommon.RelayInfo{
				RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions,
				RequestURLPath: "/v1/chat/completions", OriginModelName: "alias", UserId: userID,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType: constant.ChannelTypeOpenRouter, ChannelBaseUrl: server.URL, ApiKey: "shared-channel-key",
					UpstreamModelName: "mapped/target", ChannelOtherSettings: dto.ChannelOtherSettings{},
				},
			}
			adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
			adaptor.Init(info)
			response, err := adaptor.DoRequest(c, info, strings.NewReader(finalBody))
			require.NoError(t, err)
			require.NoError(t, response.(*http.Response).Body.Close())

			var outbound map[string]any
			require.NoError(t, common.Unmarshal(gotBody, &outbound))
			assert.Equal(t, "vendor/model:free:nitro", outbound["model"], "manual suffix remains unchanged without a policy")
			provider, ok := outbound["provider"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, []any{"client-provider"}, provider["order"], "unconfigured provider routing remains unchanged")
			sessionID, ok := outbound["session_id"].(string)
			require.True(t, ok)
			assert.Equal(t, sessionID, gotHeader.Get("x-session-id"))
			assert.NotContains(t, sessionID, "caller-conversation")
			sessionIDs = append(sessionIDs, sessionID)
		}
		assert.NotEqual(t, sessionIDs[0], sessionIDs[1], "different authenticated users get separate upstream session keys")
	})

	t.Run("enterprise channel", func(t *testing.T) {
		var gotBody []byte
		var gotHeader http.Header
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeader = r.Header.Clone()
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		}))
		defer server.Close()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","session_id":"caller-conversation","messages":[{"role":"user","content":"hello"}]}`))
		c.Request.Header.Set("Content-Type", "application/json")
		enterprise := true
		info := &relaycommon.RelayInfo{
			RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions,
			RequestURLPath: "/v1/chat/completions", OriginModelName: "alias", UserId: 42,
			ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenRouter, ChannelBaseUrl: server.URL, ApiKey: "channel-key", ChannelOtherSettings: dto.ChannelOtherSettings{OpenRouterEnterprise: &enterprise}},
		}
		adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
		adaptor.Init(info)
		response, err := adaptor.DoRequest(c, info, strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hello"}]}`))
		require.NoError(t, err)
		require.NoError(t, response.(*http.Response).Body.Close())
		var outbound map[string]any
		require.NoError(t, common.Unmarshal(gotBody, &outbound))
		assert.NotContains(t, outbound, "session_id")
		assert.Empty(t, gotHeader.Get("x-session-id"))
	})

	t.Run("other channel type", func(t *testing.T) {
		var gotBody []byte
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		}))
		defer server.Close()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","session_id":"caller-conversation","messages":[{"role":"user","content":"hello"}]}`))
		c.Request.Header.Set("Content-Type", "application/json")
		info := &relaycommon.RelayInfo{
			RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions,
			RequestURLPath: "/v1/chat/completions", OriginModelName: "alias", UserId: 42,
			ChannelMeta: &relaycommon.ChannelMeta{
				ChannelType: constant.ChannelTypeOpenAI, ChannelBaseUrl: server.URL, ApiKey: "channel-key", UpstreamModelName: "mapped/model",
				ChannelOtherSettings: dto.ChannelOtherSettings{OpenRouterModelRouting: map[string]dto.OpenRouterModelRoutingPolicy{
					"alias": {Mode: dto.OpenRouterModelRoutingFloor},
				}},
			},
		}
		adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
		adaptor.Init(info)
		finalBody := `{"model":"mapped/model","messages":[{"role":"user","content":"hello"}]}`
		response, err := adaptor.DoRequest(c, info, strings.NewReader(finalBody))
		require.NoError(t, err)
		require.NoError(t, response.(*http.Response).Body.Close())
		assert.JSONEq(t, finalBody, string(gotBody))
	})
}

func TestConfiguredOpenRouterProviderPolicyPreservesRestrictions(t *testing.T) {
	require.NoError(t, i18n.Init())
	service.InitHttpClient()
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	originalBody := `{"model":"alias","session_id":"conversation","provider":{"only":["google-vertex","other"],"ignore":["blocked"],"data_collection":"deny","zdr":true,"allow_fallbacks":true,"order":["client-order"],"sort":"price"}}`
	finalBody := `{"model":"param-override","provider":{"only":["google-vertex","other"],"ignore":["final-ignore"],"data_collection":"allow","zdr":false,"allow_fallbacks":true,"order":["override-order"],"sort":"latency"}}`
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(originalBody))
	c.Request.Header.Set("Content-Type", "application/json")
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions,
		RequestURLPath: "/v1/chat/completions", OriginModelName: "alias", UserId: 7,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType: constant.ChannelTypeOpenRouter, ChannelBaseUrl: server.URL, ApiKey: "channel-key",
			UpstreamModelName: "vendor/model:free",
			ChannelOtherSettings: dto.ChannelOtherSettings{OpenRouterModelRouting: map[string]dto.OpenRouterModelRoutingPolicy{
				"alias": {Mode: dto.OpenRouterModelRoutingLocked, Provider: "google-vertex/us-east5"},
			}},
		},
	}
	adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
	adaptor.Init(info)
	response, err := adaptor.DoRequest(c, info, strings.NewReader(finalBody))
	require.NoError(t, err)
	require.NoError(t, response.(*http.Response).Body.Close())

	var outbound map[string]any
	require.NoError(t, common.Unmarshal(gotBody, &outbound))
	assert.Equal(t, "vendor/model:free", outbound["model"])
	provider, ok := outbound["provider"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"google-vertex/us-east5"}, provider["order"])
	assert.Equal(t, []any{"google-vertex/us-east5"}, provider["only"])
	assert.Equal(t, false, provider["allow_fallbacks"])
	assert.Equal(t, []any{"blocked", "final-ignore"}, provider["ignore"])
	assert.Equal(t, "deny", provider["data_collection"])
	assert.Equal(t, true, provider["zdr"])
	assert.NotContains(t, provider, "sort")
}

func TestOpenRouterPreservesProviderOptionsWithoutModelPolicy(t *testing.T) {
	service.InitHttpClient()
	provider := `{"order":["google-vertex/us-east5"],"sort":"price","only":["google-vertex"],"ignore":["blocked"],"allow_fallbacks":false,"require_parameters":false,"zdr":false,"data_collection":"allow","max_price":{"prompt":0,"completion":0},"preferred_min_throughput":0}`
	for _, tc := range []struct {
		name   string
		format types.RelayFormat
		mode   int
		path   string
		native bool
		exacto bool
	}{
		{"Chat", types.RelayFormatOpenAI, relayconstant.RelayModeChatCompletions, "/v1/chat/completions", false, false},
		{"Completions", types.RelayFormatOpenAI, relayconstant.RelayModeCompletions, "/v1/completions", false, false},
		{"Responses", types.RelayFormatOpenAIResponses, relayconstant.RelayModeResponses, "/v1/responses", false, false},
		{"native Messages", types.RelayFormatClaude, relayconstant.RelayModeChatCompletions, "/v1/messages", true, false},
		{"converted Messages", types.RelayFormatClaude, relayconstant.RelayModeChatCompletions, "/v1/messages", false, false},
		{"converted Gemini", types.RelayFormatGemini, relayconstant.RelayModeGemini, "/v1beta/models/alias:generateContent", false, false},
		{"same-model Exacto", types.RelayFormatOpenAI, relayconstant.RelayModeChatCompletions, "/v1/chat/completions", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				_, _ = io.WriteString(w, `{}`)
			}))
			defer server.Close()
			upstreamModel := "vendor/model"
			if tc.exacto {
				upstreamModel += ":exacto"
			}
			original := `{"model":"` + upstreamModel + `","provider":` + provider + `}`
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(original))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("Upgrade", "websocket")
			info := &relaycommon.RelayInfo{
				RelayFormat: tc.format, RelayMode: tc.mode, RequestURLPath: tc.path, OriginModelName: "alias",
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType: constant.ChannelTypeOpenRouter, ChannelBaseUrl: server.URL, UpstreamModelName: upstreamModel,
					ChannelOtherSettings: dto.ChannelOtherSettings{OpenRouterNativeMessages: tc.native},
				},
			}
			adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
			adaptor.Init(info)
			// Typed conversion drops unknown provider options; restore only routing
			// metadata, keeping the validated and mapped final request intact.
			response, err := adaptor.DoRequest(c, info, strings.NewReader(`{"model":"`+upstreamModel+`","messages":[],"max_tokens":32,"temperature":0,"stream":false}`))
			require.NoError(t, err)
			require.NoError(t, response.(*http.Response).Body.Close())
			var outbound map[string]json.RawMessage
			require.NoError(t, common.Unmarshal(gotBody, &outbound))
			assert.JSONEq(t, provider, string(outbound["provider"]))
			assert.JSONEq(t, `"`+upstreamModel+`"`, string(outbound["model"]))
			assert.JSONEq(t, `32`, string(outbound["max_tokens"]))
			assert.JSONEq(t, `0`, string(outbound["temperature"]))
			assert.JSONEq(t, `false`, string(outbound["stream"]))
		})
	}
}

func TestOpenRouterUnconfiguredProviderOverridePrecedence(t *testing.T) {
	require.NoError(t, i18n.Init())
	service.InitHttpClient()
	for _, tc := range []struct {
		name     string
		original string
		final    string
		want     string
	}{
		{
			name:     "administrator order wins and restrictions intersect",
			original: `{"order":["client"],"only":["google-vertex","other"],"ignore":["blocked"],"zdr":true,"require_parameters":true,"data_collection":"deny","allow_fallbacks":false}`,
			final:    `{"order":["admin"],"only":["google-vertex/us-east5"],"ignore":["admin-blocked"],"zdr":false,"require_parameters":false,"data_collection":"allow","allow_fallbacks":true}`,
			want:     `{"order":["admin"],"only":["google-vertex/us-east5"],"ignore":["blocked","admin-blocked"],"zdr":true,"require_parameters":true,"data_collection":"deny","allow_fallbacks":false}`,
		},
		{
			name:     "provider failover true stays explicit",
			original: `{"allow_fallbacks":true}`,
			final:    `{"sort":"throughput"}`,
			want:     `{"allow_fallbacks":true,"sort":"throughput"}`,
		},
		{
			name:     "disjoint allowlists reject",
			original: `{"only":["google"]}`,
			final:    `{"only":["deepinfra"]}`,
		},
		{
			name:     "conflicting price caps reject rather than relax",
			original: `{"max_price":{"prompt":0}}`,
			final:    `{"max_price":{"prompt":1}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				_, _ = io.WriteString(w, `{}`)
			}))
			defer server.Close()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","provider":`+tc.original+`}`))
			c.Request.Header.Set("Content-Type", "application/json")
			info := &relaycommon.RelayInfo{
				RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions, OriginModelName: "alias",
				ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenRouter, ChannelBaseUrl: server.URL, UpstreamModelName: "vendor/model"},
			}
			adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
			adaptor.Init(info)
			response, err := adaptor.DoRequest(c, info, strings.NewReader(`{"model":"vendor/model","provider":`+tc.final+`}`))
			if tc.want == "" {
				require.Error(t, err)
				assert.Empty(t, gotBody, "conflicting restrictions must not reach upstream")
				return
			}
			require.NoError(t, err)
			require.NoError(t, response.(*http.Response).Body.Close())
			var outbound map[string]json.RawMessage
			require.NoError(t, common.Unmarshal(gotBody, &outbound))
			assert.JSONEq(t, tc.want, string(outbound["provider"]))
		})
	}
}

func TestOpenRouterRejectsCrossModelRoutingBeforeBillingAndForwarding(t *testing.T) {
	require.NoError(t, i18n.Init())
	service.InitHttpClient()
	for _, tc := range []struct {
		name        string
		body        string
		completions bool
	}{
		{"model list", `{"model":"alias","models":["unpriced-model"]}`, false},
		{"legacy Completions model list", `{"model":"alias","models":["unpriced-model"]}`, true},
		{"fallback list", `{"model":"alias","fallbacks":["unauthorized-model"]}`, false},
		{"preset", `{"model":"alias","preset":"opaque-preset"}`, false},
		{"route", `{"model":"alias","route":"fallback"}`, false},
		{"router", `{"model":"alias","router":"opaque-router"}`, false},
		{"model router", `{"model":"alias","model_router":"opaque-router"}`, false},
		{"remote model", `{"model":"openrouter/auto"}`, false},
		{"preset model", `{"model":"@preset/opaque-preset"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode, path := relayconstant.RelayModeChatCompletions, "/v1/chat/completions"
			if tc.completions {
				mode, path = relayconstant.RelayModeCompletions, "/v1/completions"
			}
			for _, billingMode := range []string{"standard", "returned-cost"} {
				t.Run(billingMode, func(t *testing.T) {
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(tc.body))
					c.Request.Header.Set("Content-Type", "text/plain")
					info := &relaycommon.RelayInfo{
						RelayFormat: types.RelayFormatOpenAI, RelayMode: mode, OriginModelName: "alias",
						ChannelMeta: &relaycommon.ChannelMeta{
							ChannelType:          constant.ChannelTypeOpenRouter,
							ChannelOtherSettings: dto.ChannelOtherSettings{OpenRouterCostBilling: billingMode == "returned-cost"},
						},
					}
					// No database or account fixture: rejection must happen before any
					// pricing lookup, wallet/subscription reservation or cost ledger.
					apiErr := relay.PrepareRequestBilling(c, info)
					require.NotNil(t, apiErr)
					assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
					assert.True(t, types.IsSkipRetryError(apiErr))
					assert.Empty(t, info.BillingPathMode)
					assert.Nil(t, info.Billing)
					assert.Zero(t, info.FinalPreConsumedQuota)
				})
			}
			for _, source := range []string{"original", "final override"} {
				t.Run(source, func(t *testing.T) {
					forwarded := false
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = true }))
					defer server.Close()
					original, final := tc.body, `{"model":"vendor/model"}`
					if source == "final override" {
						original, final = `{"model":"alias"}`, tc.body
					}
					c, _ := gin.CreateTestContext(httptest.NewRecorder())
					c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(original))
					c.Request.Header.Set("Content-Type", "text/plain")
					c.Request.Header.Set("Upgrade", "websocket")
					info := &relaycommon.RelayInfo{
						RelayFormat: types.RelayFormatOpenAI, RelayMode: mode, OriginModelName: "alias",
						ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenRouter, ChannelBaseUrl: server.URL, UpstreamModelName: "vendor/model"},
					}
					adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
					adaptor.Init(info)
					_, err := adaptor.DoRequest(c, info, strings.NewReader(final))
					require.Error(t, err)
					assert.False(t, forwarded)
				})
			}
		})
	}
}

func TestConfiguredOpenRouterPolicyRejectsBypassAndInvalidSessionBeforeForwarding(t *testing.T) {
	require.NoError(t, i18n.Init())
	service.InitHttpClient()
	forwarded := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	for _, tc := range []struct {
		name         string
		originalBody string
		finalBody    string
		policy       dto.OpenRouterModelRoutingPolicy
		mappedModel  string
	}{
		{
			name:         "hidden fallback from original body",
			originalBody: `{"model":"alias","models":["another-model"]}`,
			finalBody:    `{"model":"alias"}`,
			policy:       dto.OpenRouterModelRoutingPolicy{Mode: dto.OpenRouterModelRoutingNitro},
		},
		{
			name:         "locked provider conflicts with only",
			originalBody: `{"model":"alias","provider":{"only":["other"]}}`,
			finalBody:    `{"model":"alias"}`,
			policy:       dto.OpenRouterModelRoutingPolicy{Mode: dto.OpenRouterModelRoutingLocked, Provider: "google"},
		},
		{
			name:         "locked provider conflicts with base provider ignore",
			originalBody: `{"model":"alias","provider":{"ignore":["deepinfra"]}}`,
			finalBody:    `{"model":"alias"}`,
			policy:       dto.OpenRouterModelRoutingPolicy{Mode: dto.OpenRouterModelRoutingLocked, Provider: "deepinfra/turbo"},
		},
		{
			name:         "provider only intersection is empty",
			originalBody: `{"model":"alias","provider":{"only":["deepinfra"]}}`,
			finalBody:    `{"model":"alias","provider":{"only":["google"]}}`,
			policy:       dto.OpenRouterModelRoutingPolicy{Mode: dto.OpenRouterModelRoutingNitro},
		},
		{
			name:         "mapped preset model",
			originalBody: `{"model":"alias"}`,
			finalBody:    `{"model":"alias"}`,
			policy:       dto.OpenRouterModelRoutingPolicy{Mode: dto.OpenRouterModelRoutingStandard},
			mappedModel:  "@preset/preset-slug",
		},
		{
			name:         "mapped model with preset suffix",
			originalBody: `{"model":"alias"}`,
			finalBody:    `{"model":"alias"}`,
			policy:       dto.OpenRouterModelRoutingPolicy{Mode: dto.OpenRouterModelRoutingStandard},
			mappedModel:  "vendor/model@preset/preset-slug",
		},
		{
			name:         "unhandled exacto routing variant",
			originalBody: `{"model":"alias"}`,
			finalBody:    `{"model":"alias"}`,
			policy:       dto.OpenRouterModelRoutingPolicy{Mode: dto.OpenRouterModelRoutingStandard},
			mappedModel:  "vendor/model:nitro:exacto",
		},
		{
			name:         "original body preset hidden by mapped final body",
			originalBody: `{"model":"@preset/preset-slug"}`,
			finalBody:    `{"model":"alias"}`,
			policy:       dto.OpenRouterModelRoutingPolicy{Mode: dto.OpenRouterModelRoutingStandard},
		},
		{
			name:         "final model override preset",
			originalBody: `{"model":"alias"}`,
			finalBody:    `{"model":"vendor/model@preset/preset-slug"}`,
			policy:       dto.OpenRouterModelRoutingPolicy{Mode: dto.OpenRouterModelRoutingStandard},
		},
		{
			name:         "invalid session identifier",
			originalBody: `{"model":"alias","session_id":"` + strings.Repeat("s", 257) + `"}`,
			finalBody:    `{"model":"alias"}`,
			policy:       dto.OpenRouterModelRoutingPolicy{Mode: dto.OpenRouterModelRoutingStandard},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forwarded = false
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.originalBody))
			c.Request.Header.Set("Content-Type", "application/json")
			mappedModel := tc.mappedModel
			if mappedModel == "" {
				mappedModel = "vendor/model"
			}
			info := &relaycommon.RelayInfo{
				RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions,
				RequestURLPath: "/v1/chat/completions", OriginModelName: "alias", UserId: 1,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType: constant.ChannelTypeOpenRouter, ChannelBaseUrl: server.URL, ApiKey: "channel-key", UpstreamModelName: mappedModel,
					ChannelOtherSettings: dto.ChannelOtherSettings{OpenRouterModelRouting: map[string]dto.OpenRouterModelRoutingPolicy{"alias": tc.policy}},
				},
			}
			adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
			adaptor.Init(info)
			response, err := adaptor.DoRequest(c, info, strings.NewReader(tc.finalBody))
			require.Error(t, err)
			assert.False(t, forwarded)
			if response != nil {
				_ = response.(*http.Response).Body.Close()
			}
		})
	}
}

func TestMalformedPersistedOpenRouterRoutingSettingsRejectBeforeForwarding(t *testing.T) {
	require.NoError(t, i18n.Init())
	service.InitHttpClient()
	forwarded := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	channel := &model.Channel{Type: constant.ChannelTypeOpenRouter, OtherSettings: `{"openrouter_model_routing":"invalid"}`}
	settings := channel.GetOtherSettings()
	require.True(t, settings.OpenRouterModelRoutingInvalid)
	originalSettings := channel.OtherSettings
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hello"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions,
		RequestURLPath: "/v1/chat/completions", OriginModelName: "alias", UserId: 8,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType: constant.ChannelTypeOpenRouter, ChannelBaseUrl: server.URL, ApiKey: "channel-key",
			ChannelOtherSettings: settings,
		},
	}
	adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
	adaptor.Init(info)
	_, err := adaptor.DoRequest(c, info, strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hello"}]}`))
	require.Error(t, err)
	assert.False(t, forwarded)
	assert.Equal(t, originalSettings, channel.OtherSettings)
}

func TestOpenRouterModelRoutingPolicyStaysFrozenAcrossRetry(t *testing.T) {
	service.InitHttpClient()
	var gotModels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = common.DecodeJson(r.Body, &body)
		var modelName string
		_ = common.Unmarshal(body["model"], &modelName)
		gotModels = append(gotModels, modelName)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions,
		RequestURLPath: "/v1/chat/completions", OriginModelName: "alias", UserId: 5,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType: constant.ChannelTypeOpenRouter, ChannelBaseUrl: server.URL, ApiKey: "channel-key", UpstreamModelName: "vendor/model",
			ChannelOtherSettings: dto.ChannelOtherSettings{OpenRouterModelRouting: map[string]dto.OpenRouterModelRoutingPolicy{
				"alias": {Mode: dto.OpenRouterModelRoutingNitro},
			}},
		},
	}
	adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
	adaptor.Init(info)
	for range 2 {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hello"}]}`))
		c.Request.Header.Set("Content-Type", "application/json")
		response, err := adaptor.DoRequest(c, info, strings.NewReader(`{"model":"alias","messages":[{"role":"user","content":"hello"}]}`))
		require.NoError(t, err)
		require.NoError(t, response.(*http.Response).Body.Close())
		info.ChannelMeta.ChannelOtherSettings.OpenRouterModelRouting = nil
	}
	assert.Equal(t, []string{"vendor/model:nitro", "vendor/model:nitro"}, gotModels)
}

func TestDoRequestCapturesGenerationIDFromErrorResponse(t *testing.T) {
	service.InitHttpClient()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Generation-Id", "gen-error-response")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	defer server.Close()

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	var recordedGenerationID string
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatOpenAI,
		OpenRouterGenerationRecorder: func(generationID string) error {
			recordedGenerationID = generationID
			return errors.New("ledger unavailable")
		},
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:    constant.ChannelTypeOpenRouter,
			ChannelBaseUrl: server.URL,
			ApiKey:         "channel-key",
		},
	}
	adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
	adaptor.Init(info)

	value, err := adaptor.DoRequest(c, info, strings.NewReader(`{"model":"test"}`))
	require.NoError(t, err)
	response, ok := value.(*http.Response)
	require.True(t, ok)
	defer response.Body.Close()
	assert.Equal(t, http.StatusTooManyRequests, response.StatusCode)
	assert.Equal(t, "gen-error-response", info.OpenRouterGenerationID)
	assert.Equal(t, "gen-error-response", recordedGenerationID)
}

func TestOpenRouterNativeMessagesClaudeHelperRoutingPolicy(t *testing.T) {
	service.InitHttpClient()
	global := model_setting.GetGlobalSettings()
	previous := *global
	t.Cleanup(func() { *global = previous })
	global.PassThroughRequestEnabled = false
	global.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{Enabled: true, AllChannels: true, ModelPatterns: []string{".*"}}
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing channel follows Responses policy", true: "native overrides Responses policy"}[enabled], func(t *testing.T) {
			var path string
			var body map[string]any
			var header http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				header = r.Header.Clone()
				_ = common.DecodeJson(r.Body, &body)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limited"}}`)
			}))
			defer server.Close()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"alias","session_id":"caller-conversation","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
			c.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenRouter)
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, server.URL)
			common.SetContextKey(c, constant.ContextKeyChannelKey, "channel-key")
			common.SetContextKey(c, constant.ContextKeyUserId, 42)
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "alias")
			common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
				OpenRouterNativeMessages: enabled,
				OpenRouterModelRouting: map[string]dto.OpenRouterModelRoutingPolicy{
					"alias": {Mode: dto.OpenRouterModelRoutingFloor},
				},
			})
			common.SetContextKey(c, constant.ContextKeyChannelParamOverride, map[string]any{"temperature": 0})
			common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{SystemPrompt: "configured system"})
			c.Set("model_mapping", `{"alias":"anthropic/claude-sonnet-4.6"}`)
			request := &dto.ClaudeRequest{}
			require.NoError(t, common.UnmarshalJsonStr(`{"model":"alias","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`, request))
			info := &relaycommon.RelayInfo{OriginModelName: "alias", UserId: 42, RelayFormat: types.RelayFormatClaude, Request: request, RequestURLPath: "/v1/messages", StartTime: time.Now()}
			apiErr := relay.ClaudeHelper(c, info)
			require.NotNil(t, apiErr)
			assert.Equal(t, http.StatusTooManyRequests, apiErr.StatusCode)
			assert.Contains(t, apiErr.Error(), "limited")
			if enabled {
				assert.Equal(t, "/v1/messages", path)
				assert.Equal(t, "configured system", body["system"])
				assert.Contains(t, body, "messages")
			} else {
				assert.Equal(t, "/v1/responses", path)
			}
			assert.Equal(t, "anthropic/claude-sonnet-4.6:floor", body["model"])
			assert.Equal(t, float64(0), body["temperature"])
			sessionID, ok := body["session_id"].(string)
			require.True(t, ok)
			assert.Equal(t, sessionID, header.Get("x-session-id"))
			assert.NotContains(t, sessionID, "caller-conversation")
		})
	}
}

func TestNativeMessagesResponsesAndCacheUsage(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	message := `{"id":"msg_test","type":"message","role":"assistant","model":"anthropic/test","content":[{"type":"tool_use","id":"tool1","name":"lookup","input":{"query":"hello"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5,"cost":0.002,"cache_creation_input_tokens":20,"cache_read_input_tokens":1691,"cache_creation":{"ephemeral_5m_input_tokens":8,"ephemeral_1h_input_tokens":12}}}`
	stream := "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"anthropic/test","content":[],"usage":{"input_tokens":10,"output_tokens":0,"cost":0,"cache_creation_input_tokens":20,"cache_read_input_tokens":1691,"cache_creation":{"ephemeral_5m_input_tokens":8,"ephemeral_1h_input_tokens":12}}}}` + "\n\n" +
		"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tool1","name":"lookup","input":{}}}` + "\n\n" +
		"event: content_block_delta\ndata: " + `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"hello\"}"}}` + "\n\n" +
		"event: content_block_stop\ndata: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5,"cost":0}}` + "\n\n" +
		"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
	for _, tc := range []struct {
		name              string
		body              string
		stream, wantError bool
	}{
		{"json", message, false, false},
		{"stream", stream, true, false},
		{"json error", `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, false, true},
		{"stream error", "event: error\ndata: " + `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}` + "\n\n", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatClaude, IsStream: tc.stream, DisablePing: true, StartTime: time.Now(),
				ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenRouter, UpstreamModelName: "anthropic/test", ChannelOtherSettings: dto.ChannelOtherSettings{OpenRouterNativeMessages: true}},
			}
			adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
			adaptor.Init(info)
			headers := make(http.Header)
			headers.Set("X-Generation-Id", "gen-response-test")
			value, apiErr := adaptor.DoResponse(c, &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(tc.body))}, info)
			if tc.wantError {
				require.NotNil(t, apiErr)
				assert.Contains(t, apiErr.Error(), "busy")
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, "gen-response-test", info.OpenRouterGenerationID)
			usage, ok := value.(*dto.Usage)
			require.True(t, ok)
			assert.Equal(t, 10, usage.PromptTokens)
			assert.Equal(t, 5, usage.CompletionTokens)
			assert.Equal(t, 1691, usage.PromptTokensDetails.CachedTokens)
			assert.Equal(t, 20, usage.PromptTokensDetails.CacheCreationTokensTotal())
			assert.Equal(t, 8, usage.ClaudeCacheCreation5mTokens)
			assert.Equal(t, 12, usage.ClaudeCacheCreation1hTokens)
			assert.Equal(t, "anthropic", usage.UsageSemantic)
			if tc.stream {
				assert.Equal(t, float64(0), usage.Cost)
				assert.True(t, info.OpenRouterCostFinal)
				assert.Contains(t, recorder.Body.String(), `"cost":0`)
			} else {
				assert.Equal(t, float64(0.002), usage.Cost)
				assert.True(t, info.OpenRouterCostFinal)
			}
			assert.Equal(t, types.RelayFormat(types.RelayFormatClaude), info.GetFinalRequestRelayFormat())
			if tc.stream {
				assert.Contains(t, recorder.Body.String(), "event: message_stop")
				assert.Contains(t, recorder.Body.String(), "event: content_block_delta")
				assert.NotContains(t, recorder.Body.String(), "[DONE]")
			} else {
				assert.JSONEq(t, message, recorder.Body.String())
			}
		})
	}
}

func TestOpenRouterChatJSONRetainsExplicitZeroCost(t *testing.T) {
	body := `{"id":"chatcmpl_test","object":"chat.completion","model":"openai/test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"cost":0}}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatOpenAI,
		RelayMode:   relayconstant.RelayModeChatCompletions,
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenRouter,
			UpstreamModelName: "openai/test",
		},
	}
	adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
	adaptor.Init(info)
	value, apiErr := adaptor.DoResponse(c, &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, info)
	require.Nil(t, apiErr)
	usage, ok := value.(*dto.Usage)
	require.True(t, ok)
	assert.Equal(t, float64(0), usage.Cost)
	assert.True(t, info.OpenRouterCostFinal)
	assert.Contains(t, recorder.Body.String(), `"cost":0`)
}

func TestOpenRouterResponsesJSONCostRequiresCompletedStatus(t *testing.T) {
	for _, tc := range []struct {
		status    string
		wantFinal bool
	}{
		{status: "completed", wantFinal: true},
		{status: "incomplete"},
		{status: "cancelled"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			body, err := common.Marshal(dto.OpenAIResponsesResponse{
				ID:     "resp_test",
				Object: "response",
				Status: []byte(`"` + tc.status + `"`),
				Usage:  &dto.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2, Cost: float64(0)},
			})
			require.NoError(t, err)
			info := &relaycommon.RelayInfo{
				RelayFormat: types.RelayFormatOpenAIResponses,
				RelayMode:   relayconstant.RelayModeResponses,
				ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenRouter, UpstreamModelName: "openai/test"},
			}
			adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
			adaptor.Init(info)
			value, apiErr := adaptor.DoResponse(c, &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(string(body))),
			}, info)
			require.Nil(t, apiErr)
			usage, ok := value.(*dto.Usage)
			require.True(t, ok)
			assert.Equal(t, float64(0), usage.Cost)
			assert.Equal(t, tc.wantFinal, info.OpenRouterCostFinal)
		})
	}
}

func TestOpenRouterCostFinalityForChatStreams(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	for _, tc := range []struct {
		name      string
		body      string
		wantFinal bool
	}{
		{
			name: "terminal accounting frame with explicit zero",
			body: "data: " + `{"id":"chatcmpl_test","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}` + "\n\n" +
				"data: " + `{"id":"chatcmpl_test","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}]}` + "\n\n" +
				"data: " + `{"id":"chatcmpl_test","choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"cost":0}}` + "\n\n" +
				"data: [DONE]\n\n",
			wantFinal: true,
		},
		{
			name: "intermediate cost followed by disconnect",
			body: "data: " + `{"id":"chatcmpl_test","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"cost":0}}` + "\n\n" +
				"data: " + `{"id":"chatcmpl_test","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}]}` + "\n\n",
			wantFinal: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{
				RelayFormat: types.RelayFormatOpenAI,
				RelayMode:   relayconstant.RelayModeChatCompletions,
				IsStream:    true,
				DisablePing: true,
				StartTime:   time.Now(),
				ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenRouter, UpstreamModelName: "openai/test"},
			}
			adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
			adaptor.Init(info)
			value, apiErr := adaptor.DoResponse(c, &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(tc.body)),
			}, info)
			require.Nil(t, apiErr)
			usage, ok := value.(*dto.Usage)
			require.True(t, ok)
			assert.Equal(t, tc.wantFinal, info.OpenRouterCostFinal)
			if tc.wantFinal {
				assert.Equal(t, float64(0), usage.Cost)
			}
		})
	}
}

func TestOpenRouterCostIsNotFinalWithoutClaudeMessageStop(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	body := "event: message_start\ndata: " + `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"anthropic/test","content":[],"usage":{"input_tokens":1,"cost":0}}}` + "\n\n" +
		"event: message_delta\ndata: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1,"cost":0}}` + "\n\n"
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatClaude,
		RelayMode:   relayconstant.RelayModeChatCompletions,
		IsStream:    true,
		DisablePing: true,
		StartTime:   time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:          constant.ChannelTypeOpenRouter,
			UpstreamModelName:    "anthropic/test",
			ChannelOtherSettings: dto.ChannelOtherSettings{OpenRouterNativeMessages: true},
		},
	}
	adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
	adaptor.Init(info)
	value, apiErr := adaptor.DoResponse(c, &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}, info)
	require.Nil(t, apiErr)
	usage, ok := value.(*dto.Usage)
	require.True(t, ok)
	assert.Equal(t, float64(0), usage.Cost)
	assert.False(t, info.OpenRouterCostFinal)
}

func TestAdaptorPreservesOpenRouterHeaders(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "overrides"}[custom], func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			c.Request.Header.Set("Content-Type", "application/json")
			info := &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenRouter, ApiKey: "test-channel-key"},
			}
			headers := make(http.Header)
			if custom {
				headers.Set("HTTP-Referer", "https://app.example")
				headers.Set("X-OpenRouter-Title", "Custom App")
				headers.Set("Authorization", "Bearer overridden-key")
				info.HeadersOverride = map[string]any{"Authorization": "Bearer overridden-key"}
			}
			adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
			adaptor.Init(info)
			require.NoError(t, adaptor.SetupRequestHeader(c, &headers, info))
			assert.Equal(t, "application/json", headers.Get("Content-Type"))
			if custom {
				assert.Equal(t, "https://app.example", headers.Get("HTTP-Referer"))
				assert.Equal(t, "Custom App", headers.Get("X-OpenRouter-Title"))
				assert.Equal(t, "Bearer overridden-key", headers.Get("Authorization"))
				return
			}
			assert.Equal(t, "https://www.newapi.ai", headers.Get("HTTP-Referer"))
			assert.Equal(t, "New API", headers.Get("X-OpenRouter-Title"))
			assert.Equal(t, "Bearer test-channel-key", headers.Get("Authorization"))
		})
	}
}

func TestAdaptorPreservesOpenRouterReasoningAndUsageRequest(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	info := &relaycommon.RelayInfo{
		OriginModelName: "vendor/model", RelayFormat: types.RelayFormatOpenAI,
		ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenRouter, UpstreamModelName: "vendor/model"},
	}
	adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
	adaptor.Init(info)
	converted, err := adaptor.ConvertOpenAIRequest(c, info, &dto.GeneralOpenAIRequest{Model: "vendor/model", ReasoningEffort: "high"})
	require.NoError(t, err)
	request, ok := converted.(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	assert.Equal(t, "vendor/model", request.Model)
	assert.Empty(t, request.ReasoningEffort)
	assert.JSONEq(t, `{"enabled":true,"effort":"high"}`, string(request.Reasoning))
	assert.JSONEq(t, `{"include":true}`, string(request.Usage))
}

func TestOpenRouterStreamingOptionsAcrossRequestFormats(t *testing.T) {
	for _, tc := range []struct {
		name       string
		format     types.RelayFormat
		native     bool
		enterprise bool
	}{
		{"Chat", types.RelayFormatOpenAI, false, false},
		{"converted Messages", types.RelayFormatClaude, false, false},
		{"converted Gemini", types.RelayFormatGemini, false, false},
		{"native Messages", types.RelayFormatClaude, true, false},
		{"enterprise Messages", types.RelayFormatClaude, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenRouter)
			common.SetContextKey(c, constant.ContextKeyChannelOtherSetting, dto.ChannelOtherSettings{
				OpenRouterNativeMessages: tc.native, OpenRouterEnterprise: common.GetPointer(tc.enterprise),
			})
			info := &relaycommon.RelayInfo{RelayFormat: tc.format, RelayMode: relayconstant.RelayModeChatCompletions, IsStream: true}
			info.InitChannelMeta(c)
			adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
			adaptor.Init(info)
			var converted any
			var err error
			switch tc.format {
			case types.RelayFormatOpenAI:
				converted, err = adaptor.ConvertOpenAIRequest(c, info, &dto.GeneralOpenAIRequest{
					Model: "vendor/model", Stream: common.GetPointer(true), StreamOptions: &dto.StreamOptions{IncludeUsage: false},
				})
			case types.RelayFormatClaude:
				request := &dto.ClaudeRequest{}
				require.NoError(t, common.UnmarshalJsonStr(`{"model":"vendor/model","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hello"}]}`, request))
				converted, err = adaptor.ConvertClaudeRequest(c, info, request)
			case types.RelayFormatGemini:
				request := &dto.GeminiChatRequest{}
				require.NoError(t, common.UnmarshalJsonStr(`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, request))
				converted, err = adaptor.ConvertGeminiRequest(c, info, request)
			}
			require.NoError(t, err)
			body, err := common.Marshal(converted)
			require.NoError(t, err)
			var outbound map[string]json.RawMessage
			require.NoError(t, common.Unmarshal(body, &outbound))
			if tc.enterprise || tc.native {
				assert.NotContains(t, outbound, "stream_options")
			} else {
				require.True(t, info.SupportStreamOptions)
				want := `{"include_usage":true}`
				if tc.format == types.RelayFormatOpenAI {
					// The shared StreamOptions DTO omits false; an empty options
					// object retains the client's opt-out without forcing true.
					want = `{}`
				}
				assert.JSONEq(t, want, string(outbound["stream_options"]))
			}
		})
	}
}

func TestAdaptorPreservesEnterpriseAndProtocolResponses(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })

	chat := `{"id":"chat-test","model":"vendor/model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"prompt_tokens_details":{"cached_tokens":1}}}`
	enterprise, err := common.Marshal(openrouter.OpenRouterEnterpriseResponse{Success: true, Data: []byte(chat)})
	require.NoError(t, err)
	stream := "data: " + `{"id":"chat-test","model":"vendor/model","choices":[{"index":0,"delta":{"role":"assistant","content":"hello"},"finish_reason":null}]}` + "\n\n" +
		"data: " + `{"id":"chat-test","model":"vendor/model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: " + `{"id":"chat-test","model":"vendor/model","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"prompt_tokens_details":{"cached_tokens":1}}}` + "\n\n" +
		"data: [DONE]\n\n"
	for _, tc := range []struct {
		name       string
		body       string
		format     types.RelayFormat
		mode       int
		enterprise bool
		stream     bool
		wantError  bool
	}{
		{name: "chat", body: chat, format: types.RelayFormatOpenAI, mode: relayconstant.RelayModeChatCompletions},
		{name: "enterprise chat", body: string(enterprise), format: types.RelayFormatOpenAI, mode: relayconstant.RelayModeChatCompletions, enterprise: true},
		{name: "enterprise failure", body: `{"success":false,"data":{}}`, format: types.RelayFormatOpenAI, mode: relayconstant.RelayModeChatCompletions, enterprise: true, wantError: true},
		{name: "malformed enterprise", body: `{`, format: types.RelayFormatOpenAI, mode: relayconstant.RelayModeChatCompletions, enterprise: true, wantError: true},
		{name: "chat stream", body: stream, format: types.RelayFormatOpenAI, mode: relayconstant.RelayModeChatCompletions, stream: true},
		{name: "responses", body: `{"id":"resp-test","object":"response","model":"vendor/model","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`, format: types.RelayFormatOpenAIResponses, mode: relayconstant.RelayModeResponses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			info := &relaycommon.RelayInfo{
				OriginModelName: "vendor/model", RelayFormat: tc.format, RelayMode: tc.mode,
				IsStream: tc.stream, DisablePing: true, StartTime: time.Now(),
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelType: constant.ChannelTypeOpenRouter, UpstreamModelName: "vendor/model",
					ChannelOtherSettings: dto.ChannelOtherSettings{OpenRouterEnterprise: &tc.enterprise},
				},
			}
			adaptor := relay.GetAdaptor(constant.APITypeOpenRouter)
			adaptor.Init(info)
			usageValue, apiErr := adaptor.DoResponse(c, &http.Response{
				StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body)),
			}, info)
			if tc.wantError {
				require.NotNil(t, apiErr)
				assert.Empty(t, recorder.Body.String())
				return
			}
			require.Nil(t, apiErr)
			usage, ok := usageValue.(*dto.Usage)
			require.True(t, ok)
			require.NotNil(t, usage)
			assert.Equal(t, 3, usage.PromptTokens)
			assert.Equal(t, 2, usage.CompletionTokens)
			assert.Equal(t, 5, usage.TotalTokens)
			if tc.mode == relayconstant.RelayModeChatCompletions {
				assert.Equal(t, 1, usage.PromptTokensDetails.CachedTokens)
				assert.Contains(t, recorder.Body.String(), "hello")
			}
			if tc.enterprise {
				assert.JSONEq(t, chat, recorder.Body.String())
			}
		})
	}
}
