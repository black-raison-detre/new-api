package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

const (
	openRouterCostMaxAttempts       = 5
	openRouterCostRequestTimeout    = 5 * time.Second
	openRouterCostMaxResponseBytes  = 64 * 1024
	openRouterCostHeartbeatInterval = time.Minute
	openRouterCostLeaseDuration     = 15 * time.Minute
)

func openRouterCostMessage(c *gin.Context, key string) string {
	if c == nil || c.Request == nil {
		return i18n.Translate("", key)
	}
	return i18n.T(c, key)
}

// PrepareOpenRouterCostBilling admits a cost-billed request without a wallet
// reservation. Subscription-only requests are rejected before forwarding.
func PrepareOpenRouterCostBilling(c *gin.Context, info *relaycommon.RelayInfo) *types.NewAPIError {
	if c == nil || c.Request == nil || info == nil || !info.OpenRouterCostBilling {
		return types.NewError(errors.New(openRouterCostMessage(c, i18n.MsgRelayOpenRouterCostNotSelected)), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}
	pref := common.NormalizeBillingPreference(info.UserSetting.BillingPreference)
	switch pref {
	case "wallet_only", "wallet_first":
	case "subscription_only":
		return types.NewErrorWithStatusCode(errors.New(openRouterCostMessage(c, i18n.MsgRelayOpenRouterCostSubscription)), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	case "subscription_first":
		hasSubscription, err := model.HasActiveUserSubscription(info.UserId)
		if err != nil {
			logger.LogError(c, common.LogText("OpenRouter billing subscription lookup failed: %s", err.Error()))
			return types.NewError(errors.New(openRouterCostMessage(c, i18n.MsgDatabaseError)), types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		}
		if hasSubscription {
			return types.NewErrorWithStatusCode(errors.New(openRouterCostMessage(c, i18n.MsgRelayOpenRouterCostActiveSub)), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
	default:
		return types.NewErrorWithStatusCode(errors.New(openRouterCostMessage(c, i18n.MsgRelayOpenRouterCostWalletPref)), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}

	userQuota, err := model.GetUserQuota(info.UserId, true)
	if err != nil {
		logger.LogError(c, common.LogText("OpenRouter billing wallet lookup failed: %s", err.Error()))
		return types.NewError(errors.New(openRouterCostMessage(c, i18n.MsgDatabaseError)), types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
	}
	if userQuota <= 0 {
		return types.NewErrorWithStatusCode(errors.New(openRouterCostMessage(c, i18n.MsgRelayOpenRouterCostWalletPositive)), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	if err := common.ValidateWalletQuota(userQuota); err != nil {
		logger.LogWarn(c, common.LogText("OpenRouter billing admission rejected an invalid wallet balance: %s", err.Error()))
		return types.NewErrorWithStatusCode(errors.New(openRouterCostMessage(c, i18n.MsgRelayOpenRouterCostWalletInvalid)), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	info.UserQuota = userQuota
	if !info.IsPlayground && !info.TokenUnlimited && info.TokenId > 0 {
		token, tokenErr := model.GetTokenByIds(info.TokenId, info.UserId)
		if tokenErr != nil {
			logger.LogError(c, common.LogText("OpenRouter billing token lookup failed: %s", tokenErr.Error()))
			return types.NewError(errors.New(openRouterCostMessage(c, i18n.MsgDatabaseError)), types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		}
		if token.RemainQuota <= 0 {
			return types.NewErrorWithStatusCode(errors.New(openRouterCostMessage(c, i18n.MsgRelayOpenRouterCostTokenPositive)), types.ErrorCodePreConsumeTokenQuotaFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
	}

	info.BillingSource = BillingSourceWallet
	info.FinalPreConsumedQuota = 0
	info.Billing = nil
	return nil
}

func PrepareOpenRouterCostForSelectedGroup(c *gin.Context, info *relaycommon.RelayInfo) error {
	if c == nil || c.Request == nil || info == nil || !info.OpenRouterCostBilling {
		return errors.New("OpenRouter actual-cost billing context is unavailable")
	}
	probe := *info
	probe.InitChannelMeta(c)
	if !probe.OpenRouterCostBillingConfigured() {
		return errors.New("channel retry changed the request billing mode; OpenRouter actual-cost billing requires an enabled standard channel")
	}
	info.OpenRouterCostGroupRatio = info.PriceData.GroupRatioInfo.GroupRatio
	ledgerInput, err := openRouterCostLedgerInput(c, info, probe.ChannelMeta)
	if err != nil {
		return err
	}
	if err := model.EnsureOpenRouterCostLedger(ledgerInput); err != nil {
		return err
	}
	requestId := info.RequestId
	info.OpenRouterGenerationRecorder = func(generationId string) error {
		return model.RecordOpenRouterGenerationID(requestId, generationId)
	}
	startOpenRouterCostHeartbeat(info)
	return nil
}

func startOpenRouterCostHeartbeat(info *relaycommon.RelayInfo) {
	if info == nil || info.RequestId == "" {
		return
	}
	stopOpenRouterCostHeartbeat(info)
	ctx, cancel := context.WithCancel(context.Background())
	info.OpenRouterCostHeartbeatStop = cancel
	requestId := info.RequestId
	go func() {
		ticker := time.NewTicker(openRouterCostHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				nextAttempt := time.Now().Add(openRouterCostLeaseDuration).Unix()
				if err := model.ExtendOpenRouterCostLedgerLease(requestId, nextAttempt); err != nil {
					logger.LogError(nil, common.LogText("failed to extend OpenRouter cost reconciliation lease for request %s", requestId))
				}
			}
		}
	}()
}

func stopOpenRouterCostHeartbeat(info *relaycommon.RelayInfo) {
	if info != nil && info.OpenRouterCostHeartbeatStop != nil {
		info.OpenRouterCostHeartbeatStop()
		info.OpenRouterCostHeartbeatStop = nil
	}
}

func FinalizeOpenRouterCostFailure(info *relaycommon.RelayInfo) {
	if info == nil {
		return
	}
	stopOpenRouterCostHeartbeat(info)
	ledger, err := model.GetOpenRouterCostLedger(info.RequestId)
	if err == nil && ledger.UpstreamCostUSD != "" {
		syncOpenRouterCostLog(info.RequestId, true)
		return
	}
	generationId := strings.TrimSpace(info.OpenRouterGenerationID)
	status := model.OpenRouterCostStatusPending
	reason := "OpenRouter request ended before a final usage cost was available"
	if generationId == "" {
		status = model.OpenRouterCostStatusUnresolved
		reason = "OpenRouter request ended without a generation id; cost cannot be reconciled"
	}
	if err := model.UpdateOpenRouterCostLedgerResult(info.RequestId, generationId, "", status, reason); err != nil {
		logger.LogError(nil, common.LogText("failed to finalize OpenRouter cost record for request %s: %s", info.RequestId, err.Error()))
		return
	}
	syncOpenRouterCostLog(info.RequestId, true)
}

func openRouterCostLedgerInput(c *gin.Context, info *relaycommon.RelayInfo, channelMeta *relaycommon.ChannelMeta) (model.OpenRouterCostLedgerInput, error) {
	if channelMeta == nil || channelMeta.ApiKey == "" {
		return model.OpenRouterCostLedgerInput{}, errors.New("OpenRouter channel credential is unavailable for cost reconciliation")
	}
	if channelMeta.ChannelType != constant.ChannelTypeOpenRouter || !channelMeta.ChannelOtherSettings.OpenRouterCostBilling || channelMeta.ChannelOtherSettings.IsOpenRouterEnterprise() {
		return model.OpenRouterCostLedgerInput{}, errors.New("channel retry changed the request billing mode; OpenRouter actual-cost billing requires an enabled standard channel")
	}
	baseURL := channelMeta.ChannelBaseUrl
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(constant.ChannelTypeOpenRouter)
	}
	endpoint := relaycommon.GetFullRequestURL(baseURL, "/v1/generation", constant.ChannelTypeOpenRouter)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return model.OpenRouterCostLedgerInput{}, errors.New("OpenRouter cost reconciliation requires an HTTPS generation endpoint without embedded credentials or query parameters")
	}
	endpoint = parsed.String()
	if strings.ContainsAny(channelMeta.ApiKey, "\r\n") {
		return model.OpenRouterCostLedgerInput{}, errors.New("OpenRouter channel credential is invalid")
	}
	keyFingerprint := fingerprintOpenRouterValue(channelMeta.ApiKey)
	endpointFingerprint := fingerprintOpenRouterValue(endpoint)
	ratio := info.PriceData.GroupRatioInfo.GroupRatio
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio < 0 {
		return model.OpenRouterCostLedgerInput{}, errors.New("OpenRouter cost group ratio is invalid")
	}
	if info.RequestId == "" {
		return model.OpenRouterCostLedgerInput{}, errors.New("OpenRouter request id is unavailable")
	}
	return model.OpenRouterCostLedgerInput{
		RequestId:             info.RequestId,
		GenerationEndpoint:    endpoint,
		EndpointFingerprint:   endpointFingerprint,
		ChannelKeyFingerprint: keyFingerprint,
		ChannelKeyIndex:       channelMeta.ChannelMultiKeyIndex,
		ChannelId:             channelMeta.ChannelId,
		ModelName:             info.GetBillingModelName(),
		UserId:                info.UserId,
		Username:              c.GetString("username"),
		TokenId:               info.TokenId,
		IsPlayground:          info.IsPlayground,
		BillingSource:         BillingSourceWallet,
		Group:                 info.UsingGroup,
		NodeName:              common.NodeName,
		GroupRatio:            decimal.NewFromFloat(ratio).String(),
		IsStream:              info.IsStream,
		QuotaPerUnit:          int64(common.QuotaPerUnit),
	}, nil
}

func fingerprintOpenRouterValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func parseOpenRouterCost(value any) (decimal.Decimal, bool) {
	var cost decimal.Decimal
	switch value := value.(type) {
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return decimal.Zero, false
		}
		cost = decimal.NewFromFloat(value)
	case float32:
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) || value < 0 {
			return decimal.Zero, false
		}
		cost = decimal.NewFromFloat32(value)
	case json.Number:
		var ok bool
		cost, ok = parseOpenRouterCostText(value.String())
		if !ok {
			return decimal.Zero, false
		}
	case json.RawMessage:
		switch common.GetJsonType(value) {
		case "number":
			var ok bool
			cost, ok = parseOpenRouterCostText(string(value))
			if !ok {
				return decimal.Zero, false
			}
		case "string":
			var text string
			if err := common.Unmarshal(value, &text); err != nil {
				return decimal.Zero, false
			}
			var ok bool
			cost, ok = parseOpenRouterCostText(text)
			if !ok {
				return decimal.Zero, false
			}
		default:
			return decimal.Zero, false
		}
	case string:
		var ok bool
		cost, ok = parseOpenRouterCostText(value)
		if !ok {
			return decimal.Zero, false
		}
	case decimal.Decimal:
		var ok bool
		cost, ok = parseOpenRouterCostText(value.String())
		if !ok {
			return decimal.Zero, false
		}
	case int:
		cost = decimal.NewFromInt(int64(value))
	case int64:
		cost = decimal.NewFromInt(value)
	case uint:
		cost = decimal.NewFromInt(int64(value))
	case uint64:
		cost = decimal.NewFromBigInt(new(big.Int).SetUint64(value), 0)
	default:
		return decimal.Zero, false
	}
	if cost.IsNegative() {
		return decimal.Zero, false
	}
	return cost, true
}

func parseOpenRouterCostText(value string) (decimal.Decimal, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return decimal.Zero, false
	}
	mantissa, exponent, hasExponent := strings.Cut(value, "e")
	if !hasExponent {
		mantissa, exponent, hasExponent = strings.Cut(value, "E")
	}
	if hasExponent {
		if strings.ContainsAny(exponent, "eE") {
			return decimal.Zero, false
		}
		exponentValue, err := strconv.Atoi(exponent)
		if err != nil || exponentValue < -18 || exponentValue > 18 {
			return decimal.Zero, false
		}
	}
	digits := 0
	for _, r := range mantissa {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	if digits == 0 || digits > 40 {
		return decimal.Zero, false
	}
	cost, err := decimal.NewFromString(value)
	if err != nil {
		return decimal.Zero, false
	}
	return cost, true
}

func openRouterCostQuota(costUSD, groupRatio string, quotaPerUnit int64) (int64, *common.QuotaClamp, error) {
	cost, err := decimal.NewFromString(costUSD)
	if err != nil || cost.IsNegative() {
		return 0, nil, errors.New("OpenRouter USD cost is invalid")
	}
	ratio, err := decimal.NewFromString(groupRatio)
	if err != nil || ratio.IsNegative() || quotaPerUnit <= 0 {
		return 0, nil, errors.New("OpenRouter frozen billing snapshot is invalid")
	}
	quota, clamp := common.QuotaFromDecimalChecked(cost.Mul(ratio).Mul(decimal.NewFromInt(quotaPerUnit)))
	return int64(quota), clamp, nil
}

func settleSavedOpenRouterCost(info *relaycommon.RelayInfo, ledger *model.OpenRouterCostLedger) (int64, *common.QuotaClamp, bool, error) {
	quota, clamp, err := openRouterCostQuota(ledger.UpstreamCostUSD, ledger.GroupRatio, ledger.QuotaPerUnit)
	if err != nil {
		return 0, nil, false, err
	}
	applied, err := model.SettleOpenRouterCostLedger(ledger.RequestId, ledger.UpstreamCostUSD, quota, clamp)
	if err != nil {
		return 0, clamp, false, err
	}
	if applied && clamp != nil {
		if info != nil {
			noteQuotaClamp(info, clamp)
		} else {
			common.SysError(common.LogText("OpenRouter cost quota saturated for request %s: %s", ledger.RequestId, clamp.Error()))
		}
	}
	return quota, clamp, applied, nil
}

func settleOpenRouterCostResponse(info *relaycommon.RelayInfo, usage *dto.Usage) (status string, quota int, costUSD string) {
	generationId := ""
	if info != nil {
		generationId = strings.TrimSpace(info.OpenRouterGenerationID)
	}
	if usage != nil && info != nil && info.OpenRouterCostFinal {
		if cost, ok := parseOpenRouterCost(usage.Cost); ok {
			costUSD = cost.String()
			if err := model.UpdateOpenRouterCostLedgerResult(info.RequestId, generationId, costUSD, model.OpenRouterCostStatusPending, ""); err == nil {
				ledger, getErr := model.GetOpenRouterCostLedger(info.RequestId)
				if getErr == nil {
					settledQuota, _, _, settleErr := settleSavedOpenRouterCost(info, ledger)
					if settleErr == nil {
						return model.OpenRouterCostStatusSettled, int(settledQuota), costUSD
					}
					if errors.Is(settleErr, model.ErrOpenRouterCostStorageBound) {
						_ = model.MarkOpenRouterCostLedgerUnresolved(info.RequestId, "OpenRouter cost exceeds an accounting column range")
						return model.OpenRouterCostStatusUnresolved, 0, costUSD
					}
					logger.LogError(nil, common.LogText("OpenRouter cost settlement deferred for request %s: %s", info.RequestId, settleErr.Error()))
				} else {
					logger.LogError(nil, common.LogText("OpenRouter cost ledger lookup failed for request %s: %s", info.RequestId, getErr.Error()))
				}
			} else {
				logger.LogError(nil, common.LogText("OpenRouter cost snapshot could not be saved for request %s: %s", info.RequestId, err.Error()))
			}
			return model.OpenRouterCostStatusPending, 0, costUSD
		}
	}

	reason := "OpenRouter response did not include a final valid USD cost"
	status = model.OpenRouterCostStatusPending
	if generationId == "" {
		status = model.OpenRouterCostStatusUnresolved
		reason = "OpenRouter generation id is missing; cost cannot be reconciled"
	}
	if info != nil {
		if err := model.UpdateOpenRouterCostLedgerResult(info.RequestId, generationId, "", status, reason); err != nil {
			logger.LogError(nil, common.LogText("OpenRouter pending cost could not be saved for request %s: %s", info.RequestId, err.Error()))
		}
	}
	return status, 0, ""
}

func postOpenRouterCostConsumeQuota(ctx *gin.Context, relayInfo *relaycommon.RelayInfo, usage *dto.Usage, extraContent []*common.Message) {
	stopOpenRouterCostHeartbeat(relayInfo)
	originUsage := usage
	billingUsage := effectiveBillingUsage(usage)
	if usage == nil {
		extraContent = append(extraContent, common.NewMessage("Upstream returned no usage"))
	}
	if originUsage != nil {
		ObserveChannelAffinityUsageCacheByRelayFormat(ctx, billingUsage, relayInfo.GetFinalRequestRelayFormat())
	}
	summary := calculateTextQuotaSummary(ctx, relayInfo, billingUsage)
	status, quota, _ := settleOpenRouterCostResponse(relayInfo, originUsage)
	summary.Quota = quota
	if status == model.OpenRouterCostStatusSettled && quota > 0 {
		checkAndSendQuotaNotify(relayInfo, quota, 0)
	}

	other := GenerateTextOtherInfo(ctx, relayInfo, summary.ModelRatio, summary.GroupRatio, summary.CompletionRatio, summary.CacheTokens, summary.CacheRatio, summary.ModelPrice, relayInfo.PriceData.GroupRatioInfo.GroupSpecialRatio)
	other.SetPublic("billing_mode", "openrouter_cost")
	other.SetPublic("billing_status", status)
	appendUsageBillingPathForLog(other, common.GetContextKeyBool(ctx, constant.ContextKeyLocalCountTokens), originUsage)
	adminRejectReason := common.GetContextKeyString(ctx, constant.ContextKeyAdminRejectReason)
	if adminRejectReason != "" {
		other.SetAdmin("reject_reason", adminRejectReason)
	}
	attachQuotaSaturation(ctx, relayInfo, other)
	contentTexts := make([]string, 0, len(extraContent))
	for _, message := range extraContent {
		if message != nil {
			contentTexts = append(contentTexts, message.Error())
		}
	}
	content := strings.Join(contentTexts, ", ")
	if len(extraContent) > 0 {
		other.SetPublic("content_parts", extraContent)
	}
	ip := ""
	if userSetting, err := model.GetUserSetting(relayInfo.UserId, false); err == nil && userSetting.RecordIpLog {
		ip = ctx.ClientIP()
	}
	payload := model.OpenRouterCostConsumeLogPayload{
		UserId:            relayInfo.UserId,
		Username:          ctx.GetString("username"),
		CreatedAt:         common.GetTimestamp(),
		Content:           content,
		ModelName:         summary.ModelName,
		TokenName:         summary.TokenName,
		Quota:             summary.Quota,
		PromptTokens:      summary.PromptTokens,
		CompletionTokens:  summary.CompletionTokens,
		UseTime:           int(summary.UseTimeSeconds),
		IsStream:          relayInfo.IsStream,
		ChannelId:         relayInfo.ChannelId,
		TokenId:           relayInfo.TokenId,
		Group:             relayInfo.UsingGroup,
		RequestId:         relayInfo.RequestId,
		UpstreamRequestId: ctx.GetString(common.UpstreamRequestIdKey),
		Ip:                ip,
		Other:             other.JSONString(),
	}
	if err := model.SaveOpenRouterCostLogPayload(relayInfo.RequestId, payload); err != nil {
		logger.LogError(ctx, common.LogText("failed to save OpenRouter cost consume log payload: %s", err.Error()))
	}
	syncOpenRouterCostLog(relayInfo.RequestId, true)
	relayInfo.PerformanceOutputTokens = int64(summary.CompletionTokens)
}

type openRouterGenerationResponse struct {
	Data struct {
		ID        string          `json:"id"`
		TotalCost json.RawMessage `json:"total_cost"`
		IsBYOK    *bool           `json:"is_byok"`
	} `json:"data"`
}

func StartOpenRouterCostReconciliation() {
	go func() {
		reconcileOpenRouterCostLedgers(context.Background())
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			reconcileOpenRouterCostLedgers(context.Background())
		}
	}()
}

func reconcileOpenRouterCostLedgers(parent context.Context) {
	now := time.Now().Unix()
	ledgers, err := model.ListOpenRouterCostLedgersForReconciliation(50, now, openRouterCostMaxAttempts)
	if err != nil {
		logger.LogError(nil, common.LogText("failed to scan OpenRouter cost ledger: %s", err.Error()))
		return
	}
	for i := range ledgers {
		ledger := &ledgers[i]
		if ledger.Status == model.OpenRouterCostStatusSettled {
			if ledger.LogUpdatePending {
				syncOpenRouterCostLog(ledger.RequestId)
			}
			continue
		}
		if ledger.UpstreamCostUSD != "" {
			if _, _, _, err := settleSavedOpenRouterCost(nil, ledger); err != nil {
				if errors.Is(err, model.ErrOpenRouterCostStorageBound) {
					_ = model.MarkOpenRouterCostLedgerUnresolved(ledger.RequestId, "OpenRouter cost exceeds an accounting column range")
				}
				logger.LogError(nil, common.LogText("failed to apply saved OpenRouter cost for request %s: %s", ledger.RequestId, err.Error()))
			}
			syncOpenRouterCostLog(ledger.RequestId)
			continue
		}
		if ledger.Status == model.OpenRouterCostStatusPending && ledger.GenerationId == "" {
			if err := model.MarkOpenRouterCostLedgerUnresolved(ledger.RequestId, "OpenRouter generation id is missing; cost cannot be reconciled"); err != nil {
				logger.LogError(nil, common.LogText("failed to mark OpenRouter request unresolved: %s", err.Error()))
			}
			syncOpenRouterCostLog(ledger.RequestId)
			continue
		}
		claimed, ok, err := model.ClaimOpenRouterCostLedger(ledger.Id, now, 30, openRouterCostMaxAttempts)
		if err != nil {
			logger.LogError(nil, common.LogText("failed to claim OpenRouter cost request %s: %s", ledger.RequestId, err.Error()))
			continue
		}
		if !ok {
			if ledger.LogUpdatePending {
				syncOpenRouterCostLog(ledger.RequestId)
			}
			continue
		}
		key, endpoint, identityErr := resolveOpenRouterCostIdentity(claimed)
		if identityErr != nil {
			_ = model.MarkOpenRouterCostLedgerUnresolved(claimed.RequestId, "OpenRouter channel credential or endpoint changed; original account cannot be verified")
			syncOpenRouterCostLog(claimed.RequestId)
			continue
		}
		cost, lookupErr := fetchOpenRouterGenerationCost(parent, endpoint, claimed.GenerationId, key)
		if lookupErr != nil {
			if err := model.RecordOpenRouterCostLookupFailure(claimed.Id, lookupErr.Error(), openRouterCostMaxAttempts, time.Duration(1<<min(claimed.LookupAttempts, 5))*time.Minute); err != nil {
				logger.LogError(nil, common.LogText("failed to record OpenRouter generation lookup failure: %s", err.Error()))
			}
			syncOpenRouterCostLog(claimed.RequestId)
			continue
		}
		if err := model.UpdateOpenRouterCostLedgerResult(claimed.RequestId, claimed.GenerationId, cost.String(), model.OpenRouterCostStatusPending, ""); err != nil {
			logger.LogError(nil, common.LogText("failed to persist OpenRouter generation cost: %s", err.Error()))
			continue
		}
		saved, err := model.GetOpenRouterCostLedger(claimed.RequestId)
		if err != nil {
			logger.LogError(nil, common.LogText("failed to reload OpenRouter ledger after cost lookup: %s", err.Error()))
			continue
		}
		if _, _, _, err := settleSavedOpenRouterCost(nil, saved); err != nil {
			if errors.Is(err, model.ErrOpenRouterCostStorageBound) {
				_ = model.MarkOpenRouterCostLedgerUnresolved(claimed.RequestId, "OpenRouter cost exceeds an accounting column range")
			}
			logger.LogError(nil, common.LogText("failed to settle reconciled OpenRouter cost for request %s: %s", claimed.RequestId, err.Error()))
		}
		syncOpenRouterCostLog(claimed.RequestId)
	}
}

func resolveOpenRouterCostIdentity(ledger *model.OpenRouterCostLedger) (string, string, error) {
	if ledger == nil || ledger.ChannelId <= 0 || ledger.ChannelKeyFingerprint == "" || ledger.EndpointFingerprint == "" {
		return "", "", errors.New("OpenRouter reconciliation identity is incomplete")
	}
	channel, err := model.GetChannelById(ledger.ChannelId, true)
	if err != nil || channel == nil || channel.Type != constant.ChannelTypeOpenRouter {
		return "", "", errors.New("OpenRouter channel is unavailable")
	}
	key := channel.Key
	if channel.ChannelInfo.IsMultiKey {
		keys := channel.GetKeys()
		if ledger.ChannelKeyIndex < 0 || ledger.ChannelKeyIndex >= len(keys) {
			return "", "", errors.New("OpenRouter channel key index is unavailable")
		}
		key = keys[ledger.ChannelKeyIndex]
		if common.GetJsonType([]byte(key)) == "string" {
			var decoded string
			if err := common.Unmarshal([]byte(key), &decoded); err != nil {
				return "", "", errors.New("OpenRouter channel key could not be decoded")
			}
			key = decoded
		}
	}
	if key == "" || fingerprintOpenRouterValue(key) != ledger.ChannelKeyFingerprint {
		return "", "", errors.New("OpenRouter channel credential changed")
	}
	baseURL := ""
	if channel.BaseURL != nil {
		baseURL = *channel.BaseURL
	}
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(constant.ChannelTypeOpenRouter)
	}
	endpoint := relaycommon.GetFullRequestURL(baseURL, "/v1/generation", constant.ChannelTypeOpenRouter)
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", errors.New("OpenRouter generation endpoint is no longer safe")
	}
	endpoint = parsed.String()
	if endpoint != ledger.GenerationEndpoint || fingerprintOpenRouterValue(endpoint) != ledger.EndpointFingerprint {
		return "", "", errors.New("OpenRouter channel endpoint changed")
	}
	return key, endpoint, nil
}

func fetchOpenRouterGenerationCost(parent context.Context, endpoint, generationId, key string) (decimal.Decimal, error) {
	if generationId == "" || !strings.HasPrefix(generationId, "gen-") {
		return decimal.Zero, errors.New("OpenRouter generation id is invalid")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return decimal.Zero, errors.New("OpenRouter generation endpoint is invalid")
	}
	query := parsed.Query()
	query.Set("id", generationId)
	parsed.RawQuery = query.Encode()
	ctx, cancel := context.WithTimeout(parent, openRouterCostRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return decimal.Zero, errors.New("OpenRouter generation request could not be built")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{
		Timeout: openRouterCostRequestTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(req)
	if err != nil {
		return decimal.Zero, errors.New("OpenRouter generation lookup request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return decimal.Zero, fmt.Errorf("OpenRouter generation lookup returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, openRouterCostMaxResponseBytes+1))
	if err != nil || len(body) > openRouterCostMaxResponseBytes {
		return decimal.Zero, errors.New("OpenRouter generation lookup response was unreadable or too large")
	}
	var result openRouterGenerationResponse
	if err := common.DecodeJson(bytes.NewReader(body), &result); err != nil {
		return decimal.Zero, errors.New("OpenRouter generation lookup response was invalid")
	}
	if result.Data.ID != generationId {
		return decimal.Zero, errors.New("OpenRouter generation lookup returned a different generation id")
	}
	cost, ok := parseOpenRouterCost(result.Data.TotalCost)
	if !ok {
		return decimal.Zero, errors.New("OpenRouter generation lookup returned an invalid total_cost")
	}
	return cost, nil
}

func syncOpenRouterCostLog(requestId string, force ...bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := model.SyncOpenRouterCostConsumeLog(ctx, requestId, force...); err != nil {
		logger.LogError(nil, common.LogText("failed to synchronize OpenRouter consume log: %s", err.Error()))
	}
}
