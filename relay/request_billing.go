package relay

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

// PrepareRequestBilling estimates and reserves one request's charge. Transports
// provide the current request body through BodyStorage or BillingRequestInput;
// channel retries retain the resulting billing session and pricing snapshot.
func PrepareRequestBilling(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	if info == nil {
		return types.NewError(errors.New("relay info is nil"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	if apiErr := lockRequestBillingPath(c, info); apiErr != nil {
		return apiErr
	}

	needSensitiveCheck := setting.ShouldCheckPromptSensitive()
	meta := &types.TokenCountMeta{TokenType: types.TokenTypeTokenizer}
	if info.Request != nil && (needSensitiveCheck || constant.CountToken) {
		meta = info.Request.GetTokenCountMeta()
	} else {
		// Avoid building CombineText when only the pricing quantities are needed.
		switch request := info.Request.(type) {
		case *dto.GeneralOpenAIRequest:
			meta.MaxTokens = int(max(lo.FromPtr(request.MaxTokens), lo.FromPtr(request.MaxCompletionTokens)))
		case *dto.OpenAIResponsesRequest:
			meta.MaxTokens = int(lo.FromPtr(request.MaxOutputTokens))
		case *dto.ClaudeRequest:
			meta.MaxTokens = int(lo.FromPtr(request.MaxTokens))
		case *dto.ImageRequest:
			meta = request.GetTokenCountMeta()
		}
	}

	if needSensitiveCheck && meta != nil {
		if contains, words := service.CheckSensitiveText(meta.CombineText); contains {
			service.RequestPolicy(c).AddEvent(service.PolicyEvent{ErrorCode: string(types.ErrorCodeSensitiveWordsDetected), ErrorSource: "local", Decision: service.PolicyDecision{Action: "stop", Reason: "local_rejection", Source: "global"}, Health: "unchanged"})
			message := fmt.Sprintf("user sensitive words detected: %s", strings.Join(words, ", "))
			logger.LogWarn(c, message)
			return types.NewError(errors.New(message), types.ErrorCodeSensitiveWordsDetected)
		}
	}

	tokens, err := service.EstimateRequestToken(c, meta, info)
	if err != nil {
		return types.NewError(err, types.ErrorCodeCountTokenFailed)
	}
	info.SetEstimatePromptTokens(tokens)

	priceData, err := helper.ModelPriceHelper(c, info, tokens, meta)
	if err != nil {
		return types.NewError(err, types.ErrorCodeModelPriceError, types.ErrOptionWithStatusCode(http.StatusBadRequest))
	}
	if priceData.FreeModel {
		logger.LogInfo(c, common.LogText("model %s is free, skipping pre-consume", info.OriginModelName))
		return nil
	}
	if info.OpenRouterCostBilling {
		return service.PrepareOpenRouterCostBilling(c, info)
	}
	return service.PreConsumeBilling(c, priceData.QuotaToPreConsume, info)
}

func lockRequestBillingPath(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	modeInfo := info
	if info.ChannelMeta == nil {
		probe := *info
		probe.InitChannelMeta(c)
		modeInfo = &probe
	}
	configured := modeInfo.OpenRouterCostBillingConfigured()
	if info.BillingPathMode == "" {
		if configured {
			if !modeInfo.SupportsOpenRouterCostBillingRequest(c) {
				return types.NewErrorWithStatusCode(
					errors.New(i18n.T(c, i18n.MsgRelayOpenRouterCostUnsupported)),
					types.ErrorCodeModelPriceError, http.StatusBadRequest, types.ErrOptionWithSkipRetry(),
				)
			}
			info.BillingPathMode = "openrouter_cost"
			info.OpenRouterCostBilling = true
		} else {
			info.BillingPathMode = "standard"
			info.OpenRouterCostBilling = false
		}
	} else if (info.BillingPathMode == "openrouter_cost") != configured {
		return types.NewErrorWithStatusCode(
			errors.New(i18n.T(c, i18n.MsgRelayOpenRouterCostModeChanged)),
			types.ErrorCodeModelPriceError, http.StatusBadRequest, types.ErrOptionWithSkipRetry(),
		)
	}

	if info.OpenRouterCostBilling {
		// Relay retry policy reads this pointer so an attempt with a generation ID
		// can be preserved for reconciliation before another channel is selected.
		c.Set("relay_info", info)
		if c.GetString(common.RequestIdKey) == "" && info.RequestId != "" {
			c.Set(common.RequestIdKey, info.RequestId)
		}
	}
	return nil
}

// RefundFailedRequestBilling applies the common final-failure policy after all
// eligible attempts have ended. A settled BillingSession never refunds again.
func RefundFailedRequestBilling(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError) *types.NewAPIError {
	if apiErr == nil {
		if info != nil && info.OpenRouterCostBilling && info.OpenRouterCostHeartbeatStop != nil {
			service.FinalizeOpenRouterCostFailure(info)
		}
		return nil
	}
	apiErr = service.NormalizeViolationFeeError(apiErr)
	if info != nil && info.OpenRouterCostBilling {
		service.FinalizeOpenRouterCostFailure(info)
		return apiErr
	}
	if info.Billing != nil {
		info.Billing.Refund(c)
	}
	service.ChargeViolationFeeIfNeeded(c, info, apiErr)
	return apiErr
}
