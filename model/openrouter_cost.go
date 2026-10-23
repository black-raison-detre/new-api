package model

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	OpenRouterCostStatusPending             = "pending"
	OpenRouterCostStatusSettled             = "settled"
	OpenRouterCostStatusUnresolved          = "unresolved"
	openRouterCostInitialLookupDelaySeconds = 15 * 60
)

var ErrOpenRouterCostLedgerIdentityChanged = errors.New("OpenRouter cost ledger identity changed")
var ErrOpenRouterCostStorageBound = errors.New("OpenRouter settlement exceeds an accounting column bound")
var ErrOpenRouterCostWalletUnavailable = errors.New("OpenRouter wallet funding requires positive quota")
var ErrOpenRouterCostWalletInvalid = errors.New("OpenRouter wallet quota is outside its supported range")
var ErrOpenRouterCostSubscriptionUnavailable = errors.New("OpenRouter subscription funding requires an active subscription with available quota")
var ErrOpenRouterCostFundingChanged = errors.New("OpenRouter selected subscription is no longer available")

// OpenRouterCostLedger keeps the charge, recovery identity, and log-update
// state durable in the primary database. It intentionally stores only a
// one-way channel-key fingerprint, never a usable upstream credential.
type OpenRouterCostLedger struct {
	Id                                int    `json:"id" gorm:"primaryKey"`
	RequestId                         string `json:"request_id" gorm:"type:varchar(64);uniqueIndex;not null"`
	GenerationId                      string `json:"generation_id" gorm:"type:varchar(128);index"`
	GenerationEndpoint                string `json:"-" gorm:"type:text"`
	EndpointFingerprint               string `json:"-" gorm:"type:char(64)"`
	ChannelKeyFingerprint             string `json:"-" gorm:"type:char(64)"`
	ChannelKeyIndex                   int    `json:"-"`
	ChannelId                         int    `json:"channel_id" gorm:"index"`
	ModelName                         string `json:"model_name" gorm:"type:varchar(128)"`
	UserId                            int    `json:"user_id" gorm:"index"`
	Username                          string `json:"-" gorm:"type:varchar(64)"`
	NodeName                          string `json:"-" gorm:"type:varchar(64)"`
	TokenId                           int    `json:"token_id" gorm:"index"`
	IsPlayground                      bool   `json:"-"`
	BillingSource                     string `json:"-" gorm:"type:varchar(16)"`
	SubscriptionId                    int    `json:"-" gorm:"column:subscription_id;index"`
	SubscriptionPlanId                int    `json:"-" gorm:"column:subscription_plan_id"`
	SubscriptionPlanTitle             string `json:"-" gorm:"type:varchar(128)"`
	SubscriptionAmountTotal           int64  `json:"-" gorm:"type:bigint"`
	SubscriptionAmountUsedAtAdmission int64  `json:"-" gorm:"type:bigint"`
	SubscriptionAmountUsedAfter       int64  `json:"-" gorm:"type:bigint"`
	SubscriptionLastResetTime         int64  `json:"-" gorm:"type:bigint"`
	SubscriptionNextResetTime         int64  `json:"-" gorm:"type:bigint"`
	SubscriptionQuotaCycleVersion     int64  `json:"-" gorm:"type:bigint;not null;default:0"`
	SubscriptionCycleApplied          bool   `json:"-" gorm:"not null;default:false"`
	Group                             string `json:"group" gorm:"type:varchar(64)"`
	GroupRatio                        string `json:"group_ratio" gorm:"type:varchar(64)"`
	QuotaPerUnit                      int64  `json:"-" gorm:"type:bigint"`
	UpstreamCostUSD                   string `json:"upstream_cost_usd" gorm:"type:text"`
	Quota                             int64  `json:"quota" gorm:"type:bigint"`
	QuotaDataId                       int    `json:"-" gorm:"column:quota_data_id"`
	QuotaSaturation                   string `json:"-" gorm:"type:text"`
	LogPayload                        string `json:"-" gorm:"type:text"`
	Status                            string `json:"status" gorm:"type:varchar(16);index"`
	LookupAttempts                    int    `json:"-"`
	NextAttemptAt                     int64  `json:"-" gorm:"type:bigint;index"`
	ClaimedUntil                      int64  `json:"-" gorm:"type:bigint;index"`
	LogUpdatePending                  bool   `json:"-"`
	LogNextAttemptAt                  int64  `json:"-" gorm:"type:bigint;index"`
	IsStream                          bool   `json:"-"`
	LastError                         string `json:"-" gorm:"type:text"`
	CreatedAt                         int64  `json:"created_at" gorm:"type:bigint;index"`
	UpdatedAt                         int64  `json:"updated_at" gorm:"type:bigint"`
	SettledAt                         int64  `json:"settled_at,omitempty" gorm:"type:bigint"`
}

type OpenRouterCostLedgerInput struct {
	RequestId                         string
	GenerationId                      string
	GenerationEndpoint                string
	EndpointFingerprint               string
	ChannelKeyFingerprint             string
	ChannelKeyIndex                   int
	ChannelId                         int
	ModelName                         string
	UserId                            int
	Username                          string
	TokenId                           int
	IsPlayground                      bool
	BillingSource                     string
	SubscriptionId                    int
	SubscriptionPlanId                int
	SubscriptionPlanTitle             string
	SubscriptionAmountTotal           int64
	SubscriptionAmountUsedAtAdmission int64
	SubscriptionLastResetTime         int64
	SubscriptionNextResetTime         int64
	SubscriptionQuotaCycleVersion     int64
	Group                             string
	NodeName                          string
	GroupRatio                        string
	IsStream                          bool
	QuotaPerUnit                      int64
}

// OpenRouterCostFundingSelection is the funding identity frozen before an
// OpenRouter actual-cost request is forwarded. Wallet quota is populated only
// for wallet-funded requests.
type OpenRouterCostFundingSelection struct {
	BillingSource                     string
	UserQuota                         int
	SubscriptionId                    int
	SubscriptionPlanId                int
	SubscriptionPlanTitle             string
	SubscriptionAmountTotal           int64
	SubscriptionAmountUsedAtAdmission int64
	SubscriptionLastResetTime         int64
	SubscriptionNextResetTime         int64
	SubscriptionQuotaCycleVersion     int64
}

type OpenRouterCostConsumeLogPayload struct {
	UserId            int
	Username          string
	CreatedAt         int64
	Type              int
	Content           string
	ModelName         string
	TokenName         string
	Quota             int
	PromptTokens      int
	CompletionTokens  int
	UseTime           int
	IsStream          bool
	ChannelId         int
	TokenId           int
	Group             string
	Ip                string
	RequestId         string
	UpstreamRequestId string
	Other             string
}

func selectOpenRouterCostWalletTx(user *User) (*OpenRouterCostFundingSelection, error) {
	if err := common.ValidateWalletQuota(user.Quota); err != nil {
		return nil, ErrOpenRouterCostWalletInvalid
	}
	if user.Quota <= 0 {
		return nil, ErrOpenRouterCostWalletUnavailable
	}
	return &OpenRouterCostFundingSelection{BillingSource: "wallet", UserQuota: user.Quota}, nil
}

// SelectOpenRouterCostFunding follows the user's normal funding preference and
// locks the selected subscription while capturing its current quota cycle.
func SelectOpenRouterCostFunding(userId int, preference string) (*OpenRouterCostFundingSelection, error) {
	if userId <= 0 {
		return nil, errors.New("invalid OpenRouter cost user id")
	}
	preference = common.NormalizeBillingPreference(preference)
	selection := new(OpenRouterCostFundingSelection)
	now := GetDBTimestamp()
	err := DB.Transaction(func(tx *gorm.DB) error {
		// Match settlement's user→subscription lock order. This row lock is
		// independent of wallet spendability, so subscription-only requests do
		// not require any positive wallet quota.
		var user User
		if err := lockForUpdate(tx).Where("id = ?", userId).First(&user).Error; err != nil {
			return err
		}
		selectSubscription := func() (bool, error) {
			sub, plan, hasActive, allowWalletOverflow, err := selectOpenRouterCostSubscriptionTx(tx, userId, now)
			if err != nil {
				return false, err
			}
			if sub == nil {
				return !hasActive || allowWalletOverflow, ErrOpenRouterCostSubscriptionUnavailable
			}
			*selection = OpenRouterCostFundingSelection{
				BillingSource:                     "subscription",
				SubscriptionId:                    sub.Id,
				SubscriptionPlanId:                plan.Id,
				SubscriptionPlanTitle:             plan.Title,
				SubscriptionAmountTotal:           sub.AmountTotal,
				SubscriptionAmountUsedAtAdmission: sub.AmountUsed,
				SubscriptionLastResetTime:         sub.LastResetTime,
				SubscriptionNextResetTime:         sub.NextResetTime,
				SubscriptionQuotaCycleVersion:     sub.QuotaCycleVersion,
			}
			return true, nil
		}
		selectWallet := func() error {
			wallet, err := selectOpenRouterCostWalletTx(&user)
			if err != nil {
				return err
			}
			*selection = *wallet
			return nil
		}

		switch preference {
		case "wallet_only":
			return selectWallet()
		case "subscription_only":
			_, err := selectSubscription()
			return err
		case "wallet_first":
			if err := selectWallet(); err == nil {
				return nil
			} else if !errors.Is(err, ErrOpenRouterCostWalletUnavailable) {
				return err
			}
			_, err := selectSubscription()
			return err
		case "subscription_first":
			canUseWallet, err := selectSubscription()
			if err == nil {
				return nil
			}
			if !errors.Is(err, ErrOpenRouterCostSubscriptionUnavailable) {
				return err
			}
			if !canUseWallet {
				return ErrOpenRouterCostSubscriptionUnavailable
			}
			return selectWallet()
		default:
			return errors.New("invalid OpenRouter cost billing preference")
		}
	})
	if err != nil {
		return nil, err
	}
	return selection, nil
}

func refreshOpenRouterCostSubscriptionSnapshotTx(tx *gorm.DB, input *OpenRouterCostLedgerInput, now int64) error {
	var sub UserSubscription
	if err := lockForUpdate(tx).
		Where("id = ? AND user_id = ?", input.SubscriptionId, input.UserId).
		First(&sub).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrOpenRouterCostFundingChanged
		}
		return err
	}
	if sub.Status != "active" || sub.EndTime <= now || sub.PlanId != input.SubscriptionPlanId {
		return ErrOpenRouterCostFundingChanged
	}
	plan, err := getSubscriptionPlanByIdTx(tx, sub.PlanId)
	if err != nil {
		return err
	}
	if err := maybeResetUserSubscriptionWithPlanTx(tx, &sub, plan, now); err != nil {
		return err
	}
	if sub.AmountTotal > 0 && sub.AmountUsed >= sub.AmountTotal {
		return ErrOpenRouterCostFundingChanged
	}
	input.SubscriptionPlanTitle = plan.Title
	input.SubscriptionAmountTotal = sub.AmountTotal
	input.SubscriptionAmountUsedAtAdmission = sub.AmountUsed
	input.SubscriptionLastResetTime = sub.LastResetTime
	input.SubscriptionNextResetTime = sub.NextResetTime
	input.SubscriptionQuotaCycleVersion = sub.QuotaCycleVersion
	return nil
}

// EnsureOpenRouterCostLedger creates the pre-upstream durable record or
// refreshes its per-attempt channel/group snapshot while no charge is settled.
func EnsureOpenRouterCostLedger(input OpenRouterCostLedgerInput) error {
	if input.RequestId == "" || input.GenerationEndpoint == "" || input.EndpointFingerprint == "" || input.ChannelKeyFingerprint == "" || input.UserId <= 0 || input.ChannelId <= 0 || (input.BillingSource != "wallet" && input.BillingSource != "subscription") {
		return errors.New("invalid OpenRouter cost ledger identity")
	}
	if input.BillingSource == "subscription" && (input.SubscriptionId <= 0 || input.SubscriptionPlanId <= 0 || input.SubscriptionQuotaCycleVersion < 0) {
		return errors.New("invalid OpenRouter subscription funding snapshot")
	}
	if input.BillingSource == "wallet" && (input.SubscriptionId != 0 || input.SubscriptionPlanId != 0 || input.SubscriptionPlanTitle != "" || input.SubscriptionAmountTotal != 0 || input.SubscriptionAmountUsedAtAdmission != 0 || input.SubscriptionLastResetTime != 0 || input.SubscriptionNextResetTime != 0 || input.SubscriptionQuotaCycleVersion != 0) {
		return errors.New("wallet-funded OpenRouter ledger includes subscription state")
	}
	if input.QuotaPerUnit <= 0 {
		return errors.New("invalid OpenRouter quota unit")
	}
	now := common.GetTimestamp()
	fundingCheckTime := int64(0)
	if input.BillingSource == "subscription" {
		fundingCheckTime = GetDBTimestamp()
	}
	var existing OpenRouterCostLedger
	lookupErr := DB.Where("request_id = ?", input.RequestId).First(&existing).Error
	alreadyExists := lookupErr == nil
	if lookupErr != nil && !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
		return lookupErr
	}

	return DB.Transaction(func(tx *gorm.DB) error {
		if !alreadyExists {
			candidate := OpenRouterCostLedger{
				RequestId:                         input.RequestId,
				GenerationId:                      input.GenerationId,
				GenerationEndpoint:                input.GenerationEndpoint,
				EndpointFingerprint:               input.EndpointFingerprint,
				ChannelKeyFingerprint:             input.ChannelKeyFingerprint,
				ChannelKeyIndex:                   input.ChannelKeyIndex,
				ChannelId:                         input.ChannelId,
				ModelName:                         input.ModelName,
				UserId:                            input.UserId,
				Username:                          input.Username,
				TokenId:                           input.TokenId,
				IsPlayground:                      input.IsPlayground,
				BillingSource:                     input.BillingSource,
				SubscriptionId:                    input.SubscriptionId,
				SubscriptionPlanId:                input.SubscriptionPlanId,
				SubscriptionPlanTitle:             input.SubscriptionPlanTitle,
				SubscriptionAmountTotal:           input.SubscriptionAmountTotal,
				SubscriptionAmountUsedAtAdmission: input.SubscriptionAmountUsedAtAdmission,
				SubscriptionAmountUsedAfter:       input.SubscriptionAmountUsedAtAdmission,
				SubscriptionLastResetTime:         input.SubscriptionLastResetTime,
				SubscriptionNextResetTime:         input.SubscriptionNextResetTime,
				SubscriptionQuotaCycleVersion:     input.SubscriptionQuotaCycleVersion,
				Group:                             input.Group,
				NodeName:                          input.NodeName,
				GroupRatio:                        input.GroupRatio,
				IsStream:                          input.IsStream,
				QuotaPerUnit:                      input.QuotaPerUnit,
				Status:                            OpenRouterCostStatusPending,
				NextAttemptAt:                     now + openRouterCostInitialLookupDelaySeconds,
				CreatedAt:                         now,
				UpdatedAt:                         now,
			}
			// Use a conflict-safe insert, then lock the actual row. The initial
			// existence query above is outside this transaction and never locks a
			// missing unique key.
			insertResult := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "request_id"}}, DoNothing: true}).Create(&candidate)
			if insertResult.Error != nil {
				return insertResult.Error
			}
			if insertResult.RowsAffected == 1 && input.BillingSource == "subscription" {
				if err := refreshOpenRouterCostSubscriptionSnapshotTx(tx, &input, fundingCheckTime); err != nil {
					return err
				}
				candidate.SubscriptionPlanTitle = input.SubscriptionPlanTitle
				candidate.SubscriptionAmountTotal = input.SubscriptionAmountTotal
				candidate.SubscriptionAmountUsedAtAdmission = input.SubscriptionAmountUsedAtAdmission
				candidate.SubscriptionAmountUsedAfter = input.SubscriptionAmountUsedAtAdmission
				candidate.SubscriptionLastResetTime = input.SubscriptionLastResetTime
				candidate.SubscriptionNextResetTime = input.SubscriptionNextResetTime
				candidate.SubscriptionQuotaCycleVersion = input.SubscriptionQuotaCycleVersion
				if err := tx.Save(&candidate).Error; err != nil {
					return err
				}
			}
		}

		var ledger OpenRouterCostLedger
		if err := lockForUpdate(tx).Where("request_id = ?", input.RequestId).First(&ledger).Error; err != nil {
			return err
		}
		if ledger.Status == OpenRouterCostStatusSettled {
			return ErrOpenRouterCostLedgerIdentityChanged
		}
		if ledger.UserId != input.UserId || ledger.TokenId != input.TokenId || ledger.IsPlayground != input.IsPlayground || ledger.BillingSource != input.BillingSource || ledger.SubscriptionId != input.SubscriptionId || ledger.SubscriptionPlanId != input.SubscriptionPlanId || ledger.SubscriptionPlanTitle != input.SubscriptionPlanTitle || ledger.SubscriptionAmountTotal != input.SubscriptionAmountTotal || ledger.SubscriptionAmountUsedAtAdmission != input.SubscriptionAmountUsedAtAdmission || ledger.SubscriptionLastResetTime != input.SubscriptionLastResetTime || ledger.SubscriptionNextResetTime != input.SubscriptionNextResetTime || ledger.SubscriptionQuotaCycleVersion != input.SubscriptionQuotaCycleVersion {
			return ErrOpenRouterCostLedgerIdentityChanged
		}
		ledger.GenerationId = input.GenerationId
		ledger.GenerationEndpoint = input.GenerationEndpoint
		ledger.EndpointFingerprint = input.EndpointFingerprint
		ledger.ChannelKeyFingerprint = input.ChannelKeyFingerprint
		ledger.ChannelKeyIndex = input.ChannelKeyIndex
		ledger.ChannelId = input.ChannelId
		ledger.ModelName = input.ModelName
		ledger.Username = input.Username
		ledger.Group = input.Group
		ledger.NodeName = input.NodeName
		ledger.GroupRatio = input.GroupRatio
		ledger.QuotaPerUnit = input.QuotaPerUnit
		ledger.IsStream = input.IsStream
		ledger.Status = OpenRouterCostStatusPending
		ledger.NextAttemptAt = now + openRouterCostInitialLookupDelaySeconds
		ledger.ClaimedUntil = 0
		ledger.LastError = ""
		ledger.UpdatedAt = now
		return tx.Save(&ledger).Error
	})
}

// RecordOpenRouterGenerationID persists the response header before a stream
// starts relaying. It preserves the initial delay so reconciliation does not
// mistake an active generation's partial cost for its final cost.
func RecordOpenRouterGenerationID(requestId, generationId string) error {
	if requestId == "" || generationId == "" {
		return nil
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var ledger OpenRouterCostLedger
		if err := lockForUpdate(tx).Where("request_id = ?", requestId).First(&ledger).Error; err != nil {
			return err
		}
		if ledger.Status == OpenRouterCostStatusSettled {
			return nil
		}
		if ledger.GenerationId != "" && ledger.GenerationId != generationId {
			return ErrOpenRouterCostLedgerIdentityChanged
		}
		ledger.GenerationId = generationId
		ledger.UpdatedAt = common.GetTimestamp()
		return tx.Save(&ledger).Error
	})
}

func ExtendOpenRouterCostLedgerLease(requestId string, nextAttemptAt int64) error {
	if requestId == "" || nextAttemptAt <= 0 {
		return errors.New("invalid OpenRouter cost lease")
	}
	return DB.Model(&OpenRouterCostLedger{}).
		Where("request_id = ? AND status = ?", requestId, OpenRouterCostStatusPending).
		Updates(map[string]any{"next_attempt_at": nextAttemptAt, "updated_at": common.GetTimestamp()}).Error
}

func UpdateOpenRouterCostLedgerResult(requestId, generationId, costUSD, status, lastError string) error {
	if requestId == "" {
		return errors.New("OpenRouter request id is empty")
	}
	if status != OpenRouterCostStatusPending && status != OpenRouterCostStatusUnresolved {
		return errors.New("invalid OpenRouter cost status")
	}
	now := common.GetTimestamp()
	return DB.Transaction(func(tx *gorm.DB) error {
		var ledger OpenRouterCostLedger
		if err := lockForUpdate(tx).Where("request_id = ?", requestId).First(&ledger).Error; err != nil {
			return err
		}
		if ledger.Status == OpenRouterCostStatusSettled {
			return nil
		}
		if generationId != "" {
			ledger.GenerationId = generationId
		}
		ledger.UpstreamCostUSD = costUSD
		ledger.Status = status
		ledger.LastError = lastError
		ledger.NextAttemptAt = now
		ledger.ClaimedUntil = 0
		ledger.LogUpdatePending = true
		ledger.LogNextAttemptAt = now
		ledger.UpdatedAt = now
		return tx.Save(&ledger).Error
	})
}

func applyOpenRouterCostSubscriptionChargeTx(tx *gorm.DB, ledger *OpenRouterCostLedger, quota, now int64) error {
	if ledger.SubscriptionId <= 0 || ledger.SubscriptionPlanId <= 0 {
		return ErrOpenRouterCostLedgerIdentityChanged
	}
	var sub UserSubscription
	err := lockForUpdate(tx).Where("id = ?", ledger.SubscriptionId).First(&sub).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// Deleting a subscription does not redirect an already incurred charge.
		ledger.SubscriptionCycleApplied = false
		ledger.SubscriptionAmountUsedAfter = 0
		return nil
	}
	if err != nil {
		return err
	}
	if sub.UserId != ledger.UserId || sub.PlanId != ledger.SubscriptionPlanId {
		return ErrOpenRouterCostLedgerIdentityChanged
	}

	plan, planErr := getSubscriptionPlanByIdTx(tx, sub.PlanId)
	if planErr != nil && !errors.Is(planErr, gorm.ErrRecordNotFound) {
		return planErr
	}
	if planErr == nil {
		if err := maybeResetUserSubscriptionWithPlanTx(tx, &sub, plan, now); err != nil {
			return err
		}
	}
	if sub.QuotaCycleVersion != ledger.SubscriptionQuotaCycleVersion || sub.LastResetTime != ledger.SubscriptionLastResetTime || sub.NextResetTime != ledger.SubscriptionNextResetTime {
		// The old request belongs to the prior allowance cycle. Keep its charge
		// on the ledger and log, without consuming the refreshed allowance.
		ledger.SubscriptionCycleApplied = false
		ledger.SubscriptionAmountUsedAfter = 0
		return nil
	}
	if sub.AmountUsed > math.MaxInt64-quota {
		return ErrOpenRouterCostStorageBound
	}
	sub.AmountUsed += quota
	if err := tx.Save(&sub).Error; err != nil {
		return err
	}
	ledger.SubscriptionCycleApplied = true
	ledger.SubscriptionAmountUsedAfter = sub.AmountUsed
	return nil
}

// SettleOpenRouterCostLedger atomically applies the selected funding source,
// token/channel usage, request counters, and idempotent ledger state.
func SettleOpenRouterCostLedger(requestId, costUSD string, quota int64, clamp *common.QuotaClamp) (bool, error) {
	if requestId == "" || quota < 0 || quota > math.MaxInt32 {
		return false, errors.New("invalid OpenRouter settlement")
	}
	now := common.GetTimestamp()
	ledgerBefore, err := GetOpenRouterCostLedger(requestId)
	if err != nil {
		return false, err
	}
	if err := invalidateUserCache(ledgerBefore.UserId); err != nil {
		return false, err
	}
	tokenKey := ""
	if ledgerBefore.Status != OpenRouterCostStatusSettled && !ledgerBefore.IsPlayground {
		// Deleting a credential revokes access, but does not cancel charges
		// already incurred by its owner. Keep ordinary token lookups scoped.
		var token Token
		if err := DB.Unscoped().Where("id = ? AND user_id = ?", ledgerBefore.TokenId, ledgerBefore.UserId).First(&token).Error; err != nil {
			return false, err
		}
		tokenKey = token.Key
		if err := invalidateTokenCacheForMutation(tokenKey); err != nil {
			return false, err
		}
	}
	var userId int
	charged := false
	err = DB.Transaction(func(tx *gorm.DB) error {
		var ledger OpenRouterCostLedger
		if err := lockForUpdate(tx).Where("request_id = ?", requestId).First(&ledger).Error; err != nil {
			return err
		}
		if ledger.Status == OpenRouterCostStatusSettled {
			return nil
		}
		if (ledger.BillingSource != "wallet" && ledger.BillingSource != "subscription") || ledger.UpstreamCostUSD != costUSD {
			return ErrOpenRouterCostLedgerIdentityChanged
		}
		var user User
		if err := lockForUpdate(tx).Where("id = ?", ledger.UserId).First(&user).Error; err != nil {
			return err
		}
		if user.UsedQuota < math.MinInt32 || user.UsedQuota > math.MaxInt32-int(quota) || user.RequestCount == math.MaxInt32 {
			return ErrOpenRouterCostStorageBound
		}
		userUpdates := map[string]any{
			"used_quota":    gorm.Expr("used_quota + ?", quota),
			"request_count": gorm.Expr("request_count + 1"),
		}
		if ledger.BillingSource == "wallet" {
			if int64(user.Quota) < math.MinInt64+quota || common.ValidateWalletQuota(user.Quota) != nil {
				return ErrOpenRouterCostStorageBound
			}
			userUpdates["quota"] = gorm.Expr("quota - ?", quota)
		} else if err := applyOpenRouterCostSubscriptionChargeTx(tx, &ledger, quota, now); err != nil {
			return err
		}
		userUpdate := tx.Model(&User{}).Where("id = ?", ledger.UserId).Updates(userUpdates)
		if userUpdate.Error != nil {
			return userUpdate.Error
		}
		if userUpdate.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		userId = ledger.UserId
		if !ledger.IsPlayground {
			var token Token
			if err := lockForUpdate(tx.Unscoped()).Where("id = ? AND user_id = ?", ledger.TokenId, ledger.UserId).First(&token).Error; err != nil {
				return err
			}
			if token.Key != tokenKey {
				return ErrOpenRouterCostLedgerIdentityChanged
			}
			if token.RemainQuota < math.MinInt32+int(quota) || token.UsedQuota < math.MinInt32 || token.UsedQuota > math.MaxInt32-int(quota) {
				return ErrOpenRouterCostStorageBound
			}
			if quota > 0 {
				tokenUpdate := tx.Unscoped().Model(&Token{}).Where("id = ? AND user_id = ?", ledger.TokenId, ledger.UserId).Updates(map[string]any{
					"remain_quota":  gorm.Expr("remain_quota - ?", quota),
					"used_quota":    gorm.Expr("used_quota + ?", quota),
					"accessed_time": now,
				})
				if tokenUpdate.Error != nil {
					return tokenUpdate.Error
				}
				if tokenUpdate.RowsAffected != 1 {
					return gorm.ErrRecordNotFound
				}
			}
		}
		var channel Channel
		if err := lockForUpdate(tx).Where("id = ?", ledger.ChannelId).First(&channel).Error; err != nil {
			return err
		}
		if channel.UsedQuota > math.MaxInt64-quota {
			return ErrOpenRouterCostStorageBound
		}
		if quota > 0 {
			channelUpdate := tx.Model(&Channel{}).Where("id = ?", ledger.ChannelId).Update("used_quota", gorm.Expr("used_quota + ?", quota))
			if channelUpdate.Error != nil {
				return channelUpdate.Error
			}
			if channelUpdate.RowsAffected != 1 {
				return gorm.ErrRecordNotFound
			}
		}

		ledger.Status = OpenRouterCostStatusSettled
		ledger.Quota = quota
		if common.DataExportEnabled || ledger.QuotaDataId > 0 {
			var payload *OpenRouterCostConsumeLogPayload
			if ledger.LogPayload != "" {
				var savedPayload OpenRouterCostConsumeLogPayload
				if err := common.Unmarshal([]byte(ledger.LogPayload), &savedPayload); err != nil {
					return err
				}
				payload = &savedPayload
			}
			if err := syncOpenRouterCostQuotaData(tx, &ledger, payload); err != nil {
				return err
			}
		}
		if clamp != nil {
			clampJSON, err := common.Marshal(clamp)
			if err != nil {
				return err
			}
			ledger.QuotaSaturation = string(clampJSON)
		}
		ledger.SettledAt = now
		ledger.UpdatedAt = now
		ledger.LogUpdatePending = true
		ledger.LogNextAttemptAt = now
		ledger.LastError = ""
		if err := tx.Save(&ledger).Error; err != nil {
			return err
		}
		charged = true
		return nil
	})
	if err != nil {
		return false, err
	}
	// Keep Redis reads coherent with the committed DB state. Both invalidations
	// are best-effort; a cache failure cannot repeat or roll back the charge.
	if userId > 0 {
		if err := invalidateUserCache(userId); err != nil {
			common.SysLog(common.LogText("failed to invalidate user quota cache after OpenRouter settlement: %s", err.Error()))
		}
	}
	if tokenKey != "" {
		if err := invalidateTokenCacheForMutation(tokenKey); err != nil {
			common.SysLog(common.LogText("failed to invalidate token quota cache after OpenRouter settlement: %s", err.Error()))
		}
	}
	return charged, nil
}

func GetOpenRouterCostLedger(requestId string) (*OpenRouterCostLedger, error) {
	if requestId == "" {
		return nil, errors.New("OpenRouter request id is empty")
	}
	var ledger OpenRouterCostLedger
	if err := DB.Where("request_id = ?", requestId).First(&ledger).Error; err != nil {
		return nil, err
	}
	return &ledger, nil
}

func ListOpenRouterCostLedgersForReconciliation(limit int, now int64, maxAttempts int) ([]OpenRouterCostLedger, error) {
	if limit <= 0 {
		limit = 50
	}
	var ledgers []OpenRouterCostLedger
	err := DB.Where(
		"(status = ? AND generation_id <> '' AND lookup_attempts < ? AND next_attempt_at <= ?) OR (status = ? AND generation_id = '' AND next_attempt_at <= ?) OR (log_update_pending = ? AND log_next_attempt_at <= ?)",
		OpenRouterCostStatusPending, maxAttempts, now, OpenRouterCostStatusPending, now, true, now,
	).Order("created_at asc").Limit(limit).Find(&ledgers).Error
	return ledgers, err
}

func ClaimOpenRouterCostLedger(id int, now, leaseSeconds int64, maxAttempts int) (*OpenRouterCostLedger, bool, error) {
	var claimed OpenRouterCostLedger
	err := DB.Transaction(func(tx *gorm.DB) error {
		var ledger OpenRouterCostLedger
		if err := lockForUpdate(tx).Where("id = ?", id).First(&ledger).Error; err != nil {
			return err
		}
		if ledger.Status == OpenRouterCostStatusSettled || ledger.GenerationId == "" || ledger.LookupAttempts >= maxAttempts || ledger.NextAttemptAt > now || ledger.ClaimedUntil > now {
			return nil
		}
		ledger.ClaimedUntil = now + leaseSeconds
		ledger.UpdatedAt = now
		claimed = ledger
		return tx.Save(&ledger).Error
	})
	if err != nil {
		return nil, false, err
	}
	return &claimed, claimed.Id != 0, nil
}

func RecordOpenRouterCostLookupFailure(id int, failure string, maxAttempts int, retryAfter time.Duration) error {
	now := common.GetTimestamp()
	return DB.Transaction(func(tx *gorm.DB) error {
		var ledger OpenRouterCostLedger
		if err := lockForUpdate(tx).Where("id = ?", id).First(&ledger).Error; err != nil {
			return err
		}
		if ledger.Status == OpenRouterCostStatusSettled {
			return nil
		}
		ledger.LookupAttempts++
		ledger.ClaimedUntil = 0
		ledger.LastError = failure
		ledger.UpdatedAt = now
		ledger.LogUpdatePending = true
		ledger.LogNextAttemptAt = now
		if ledger.LookupAttempts >= maxAttempts {
			ledger.Status = OpenRouterCostStatusUnresolved
			ledger.NextAttemptAt = 0
		} else {
			ledger.Status = OpenRouterCostStatusPending
			ledger.NextAttemptAt = now + int64(retryAfter.Seconds())
		}
		return tx.Save(&ledger).Error
	})
}

func MarkOpenRouterCostLedgerUnresolved(requestId, reason string) error {
	if requestId == "" {
		return errors.New("OpenRouter request id is empty")
	}
	return DB.Model(&OpenRouterCostLedger{}).Where("request_id = ? AND status <> ?", requestId, OpenRouterCostStatusSettled).Updates(map[string]any{
		"status":              OpenRouterCostStatusUnresolved,
		"last_error":          reason,
		"next_attempt_at":     0,
		"claimed_until":       0,
		"log_update_pending":  true,
		"log_next_attempt_at": common.GetTimestamp(),
		"updated_at":          common.GetTimestamp(),
	}).Error
}

func SaveOpenRouterCostLogPayload(requestId string, payload OpenRouterCostConsumeLogPayload) error {
	if requestId == "" {
		return errors.New("OpenRouter request id is empty")
	}
	payload.RequestId = requestId
	data, err := common.Marshal(payload)
	if err != nil {
		return err
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		var ledger OpenRouterCostLedger
		if err := lockForUpdate(tx).Where("request_id = ?", requestId).First(&ledger).Error; err != nil {
			return err
		}
		ledger.LogPayload = string(data)
		ledger.LogUpdatePending = true
		ledger.LogNextAttemptAt = common.GetTimestamp()
		ledger.UpdatedAt = common.GetTimestamp()
		if err := syncOpenRouterCostQuotaData(tx, &ledger, &payload); err != nil {
			return err
		}
		return tx.Save(&ledger).Error
	})
}

// syncOpenRouterCostQuotaData keeps one dashboard row per durable request.
// The ledger row lock makes creation and later quota updates idempotent across
// foreground settlement and reconciliation.
func syncOpenRouterCostQuotaData(tx *gorm.DB, ledger *OpenRouterCostLedger, payload *OpenRouterCostConsumeLogPayload) error {
	if ledger.QuotaDataId == 0 && (!common.DataExportEnabled || !common.LogConsumeEnabled) {
		return nil
	}

	var quotaData QuotaData
	if ledger.QuotaDataId > 0 {
		err := tx.Where("id = ?", ledger.QuotaDataId).First(&quotaData).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			ledger.QuotaDataId = 0
		}
	}

	if ledger.QuotaDataId == 0 {
		requestId := ledger.RequestId
		quotaData = QuotaData{Count: 1, BillingRequestId: &requestId}
		lookupErr := tx.Where("billing_request_id = ?", requestId).First(&quotaData).Error
		if lookupErr != nil && !errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return lookupErr
		}
		if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			quotaData = QuotaData{Count: 1, BillingRequestId: &requestId}
		} else {
			ledger.QuotaDataId = quotaData.Id
		}
	}

	if payload != nil {
		if payload.PromptTokens < 0 || payload.CompletionTokens < 0 || payload.PromptTokens > int(^uint(0)>>1)-payload.CompletionTokens {
			return errors.New("OpenRouter consume token count is outside the dashboard range")
		}
		quotaData.TokenUsed = payload.PromptTokens + payload.CompletionTokens
		var cacheTokens struct {
			Read     int `json:"cache_tokens"`
			Creation int `json:"cache_write_tokens"`
		}
		if payload.Other != "" {
			if err := common.UnmarshalJsonStr(payload.Other, &cacheTokens); err != nil {
				return err
			}
		}
		// These public values are normalized totals, so cache creation breakdowns
		// must not be added again here.
		quotaData.CacheReadTokens = int64(max(0, cacheTokens.Read))
		quotaData.CacheCreationTokens = int64(max(0, cacheTokens.Creation))
		if payload.CreatedAt > 0 {
			quotaData.CreatedAt = payload.CreatedAt
		}
		if payload.Username != "" {
			quotaData.Username = payload.Username
		}
	}
	if quotaData.CreatedAt == 0 {
		quotaData.CreatedAt = ledger.CreatedAt
	}
	quotaData.CreatedAt -= quotaData.CreatedAt % 3600
	if quotaData.Username == "" {
		quotaData.Username = ledger.Username
	}
	quotaData.UserID = ledger.UserId
	quotaData.ModelName = ledger.ModelName
	quotaData.UseGroup = ledger.Group
	quotaData.TokenID = ledger.TokenId
	quotaData.ChannelID = ledger.ChannelId
	quotaData.NodeName = ledger.NodeName
	quotaData.Quota = int(ledger.Quota)
	quotaData.Count = 1
	requestId := ledger.RequestId
	quotaData.BillingRequestId = &requestId

	if ledger.QuotaDataId == 0 {
		if err := tx.Create(&quotaData).Error; err != nil {
			return err
		}
		ledger.QuotaDataId = quotaData.Id
		return nil
	}
	return tx.Model(&QuotaData{}).Where("id = ?", ledger.QuotaDataId).Updates(map[string]any{
		"username":              quotaData.Username,
		"model_name":            quotaData.ModelName,
		"created_at":            quotaData.CreatedAt,
		"use_group":             quotaData.UseGroup,
		"token_id":              quotaData.TokenID,
		"channel_id":            quotaData.ChannelID,
		"node_name":             quotaData.NodeName,
		"billing_request_id":    quotaData.BillingRequestId,
		"count":                 quotaData.Count,
		"quota":                 quotaData.Quota,
		"token_used":            quotaData.TokenUsed,
		"cache_read_tokens":     quotaData.CacheReadTokens,
		"cache_creation_tokens": quotaData.CacheCreationTokens,
	}).Error
}

// SyncOpenRouterCostConsumeLog holds the durable ledger lock through the log
// projection. For a separate log DB, replaying after a crash updates by
// request ID again; for a shared DB, both writes commit atomically.
func SyncOpenRouterCostConsumeLog(ctx context.Context, requestId string, force ...bool) error {
	if requestId == "" {
		return errors.New("OpenRouter request id is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	forceNow := len(force) > 0 && force[0]
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			// GORM's SQLite lock helper intentionally emits no FOR UPDATE. Take
			// the write lock before reading the ledger or mutating a separate log
			// DB so two deferred transactions cannot both project stale snapshots.
			if err := tx.Model(&OpenRouterCostLedger{}).
				Where("request_id = ?", requestId).
				Update("updated_at", gorm.Expr("updated_at")).Error; err != nil {
				return err
			}
		}
		var ledger OpenRouterCostLedger
		if err := lockForUpdate(tx).Where("request_id = ?", requestId).First(&ledger).Error; err != nil {
			return err
		}
		if !ledger.LogUpdatePending {
			return nil
		}
		if !forceNow && ledger.LogNextAttemptAt > common.GetTimestamp() {
			return nil
		}
		if !common.LogConsumeEnabled {
			ledger.LogUpdatePending = false
			ledger.LogNextAttemptAt = 0
			ledger.UpdatedAt = common.GetTimestamp()
			return tx.Save(&ledger).Error
		}

		payload := OpenRouterCostConsumeLogPayload{
			UserId:    ledger.UserId,
			CreatedAt: ledger.CreatedAt,
			Type:      LogTypeConsume,
			ModelName: ledger.ModelName,
			Quota:     int(ledger.Quota),
			IsStream:  ledger.IsStream,
			ChannelId: ledger.ChannelId,
			TokenId:   ledger.TokenId,
			Group:     ledger.Group,
			RequestId: ledger.RequestId,
		}
		var quotaPayload *OpenRouterCostConsumeLogPayload
		if ledger.LogPayload != "" {
			if err := common.Unmarshal([]byte(ledger.LogPayload), &payload); err != nil {
				return err
			}
			quotaPayload = &payload
		}
		payload.RequestId = ledger.RequestId
		payload.Type = LogTypeConsume
		payload.UserId = ledger.UserId
		payload.CreatedAt = ledger.CreatedAt
		payload.ModelName = ledger.ModelName
		payload.ChannelId = ledger.ChannelId
		payload.TokenId = ledger.TokenId
		payload.Group = ledger.Group
		payload.IsStream = ledger.IsStream
		payload.Quota = int(ledger.Quota)
		if common.DataExportEnabled || ledger.QuotaDataId > 0 {
			if err := syncOpenRouterCostQuotaData(tx, &ledger, quotaPayload); err != nil {
				return err
			}
		}

		other := make(map[string]any)
		if payload.Other != "" {
			if err := common.UnmarshalJsonStr(payload.Other, &other); err != nil {
				return err
			}
		}
		other["billing_mode"] = "openrouter_cost"
		other["billing_status"] = ledger.Status
		billingSource := ledger.BillingSource
		if billingSource == "" {
			billingSource = "wallet"
		}
		other["billing_source"] = billingSource
		if billingSource == "subscription" {
			if ledger.SubscriptionId > 0 {
				other["subscription_id"] = ledger.SubscriptionId
			}
			if ledger.SubscriptionPlanId > 0 {
				other["subscription_plan_id"] = ledger.SubscriptionPlanId
			}
			if ledger.SubscriptionPlanTitle != "" {
				other["subscription_plan_title"] = ledger.SubscriptionPlanTitle
			}
			other["wallet_quota_deducted"] = 0
			if ledger.Status == OpenRouterCostStatusSettled {
				other["subscription_consumed"] = ledger.Quota
				if ledger.SubscriptionAmountTotal > 0 {
					other["subscription_total"] = ledger.SubscriptionAmountTotal
					if ledger.SubscriptionCycleApplied {
						used := max(ledger.SubscriptionAmountUsedAfter, int64(0))
						other["subscription_used"] = used
						other["subscription_remain"] = max(ledger.SubscriptionAmountTotal-used, int64(0))
					} else {
						delete(other, "subscription_used")
						delete(other, "subscription_remain")
					}
				}
			} else {
				delete(other, "subscription_consumed")
				delete(other, "subscription_used")
				delete(other, "subscription_remain")
			}
		} else {
			for _, key := range []string{"subscription_id", "subscription_plan_id", "subscription_plan_title", "subscription_pre_consumed", "subscription_post_delta", "subscription_consumed", "subscription_remain", "subscription_total", "subscription_used", "wallet_quota_deducted"} {
				delete(other, key)
			}
		}
		// The generic text metadata includes public pricing ratios for local
		// billing. This mode exposes only the frozen administrator-only ratio.
		delete(other, "group_ratio")
		delete(other, "user_group_ratio")
		adminInfo, ok := other[logOtherAdminInfoKey].(map[string]any)
		if !ok {
			adminInfo = make(map[string]any)
		}
		if ledger.UpstreamCostUSD != "" {
			adminInfo["upstream_cost_usd"] = ledger.UpstreamCostUSD
		}
		if groupRatio, err := decimal.NewFromString(ledger.GroupRatio); err == nil {
			adminInfo["group_ratio"] = groupRatio.InexactFloat64()
		}
		if ledger.QuotaSaturation != "" {
			var saturation map[string]any
			if err := common.Unmarshal([]byte(ledger.QuotaSaturation), &saturation); err == nil {
				adminInfo["quota_saturation"] = saturation
			}
		}
		if ledger.LastError != "" {
			adminInfo["billing_reconciliation_error"] = ledger.LastError
		}
		if len(adminInfo) > 0 {
			other[logOtherAdminInfoKey] = adminInfo
		}
		otherJSON, err := common.Marshal(other)
		if err != nil {
			return err
		}
		payload.Other = string(otherJSON)

		logDB := LOG_DB.WithContext(ctx)
		if LOG_DB == DB {
			logDB = tx
		} else if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
			logDB = logDB.WithContext(clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"mutations_sync": 1})))
		}
		var existing Log
		err = logDB.Where("request_id = ? AND type = ?", ledger.RequestId, LogTypeConsume).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			err = logDB.Create(&Log{
				UserId:            payload.UserId,
				Username:          payload.Username,
				CreatedAt:         payload.CreatedAt,
				Type:              LogTypeConsume,
				Content:           payload.Content,
				ModelName:         payload.ModelName,
				TokenName:         payload.TokenName,
				Quota:             payload.Quota,
				PromptTokens:      payload.PromptTokens,
				CompletionTokens:  payload.CompletionTokens,
				UseTime:           payload.UseTime,
				IsStream:          payload.IsStream,
				ChannelId:         payload.ChannelId,
				TokenId:           payload.TokenId,
				Group:             payload.Group,
				Ip:                payload.Ip,
				RequestId:         payload.RequestId,
				UpstreamRequestId: payload.UpstreamRequestId,
				Other:             payload.Other,
			}).Error
		} else if err != nil {
			return err
		} else {
			err = logDB.Model(&Log{}).Where("request_id = ? AND type = ?", ledger.RequestId, LogTypeConsume).Updates(map[string]any{
				"content":             payload.Content,
				"model_name":          payload.ModelName,
				"token_name":          payload.TokenName,
				"quota":               payload.Quota,
				"prompt_tokens":       payload.PromptTokens,
				"completion_tokens":   payload.CompletionTokens,
				"use_time":            payload.UseTime,
				"is_stream":           payload.IsStream,
				"channel_id":          payload.ChannelId,
				"token_id":            payload.TokenId,
				"group":               payload.Group,
				"ip":                  payload.Ip,
				"upstream_request_id": payload.UpstreamRequestId,
				"other":               payload.Other,
			}).Error
		}
		if err != nil {
			return err
		}
		ledger.LogUpdatePending = false
		ledger.LogNextAttemptAt = 0
		ledger.UpdatedAt = common.GetTimestamp()
		return tx.Save(&ledger).Error
	})
	if err != nil {
		backoffCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = DB.WithContext(backoffCtx).Model(&OpenRouterCostLedger{}).
			Where("request_id = ? AND log_update_pending = ?", requestId, true).
			Updates(map[string]any{"log_next_attempt_at": common.GetTimestamp() + 60, "updated_at": common.GetTimestamp()}).Error
		return err
	}
	return nil
}
