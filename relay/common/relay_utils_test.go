package common

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeURLForLogMasksSensitiveQueryValues(t *testing.T) {
	rawURL := "https://example.test/v1beta/models/gemini:streamGenerateContent?alt=sse&key=sk-secret&access_token=ya29-secret&api-version=2024-02-01"

	got := SanitizeURLForLog(rawURL)

	assert.NotContains(t, got, "sk-secret")
	assert.NotContains(t, got, "ya29-secret")
	parsedURL, err := url.Parse(got)
	require.NoError(t, err)
	query := parsedURL.Query()
	assert.Equal(t, "***masked***", query.Get("key"))
	assert.Equal(t, "***masked***", query.Get("access_token"))
	assert.Equal(t, "sse", query.Get("alt"))
	assert.Equal(t, "2024-02-01", query.Get("api-version"))
}

func TestSanitizeURLForLogMasksAWSAndSecretLikeQueryKeys(t *testing.T) {
	rawURL := "https://example.test/path?X-Amz-Credential=credential&X-Amz-Signature=signature&session_token=session&client_secret=secret&model=gpt-test"

	got := SanitizeURLForLog(rawURL)

	assert.NotContains(t, got, "X-Amz-Credential=credential")
	assert.NotContains(t, got, "X-Amz-Signature=signature")
	assert.NotContains(t, got, "session_token=session")
	assert.NotContains(t, got, "client_secret=secret")
	parsedURL, err := url.Parse(got)
	require.NoError(t, err)
	query := parsedURL.Query()
	assert.Equal(t, "***masked***", query.Get("X-Amz-Credential"))
	assert.Equal(t, "***masked***", query.Get("X-Amz-Signature"))
	assert.Equal(t, "***masked***", query.Get("session_token"))
	assert.Equal(t, "***masked***", query.Get("client_secret"))
	assert.Equal(t, "gpt-test", query.Get("model"))
}

func TestSanitizeURLForLogKeepsURLWithoutSensitiveQuery(t *testing.T) {
	rawURL := "https://example.test/v1/chat/completions?api-version=2024-02-01&alt=sse"

	got := SanitizeURLForLog(rawURL)

	assert.Equal(t, rawURL, got)
}

func TestValidateMultipartDirectNormalizesImageField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := strings.NewReader(`{"model":"wan2.7-i2v","prompt":"animate","image":" https://example.com/first.png "}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/video/generations", body)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = request
	info := &RelayInfo{
		TaskRelayInfo: &TaskRelayInfo{},
	}

	taskErr := ValidateMultipartDirect(context, info)

	require.Nil(t, taskErr)
	storedReq, err := GetTaskRequest(context)
	require.NoError(t, err)
	require.Equal(t, []string{"https://example.com/first.png"}, storedReq.Images)
	require.Equal(t, constant.TaskActionImageToVideo, info.Action)
}

// TestTaskDurationBounds guards the billing invariant that user-supplied
// video duration (a quota multiplier via OtherRatio "seconds") is bounded, so
// it can never overflow quota calculation into a negative charge.
func TestTaskDurationBounds(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newContext := func(t *testing.T, body string) (*gin.Context, *RelayInfo) {
		request := httptest.NewRequest(http.MethodPost, "/v1/video/generations", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		context, _ := gin.CreateTestContext(httptest.NewRecorder())
		context.Request = request
		return context, &RelayInfo{TaskRelayInfo: &TaskRelayInfo{}}
	}

	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name:    "huge duration is rejected",
			body:    `{"model":"sora-2","prompt":"a cat","duration":9999999999}`,
			wantErr: true,
		},
		{
			name:    "huge seconds string is rejected",
			body:    `{"model":"sora-2","prompt":"a cat","seconds":"9999999999"}`,
			wantErr: true,
		},
		{
			name:    "negative duration is rejected",
			body:    `{"model":"sora-2","prompt":"a cat","duration":-8}`,
			wantErr: true,
		},
		{
			name: "normal duration is accepted",
			body: `{"model":"sora-2","prompt":"a cat","seconds":"8"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+" (multipart direct)", func(t *testing.T) {
			context, info := newContext(t, tt.body)
			taskErr := ValidateMultipartDirect(context, info)
			if tt.wantErr {
				require.NotNil(t, taskErr)
				require.Equal(t, "invalid_seconds", taskErr.Code)
			} else {
				require.Nil(t, taskErr)
			}
		})
		t.Run(tt.name+" (basic task request)", func(t *testing.T) {
			context, info := newContext(t, tt.body)
			taskErr := ValidateBasicTaskRequest(context, info, constant.TaskActionImageToVideo)
			if tt.wantErr {
				require.NotNil(t, taskErr)
				require.Equal(t, "invalid_seconds", taskErr.Code)
			} else {
				require.Nil(t, taskErr)
			}
		})
	}
}

func TestOpenRouterConversationKeyPrecedenceAndValidation(t *testing.T) {
	bodyKey, err := OpenRouterConversationKey([]byte(`{"session_id":"body-session","prompt_cache_key":"cache-key","messages":[{"role":"user","content":"hello"}]}`), "header-session")
	require.NoError(t, err)
	assert.NotContains(t, bodyKey, "body-session")

	headerKey, err := OpenRouterConversationKey([]byte(`{"prompt_cache_key":"cache-key"}`), "header-session")
	require.NoError(t, err)
	cacheKey, err := OpenRouterConversationKey([]byte(`{"prompt_cache_key":"cache-key"}`), "")
	require.NoError(t, err)
	assert.NotEqual(t, headerKey, cacheKey)

	for _, test := range []struct {
		name   string
		body   string
		header string
	}{
		{name: "body session exceeds limit", body: `{"session_id":"` + strings.Repeat("s", 257) + `"}`},
		{name: "header session exceeds limit", header: strings.Repeat("h", 257)},
		{name: "body session is not a string", body: `{"session_id":{}}`},
		{name: "malformed prompt cache key", body: `{"prompt_cache_key":42}`},
		{name: "session has surrounding whitespace", body: `{"session_id":" session "}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := OpenRouterConversationKey([]byte(test.body), test.header)
			require.Error(t, err)
		})
	}
}

func TestOpenRouterConversationAndSessionKeysAreStableAndScoped(t *testing.T) {
	firstBody := []byte(`{"system":"be concise","messages":[{"role":"user","content":"opening question"},{"role":"assistant","content":"answer"},{"role":"user","content":"follow-up"}]}`)
	secondBody := []byte(`{"system":"be concise","messages":[{"role":"user","content":"opening question"},{"role":"assistant","content":"a different answer"},{"role":"user","content":"another follow-up"}]}`)
	firstConversation, err := OpenRouterConversationKey(firstBody, "")
	require.NoError(t, err)
	secondConversation, err := OpenRouterConversationKey(secondBody, "")
	require.NoError(t, err)
	assert.Equal(t, firstConversation, secondConversation)
	differentSystem, err := OpenRouterConversationKey([]byte(`{"system":"use a different policy","messages":[{"role":"user","content":"opening question"},{"role":"assistant","content":"answer"}]}`), "")
	require.NoError(t, err)
	assert.NotEqual(t, firstConversation, differentSystem)

	key := OpenRouterAffinityKey(10, "model", "default", firstConversation)
	assert.NotEmpty(t, key)
	assert.NotEqual(t, key, OpenRouterAffinityKey(11, "model", "default", firstConversation))
	assert.NotEqual(t, key, OpenRouterAffinityKey(10, "other-model", "default", firstConversation))
	assert.NotEqual(t, key, OpenRouterAffinityKey(10, "model", "other-group", firstConversation))

	account := OpenRouterAccountScope("https://OPENROUTER.AI/api/v1/", "sk-test")
	assert.Equal(t, account, OpenRouterAccountScope("https://openrouter.ai/api/v1", "sk-test"))
	assert.NotEqual(t, account, OpenRouterAccountScope("https://openrouter.ai/api/v1", "sk-other"))

	sessionID := OpenRouterSessionID(10, "model", "default", "nitro", account, firstConversation, "sk-test")
	assert.Equal(t, sessionID, OpenRouterSessionID(10, "model", "default", "nitro", account, secondConversation, "sk-test"))
	assert.NotEqual(t, sessionID, OpenRouterSessionID(11, "model", "default", "nitro", account, firstConversation, "sk-test"))
	assert.NotEqual(t, sessionID, OpenRouterSessionID(10, "model", "default", "floor", account, firstConversation, "sk-test"))
	assert.NotContains(t, sessionID, "opening question")
}
