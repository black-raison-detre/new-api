package openrouter

import (
	"errors"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
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
	if info.UsesOpenRouterNativeMessages() {
		return channel.DoApiRequest(a, c, info, body)
	}
	return a.Adaptor.DoRequest(c, info, body)
}

func (a *Adaptor) DoResponse(c *gin.Context, response *http.Response, info *relaycommon.RelayInfo) (any, *types.NewAPIError) {
	if info.UsesOpenRouterNativeMessages() {
		return (&claude.Adaptor{}).DoResponse(c, response, info)
	}
	return a.Adaptor.DoResponse(c, response, info)
}

func (a *Adaptor) GetModelList() []string {
	return ModelList
}

func (a *Adaptor) GetChannelName() string {
	return ChannelName
}
