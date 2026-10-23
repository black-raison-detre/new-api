package openrouter

import (
	"errors"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// Adaptor retains the existing OpenAI-compatible relay behavior while giving
// OpenRouter its own dispatch point for provider-specific protocol support.
type Adaptor struct {
	openai.Adaptor
}

var _ channel.Adaptor = (*Adaptor)(nil)

func (a *Adaptor) GetRequestURL(info *relaycommon.RelayInfo) (string, error) {
	if info.UsesOpenRouterNativeMessages() {
		return relaycommon.GetFullRequestURL(info.ChannelBaseUrl, "/v1/messages", info.ChannelType), nil
	}
	return a.Adaptor.GetRequestURL(info)
}

func (a *Adaptor) SetupRequestHeader(c *gin.Context, header *http.Header, info *relaycommon.RelayInfo) error {
	if err := a.Adaptor.SetupRequestHeader(c, header, info); err != nil {
		return err
	}
	if info.UsesOpenRouterNativeMessages() {
		version := c.GetHeader("anthropic-version")
		if version == "" {
			version = "2023-06-01"
		}
		header.Set("anthropic-version", version)
		claude.CommonClaudeHeadersOperation(c, header, info)
	}
	return nil
}

func (a *Adaptor) ConvertClaudeRequest(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	if !info.UsesOpenRouterNativeMessages() {
		return a.Adaptor.ConvertClaudeRequest(c, info, request)
	}
	if request == nil {
		return nil, errors.New("request is nil")
	}
	return (&claude.Adaptor{}).ConvertClaudeRequest(c, info, request)
}

func (a *Adaptor) DoRequest(c *gin.Context, info *relaycommon.RelayInfo, body io.Reader) (any, error) {
	routedBody, err := a.routeConfiguredRequest(c, info, body)
	if err != nil {
		return nil, types.NewOpenAIError(errors.New(i18n.T(c, i18n.MsgRelayOpenRouterRoutingInvalid, nil)), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}
	if routedBody.body != nil {
		body = routedBodyReader(routedBody, body)
	}
	restoreSessionHeader := applyOpenRouterSessionHeader(info, routedBody.sessionID)
	defer restoreSessionHeader()

	var response any
	if info.UsesOpenRouterNativeMessages() {
		response, err = channel.DoApiRequest(a, c, info, body)
	} else {
		response, err = a.Adaptor.DoRequest(c, info, body)
	}
	if httpResponse, ok := response.(*http.Response); ok && httpResponse != nil && info != nil {
		generationID := httpResponse.Header.Get("X-Generation-Id")
		info.OpenRouterGenerationID = generationID
		if generationID != "" && info.OpenRouterGenerationRecorder != nil {
			if err := info.OpenRouterGenerationRecorder(generationID); err != nil {
				common.SysLog(common.LogText("failed to persist OpenRouter generation ID"))
			}
		}
	}
	return response, err
}

func (a *Adaptor) DoResponse(c *gin.Context, response *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	if response != nil && info != nil {
		info.OpenRouterGenerationID = response.Header.Get("X-Generation-Id")
	}
	var usage any
	var apiErr *types.NewAPIError
	if info.UsesOpenRouterNativeMessages() {
		usage, apiErr = (&claude.Adaptor{}).DoResponse(c, response, info)
		if info.IsStream && (apiErr != nil || info.StreamStatus == nil || info.StreamStatus.ResponseOutcome() != string(relaycommon.ResponseOutcomeCompleted)) {
			info.OpenRouterCostFinal = false
		}
	} else {
		usage, apiErr = a.Adaptor.DoResponse(c, response, info)
	}
	if info != nil && info.OpenRouterSessionAffinityEnabled && response != nil && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusBadRequest && apiErr == nil {
		streamCompleted := !info.IsStream || info.StreamStatus != nil && info.StreamStatus.ResponseOutcome() == string(relaycommon.ResponseOutcomeCompleted)
		if streamCompleted {
			service.RecordOpenRouterSessionAffinity(c, info)
		}
	}
	return usage, apiErr
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}
