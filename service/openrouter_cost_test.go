package service

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

type openRouterCostDatabaseOptions struct {
	deferredSQLite bool
	plainTables    bool
}

func openRouterCostDatabase(t *testing.T, dialect string, options ...openRouterCostDatabaseOptions) (*gin.Context, *relaycommon.RelayInfo) {
	t.Helper()
	var opts openRouterCostDatabaseOptions
	if len(options) > 0 {
		opts = options[0]
	}
	var driver gorm.Dialector
	dbType := common.DatabaseTypeSQLite
	switch dialect {
	case "sqlite":
		path := filepath.Join(t.TempDir(), "cost.sqlite")
		if !opts.deferredSQLite {
			path += "?_pragma=busy_timeout(30000)&_txlock=immediate"
		}
		driver = sqlite.Open(path)
	case "mysql":
		dsn := os.Getenv("TEST_MYSQL_DSN")
		if dsn == "" {
			t.Skip("TEST_MYSQL_DSN is not configured")
		}
		driver, dbType = mysql.Open(dsn), common.DatabaseTypeMySQL
	case "postgres":
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is not configured")
		}
		driver, dbType = postgres.Open(dsn), common.DatabaseTypePostgreSQL
	default:
		t.Fatalf("unknown database %s", dialect)
	}
	prefix := "orcost_"
	if opts.plainTables {
		prefix = ""
	}
	db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: prefix}, Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	if dialect == "sqlite" && (!opts.deferredSQLite) {
		sqlDB.SetMaxOpenConns(1)
	}
	models := []any{&model.OpenRouterCostLedger{}, &model.QuotaData{}, &model.Log{}, &model.Token{}, &model.Channel{}, &model.SubscriptionPlan{}, &model.UserSubscription{}, &model.SubscriptionPreConsumeRecord{}, &model.User{}}
	require.NoError(t, db.Migrator().DropTable(models...))
	for range 2 {
		require.NoError(t, db.AutoMigrate(models...))
	}
	originalDB, originalLog := model.DB, model.LOG_DB
	originalMainType, originalLogType := common.MainDatabaseType(), common.LogDatabaseType()
	originalRedis, originalBatch, originalLogging := common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled
	originalExport := common.DataExportEnabled
	model.DB, model.LOG_DB = db, db
	model.InvalidateSubscriptionPlanCache(1)
	common.SetDatabaseTypes(dbType, dbType)
	common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled = false, false, true
	common.DataExportEnabled = true
	t.Cleanup(func() {
		assert.NoError(t, db.Migrator().DropTable(models...))
		assert.NoError(t, sqlDB.Close())
		model.DB, model.LOG_DB = originalDB, originalLog
		model.InvalidateSubscriptionPlanCache(1)
		common.SetDatabaseTypes(originalMainType, originalLogType)
		common.RedisEnabled, common.BatchUpdateEnabled, common.LogConsumeEnabled = originalRedis, originalBatch, originalLogging
		common.DataExportEnabled = originalExport
	})
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "cost-user", AffCode: "cost-user", Quota: 1000, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, db.Create(&model.Token{Id: 1, UserId: 1, Key: "cost-token-fixture", Name: "cost-token", RemainQuota: 1000, Status: common.TokenStatusEnabled}).Error)
	channel := &model.Channel{Id: 1, Type: constant.ChannelTypeOpenRouter, Name: "cost-channel", Key: "upstream-cost-fixture", BaseURL: common.GetPointer("https://openrouter.example/api"), Status: common.ChannelStatusEnabled}
	channel.SetOtherSettings(dto.ChannelOtherSettings{OpenRouterCostBilling: true})
	require.NoError(t, db.Create(channel).Error)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(common.RequestIdKey, "cost-request")
	c.Set("username", "cost-user")
	c.Set(string(constant.ContextKeyChannelId), channel.Id)
	c.Set(string(constant.ContextKeyChannelType), channel.Type)
	c.Set(string(constant.ContextKeyChannelKey), channel.Key)
	c.Set(string(constant.ContextKeyChannelBaseUrl), *channel.BaseURL)
	c.Set(string(constant.ContextKeyChannelOtherSetting), channel.GetOtherSettings())
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 1, ChannelType: constant.ChannelTypeOpenRouter, ApiKey: channel.Key, ChannelBaseUrl: *channel.BaseURL, ChannelOtherSettings: channel.GetOtherSettings()},
		UserId:      1, TokenId: 1, TokenKey: "cost-token-fixture", OriginModelName: "unconfigured-cost-model", UsingGroup: "default", RequestId: "cost-request", OpenRouterCostBilling: true,
		UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}, StartTime: time.Now(),
		PriceData: hosttypes.PriceData{GroupRatioInfo: hosttypes.GroupRatioInfo{GroupRatio: 2}},
	}
	t.Cleanup(func() { stopOpenRouterCostHeartbeat(info) })
	return c, info
}

func seedOpenRouterCostSubscription(t *testing.T, id, planId int, amountTotal, amountUsed int64, allowWalletOverflow bool, resetPeriod string, nextResetTime int64) (*model.SubscriptionPlan, *model.UserSubscription) {
	t.Helper()
	plan := &model.SubscriptionPlan{
		Id:                      planId,
		Title:                   "OpenRouter cost plan",
		Enabled:                 true,
		TotalAmount:             amountTotal,
		QuotaResetPeriod:        resetPeriod,
		QuotaResetCustomSeconds: 86400,
	}
	require.NoError(t, model.DB.Create(plan).Error)
	model.InvalidateSubscriptionPlanCache(planId)
	sub := &model.UserSubscription{
		Id:                  id,
		UserId:              1,
		PlanId:              planId,
		AmountTotal:         amountTotal,
		AmountUsed:          amountUsed,
		StartTime:           time.Now().Add(-time.Hour).Unix(),
		EndTime:             time.Now().Add(time.Hour).Unix(),
		Status:              "active",
		AllowWalletOverflow: allowWalletOverflow,
		LastResetTime:       time.Now().Add(-time.Hour).Unix(),
		NextResetTime:       nextResetTime,
	}
	require.NoError(t, model.DB.Create(sub).Error)
	return plan, sub
}

func setOpenRouterCostSubscriptionDowngrade(t *testing.T, subId int, fromGroup, toGroup string) {
	t.Helper()
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 1).Update("group", fromGroup).Error)
	require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", subId).Updates(map[string]any{
		"upgrade_group":   fromGroup,
		"prev_user_group": toGroup,
		"downgrade_group": toGroup,
	}).Error)
}

func TestOpenRouterCostProjectionWithDeferredSQLiteAndSeparateLogDB(t *testing.T) {
	c, info := openRouterCostDatabase(t, "sqlite", openRouterCostDatabaseOptions{deferredSQLite: true})
	prepareOpenRouterCostRequest(t, c, info)
	stopOpenRouterCostHeartbeat(info)
	logDB, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "logs.sqlite")), &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "orcost_"}, Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	require.NoError(t, logDB.AutoMigrate(&model.Log{}))
	logSQL, err := logDB.DB()
	require.NoError(t, err)
	primaryLogDB := model.LOG_DB
	model.LOG_DB = logDB
	t.Cleanup(func() { model.LOG_DB = primaryLogDB; assert.NoError(t, logSQL.Close()) })
	require.NoError(t, model.UpdateOpenRouterCostLedgerResult(info.RequestId, "gen-cost-fixture", "", model.OpenRouterCostStatusPending, ""))
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			results <- model.SyncOpenRouterCostConsumeLog(ctx, info.RequestId)
		})
	}
	workers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	info.OpenRouterCostFinal = true
	info.OpenRouterGenerationID = "gen-cost-fixture"
	PostTextConsumeQuota(c, info, &dto.Usage{PromptTokens: 14, CompletionTokens: 2, TotalTokens: 16, Cost: json.Number("0.002")}, nil)
	var logs []model.Log
	require.NoError(t, logDB.Where("request_id = ?", info.RequestId).Find(&logs).Error)
	require.Len(t, logs, 1)
	assert.Equal(t, 2000, logs[0].Quota)
	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
	assert.Equal(t, "settled", other["billing_status"])
}

func prepareOpenRouterCostRequest(t *testing.T, c *gin.Context, info *relaycommon.RelayInfo) {
	t.Helper()
	require.Nil(t, PrepareOpenRouterCostBilling(c, info))
	require.Nil(t, PrepareTieredBillingForSelectedGroup(c, info))
}

func prepareOpenRouterCostSubscriptionRequest(t *testing.T, c *gin.Context, info *relaycommon.RelayInfo, requestId string) {
	t.Helper()
	info.RequestId = requestId
	info.UserSetting.BillingPreference = "subscription_only"
	info.OpenRouterGenerationID = ""
	info.OpenRouterCostFinal = false
	info.OpenRouterGenerationRecorder = nil
	info.BillingSource = ""
	info.SubscriptionId = 0
	info.SubscriptionPlanId = 0
	info.SubscriptionPlanTitle = ""
	info.SubscriptionAmountTotal = 0
	info.SubscriptionAmountUsedAfterPreConsume = 0
	info.OpenRouterCostSubscriptionLastResetTime = 0
	info.OpenRouterCostSubscriptionNextResetTime = 0
	info.OpenRouterCostSubscriptionCycleVersion = 0
	prepareOpenRouterCostRequest(t, c, info)
	stopOpenRouterCostHeartbeat(info)
}

func TestOpenRouterCostFundingPreferenceDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			_, _ = openRouterCostDatabase(t, dialect)
			plan, sub := seedOpenRouterCostSubscription(t, 1, 1, 1000, 0, false, model.SubscriptionResetNever, 0)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 1).Update("quota", 0).Error)

			selected, err := model.SelectOpenRouterCostFunding(1, "subscription_only")
			require.NoError(t, err)
			assert.Equal(t, "subscription", selected.BillingSource, "subscription-only must work without wallet balance")
			assert.Equal(t, sub.Id, selected.SubscriptionId)

			selected, err = model.SelectOpenRouterCostFunding(1, "subscription_first")
			require.NoError(t, err)
			assert.Equal(t, "subscription", selected.BillingSource)

			selected, err = model.SelectOpenRouterCostFunding(1, "wallet_first")
			require.NoError(t, err)
			assert.Equal(t, "subscription", selected.BillingSource, "wallet-first falls back only after wallet is not spendable")

			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 1).Update("quota", 1000).Error)
			selected, err = model.SelectOpenRouterCostFunding(1, "wallet_first")
			require.NoError(t, err)
			assert.Equal(t, "wallet", selected.BillingSource, "wallet-first prefers spendable wallet quota")
			selected, err = model.SelectOpenRouterCostFunding(1, "wallet_only")
			require.NoError(t, err)
			assert.Equal(t, "wallet", selected.BillingSource, "wallet-only ignores an available subscription")

			// An empty or unknown preference normalizes to subscription-first.
			selected, err = model.SelectOpenRouterCostFunding(1, "")
			require.NoError(t, err)
			assert.Equal(t, "subscription", selected.BillingSource)
			selected, err = model.SelectOpenRouterCostFunding(1, "invalid")
			require.NoError(t, err)
			assert.Equal(t, "subscription", selected.BillingSource)

			// Exhaustion falls back only when every active subscription permits it.
			require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", sub.Id).Updates(map[string]any{"amount_used": 1000, "allow_wallet_overflow": true}).Error)
			selected, err = model.SelectOpenRouterCostFunding(1, "subscription_first")
			require.NoError(t, err)
			assert.Equal(t, "wallet", selected.BillingSource)
			require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", sub.Id).Update("allow_wallet_overflow", false).Error)
			_, err = model.SelectOpenRouterCostFunding(1, "subscription_first")
			require.ErrorIs(t, err, model.ErrOpenRouterCostSubscriptionUnavailable, "strict exhausted subscriptions block wallet fallback")
			_, err = model.SelectOpenRouterCostFunding(1, "subscription_only")
			require.ErrorIs(t, err, model.ErrOpenRouterCostSubscriptionUnavailable)

			// No active subscription permits the usual subscription-first wallet path.
			require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", sub.Id).Update("status", "expired").Error)
			selected, err = model.SelectOpenRouterCostFunding(1, "subscription_first")
			require.NoError(t, err)
			assert.Equal(t, "wallet", selected.BillingSource)

			// Zero total means unlimited, even when AmountUsed is nonzero.
			require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", sub.Id).Updates(map[string]any{"status": "active", "amount_total": 0, "amount_used": 9000}).Error)
			selected, err = model.SelectOpenRouterCostFunding(1, "subscription_only")
			require.NoError(t, err)
			assert.Equal(t, "subscription", selected.BillingSource)
			assert.Zero(t, selected.SubscriptionAmountTotal)

			// A due automatic reset occurs before allowance selection and advances
			// the durable cycle marker.
			require.NoError(t, model.DB.Model(&plan).Updates(map[string]any{"quota_reset_period": model.SubscriptionResetDaily}).Error)
			model.InvalidateSubscriptionPlanCache(plan.Id)
			now := model.GetDBTimestamp()
			require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", sub.Id).Updates(map[string]any{
				"amount_total":    1000,
				"amount_used":     1000,
				"last_reset_time": now - 48*3600,
				"next_reset_time": now - 24*3600,
			}).Error)
			selected, err = model.SelectOpenRouterCostFunding(1, "subscription_only")
			require.NoError(t, err)
			assert.Equal(t, "subscription", selected.BillingSource)
			assert.Zero(t, selected.SubscriptionAmountUsedAtAdmission)
			assert.Greater(t, selected.SubscriptionQuotaCycleVersion, int64(0))
		})
	}
}

func TestOpenRouterCostConcurrentAdmissionDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			c, info := openRouterCostDatabase(t, dialect)
			_, _ = seedOpenRouterCostSubscription(t, 1, 1, 10000, 0, false, model.SubscriptionResetNever, 0)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", info.UserId).Update("quota", 0).Error)
			info.RequestId = "cost-concurrent-admission"
			info.UserSetting.BillingPreference = "subscription_only"
			first, second := *info, *info
			t.Cleanup(func() {
				stopOpenRouterCostHeartbeat(&first)
				stopOpenRouterCostHeartbeat(&second)
			})
			start := make(chan struct{})
			results := make(chan error, 2)
			var workers sync.WaitGroup
			for _, attempt := range []*relaycommon.RelayInfo{&first, &second} {
				workers.Go(func() {
					<-start
					if apiErr := PrepareOpenRouterCostBilling(c, attempt); apiErr != nil {
						results <- apiErr
						return
					}
					if apiErr := PrepareTieredBillingForSelectedGroup(c, attempt); apiErr != nil {
						results <- apiErr
						return
					}
					results <- nil
				})
			}
			close(start)
			workers.Wait()
			close(results)
			for err := range results {
				require.NoError(t, err)
			}
			var ledgers []model.OpenRouterCostLedger
			require.NoError(t, model.DB.Where("request_id = ?", info.RequestId).Find(&ledgers).Error)
			require.Len(t, ledgers, 1)
			assert.Equal(t, "subscription", ledgers[0].BillingSource)
			assert.Equal(t, 1, ledgers[0].SubscriptionId)
			var user model.User
			require.NoError(t, model.DB.First(&user, info.UserId).Error)
			assert.Zero(t, user.Quota, "duplicate admission must not require or reserve wallet quota")
		})
	}
}

func TestOpenRouterCostSubscriptionSettlementDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			c, info := openRouterCostDatabase(t, dialect)
			_, sub := seedOpenRouterCostSubscription(t, 1, 1, 1000, 500, false, model.SubscriptionResetNever, 0)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", info.UserId).Update("quota", 0).Error)
			info.UserSetting.BillingPreference = "subscription_only"
			prepareOpenRouterCostRequest(t, c, info)
			stopOpenRouterCostHeartbeat(info)

			ledger, err := model.GetOpenRouterCostLedger(info.RequestId)
			require.NoError(t, err)
			assert.Equal(t, "subscription", ledger.BillingSource)
			assert.Equal(t, sub.Id, ledger.SubscriptionId)
			assert.Equal(t, sub.PlanId, ledger.SubscriptionPlanId)
			assert.Equal(t, int64(1000), ledger.SubscriptionAmountTotal)
			assert.Equal(t, int64(500), ledger.SubscriptionAmountUsedAtAdmission)
			var user model.User
			require.NoError(t, model.DB.First(&user, info.UserId).Error)
			assert.Zero(t, user.Quota, "subscription admission must not require or reserve wallet quota")
			var token model.Token
			require.NoError(t, model.DB.First(&token, info.TokenId).Error)
			assert.Equal(t, 1000, token.RemainQuota, "admission does not reserve token quota")

			// A retry restores the source and instance snapshot from the ledger even
			// after the billing preference changes.
			info.UserSetting.BillingPreference = "wallet_only"
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", info.UserId).Update("quota", 5000).Error)
			require.Nil(t, PrepareOpenRouterCostBilling(c, info))
			assert.Equal(t, "subscription", info.BillingSource)
			assert.Equal(t, sub.Id, info.SubscriptionId)

			input, err := openRouterCostLedgerInput(c, info, info.ChannelMeta)
			require.NoError(t, err)
			input.SubscriptionId++
			require.ErrorIs(t, model.EnsureOpenRouterCostLedger(input), model.ErrOpenRouterCostLedgerIdentityChanged)

			require.NoError(t, model.UpdateOpenRouterCostLedgerResult(info.RequestId, "gen-sub-cost", "0.002", model.OpenRouterCostStatusPending, ""))
			type result struct {
				charged bool
				err     error
			}
			results := make(chan result, 2)
			var workers sync.WaitGroup
			for range 2 {
				workers.Go(func() {
					charged, err := model.SettleOpenRouterCostLedger(info.RequestId, "0.002", 2000, nil)
					results <- result{charged, err}
				})
			}
			workers.Wait()
			close(results)
			charges := 0
			for result := range results {
				require.NoError(t, result.err)
				if result.charged {
					charges++
				}
			}
			assert.Equal(t, 1, charges, "concurrent reconciliation applies one subscription charge")
			var settledSub model.UserSubscription
			require.NoError(t, model.DB.First(&settledSub, sub.Id).Error)
			assert.EqualValues(t, 2500, settledSub.AmountUsed, "postpaid overage is fully charged to the original subscription")
			var settledUser model.User
			require.NoError(t, model.DB.First(&settledUser, info.UserId).Error)
			assert.Equal(t, 5000, settledUser.Quota, "subscription overage never debits wallet quota")
			assert.Equal(t, 2000, settledUser.UsedQuota)
			assert.Equal(t, 1, settledUser.RequestCount)
			var settledToken model.Token
			require.NoError(t, model.DB.First(&settledToken, info.TokenId).Error)
			assert.Equal(t, -1000, settledToken.RemainQuota)
			assert.Equal(t, 2000, settledToken.UsedQuota)
			ledger, err = model.GetOpenRouterCostLedger(info.RequestId)
			require.NoError(t, err)
			assert.Equal(t, int64(2000), ledger.Quota)
			assert.True(t, ledger.SubscriptionCycleApplied)
			assert.Equal(t, int64(2500), ledger.SubscriptionAmountUsedAfter)

			require.NoError(t, model.SyncOpenRouterCostConsumeLog(context.Background(), info.RequestId, true))
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("request_id = ? AND type = ?", info.RequestId, model.LogTypeConsume).Find(&logs).Error)
			require.Len(t, logs, 1)
			var other map[string]any
			require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
			assert.Equal(t, "subscription", other["billing_source"])
			assert.EqualValues(t, sub.Id, other["subscription_id"])
			assert.EqualValues(t, sub.PlanId, other["subscription_plan_id"])
			assert.EqualValues(t, 2000, other["subscription_consumed"])
			assert.EqualValues(t, 0, other["wallet_quota_deducted"])

			// Strict exhausted subscriptions keep blocking wallet fallback after
			// an admitted request settles above the plan allowance.
			info.RequestId = "cost-after-sub-overage"
			info.UserSetting.BillingPreference = "subscription_first"
			assert.NotNil(t, PrepareOpenRouterCostBilling(c, info))
			var ledgerCount int64
			require.NoError(t, model.DB.Model(&model.OpenRouterCostLedger{}).Count(&ledgerCount).Error)
			assert.EqualValues(t, 1, ledgerCount)
		})
	}
}

func TestOpenRouterCostSubscriptionCycleAndLifecycleDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			c, info := openRouterCostDatabase(t, dialect)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", info.UserId).Update("quota", 0).Error)

			// A reset between admission and settlement advances the cycle marker;
			// the old request remains billed but cannot use the refreshed quota.
			resetAt := model.GetDBTimestamp()
			_, resetSub := seedOpenRouterCostSubscription(t, 1, 1, 5000, 100, false, model.SubscriptionResetDaily, resetAt+3600)
			require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", resetSub.Id).Update("last_reset_time", resetAt-48*3600).Error)
			prepareOpenRouterCostSubscriptionRequest(t, c, info, "cost-reset-crossing")
			resetLedger, err := model.GetOpenRouterCostLedger(info.RequestId)
			require.NoError(t, err)
			assert.Zero(t, resetLedger.SubscriptionQuotaCycleVersion)
			require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", resetSub.Id).Update("next_reset_time", resetAt-1).Error)
			require.NoError(t, model.UpdateOpenRouterCostLedgerResult(info.RequestId, "gen-reset-crossing", "0.002", model.OpenRouterCostStatusPending, ""))
			charged, err := model.SettleOpenRouterCostLedger(info.RequestId, "0.002", 2000, nil)
			require.NoError(t, err)
			assert.True(t, charged)
			var resetSubAfter model.UserSubscription
			require.NoError(t, model.DB.First(&resetSubAfter, resetSub.Id).Error)
			assert.Zero(t, resetSubAfter.AmountUsed, "settlement must not consume the refreshed cycle")
			assert.Greater(t, resetSubAfter.QuotaCycleVersion, resetLedger.SubscriptionQuotaCycleVersion)
			resetLedger, err = model.GetOpenRouterCostLedger(info.RequestId)
			require.NoError(t, err)
			assert.Equal(t, int64(2000), resetLedger.Quota, "the incurred charge remains in the durable ledger")
			assert.False(t, resetLedger.SubscriptionCycleApplied)
			require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", resetSub.Id).Update("status", "expired").Error)

			// Admin invalidation after admission does not cancel an incurred
			// charge while the selected allowance cycle still exists.
			_, expiredSub := seedOpenRouterCostSubscription(t, 2, 2, 5000, 100, false, model.SubscriptionResetNever, 0)
			require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", info.TokenId).Update("remain_quota", 10000).Error)
			prepareOpenRouterCostSubscriptionRequest(t, c, info, "cost-expired-after-admission")
			setOpenRouterCostSubscriptionDowngrade(t, expiredSub.Id, "premium", "basic")
			message, err := model.AdminInvalidateUserSubscription(expiredSub.Id)
			require.NoError(t, err)
			assert.NotNil(t, message)
			var downgradedUser model.User
			require.NoError(t, model.DB.First(&downgradedUser, info.UserId).Error)
			assert.Equal(t, "basic", downgradedUser.Group)
			require.NoError(t, model.UpdateOpenRouterCostLedgerResult(info.RequestId, "gen-expired-after-admission", "0.002", model.OpenRouterCostStatusPending, ""))
			charged, err = model.SettleOpenRouterCostLedger(info.RequestId, "0.002", 2000, nil)
			require.NoError(t, err)
			assert.True(t, charged)
			var expiredSubAfter model.UserSubscription
			require.NoError(t, model.DB.First(&expiredSubAfter, expiredSub.Id).Error)
			assert.Equal(t, "cancelled", expiredSubAfter.Status)
			assert.EqualValues(t, 2100, expiredSubAfter.AmountUsed)

			// Deletion preserves the charge in ledger, logs and counters without
			// redirecting it to the wallet or another subscription.
			_, deletedSub := seedOpenRouterCostSubscription(t, 3, 3, 5000, 100, false, model.SubscriptionResetNever, 0)
			require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", info.TokenId).Update("remain_quota", 10000).Error)
			prepareOpenRouterCostSubscriptionRequest(t, c, info, "cost-deleted-subscription")
			setOpenRouterCostSubscriptionDowngrade(t, deletedSub.Id, "premium", "basic")
			message, err = model.AdminDeleteUserSubscription(deletedSub.Id)
			require.NoError(t, err)
			assert.NotNil(t, message)
			var missingSub model.UserSubscription
			require.ErrorIs(t, model.DB.First(&missingSub, deletedSub.Id).Error, gorm.ErrRecordNotFound)
			require.NoError(t, model.DB.First(&downgradedUser, info.UserId).Error)
			assert.Equal(t, "basic", downgradedUser.Group)
			require.NoError(t, model.UpdateOpenRouterCostLedgerResult(info.RequestId, "gen-deleted-subscription", "0.002", model.OpenRouterCostStatusPending, ""))
			charged, err = model.SettleOpenRouterCostLedger(info.RequestId, "0.002", 2000, nil)
			require.NoError(t, err)
			assert.True(t, charged)
			deletedLedger, err := model.GetOpenRouterCostLedger(info.RequestId)
			require.NoError(t, err)
			assert.Equal(t, model.OpenRouterCostStatusSettled, deletedLedger.Status)
			assert.Equal(t, int64(2000), deletedLedger.Quota)
			assert.Equal(t, "subscription", deletedLedger.BillingSource)
			var wallet model.User
			require.NoError(t, model.DB.First(&wallet, info.UserId).Error)
			assert.Zero(t, wallet.Quota)

			// Administrative resets are versioned even when they occur in the same
			// Unix second as admission.
			_, adminResetSub := seedOpenRouterCostSubscription(t, 4, 4, 5000, 100, false, model.SubscriptionResetNever, 0)
			require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", info.TokenId).Update("remain_quota", 10000).Error)
			prepareOpenRouterCostSubscriptionRequest(t, c, info, "cost-admin-reset-same-second")
			adminLedger, err := model.GetOpenRouterCostLedger(info.RequestId)
			require.NoError(t, err)
			_, err = model.AdminResetUserSubscriptionsByPlan(info.UserId, adminResetSub.PlanId, false)
			require.NoError(t, err)
			var adminResetAfter model.UserSubscription
			require.NoError(t, model.DB.First(&adminResetAfter, adminResetSub.Id).Error)
			assert.Greater(t, adminResetAfter.QuotaCycleVersion, adminLedger.SubscriptionQuotaCycleVersion)
			require.NoError(t, model.UpdateOpenRouterCostLedgerResult(info.RequestId, "gen-admin-reset", "0.002", model.OpenRouterCostStatusPending, ""))
			charged, err = model.SettleOpenRouterCostLedger(info.RequestId, "0.002", 2000, nil)
			require.NoError(t, err)
			assert.True(t, charged)
			require.NoError(t, model.DB.First(&adminResetAfter, adminResetSub.Id).Error)
			assert.Zero(t, adminResetAfter.AmountUsed)
			adminLedger, err = model.GetOpenRouterCostLedger(info.RequestId)
			require.NoError(t, err)
			assert.False(t, adminLedger.SubscriptionCycleApplied)
			require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", adminResetSub.Id).Update("status", "expired").Error)

			// Recovery after restart reads source and plan identity from the ledger.
			_, recoverySub := seedOpenRouterCostSubscription(t, 5, 5, 5000, 0, false, model.SubscriptionResetNever, 0)
			require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", info.TokenId).Update("remain_quota", 10000).Error)
			prepareOpenRouterCostSubscriptionRequest(t, c, info, "cost-subscription-recovery")
			info.OpenRouterGenerationID = "gen-subscription-recovery"
			require.NoError(t, info.OpenRouterGenerationRecorder(info.OpenRouterGenerationID))
			FinalizeOpenRouterCostFailure(info)
			originalTransport := http.DefaultTransport
			http.DefaultTransport = openRouterCostTransport(func(req *http.Request) (*http.Response, error) {
				assert.Equal(t, "gen-subscription-recovery", req.URL.Query().Get("id"))
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"id":"gen-subscription-recovery","total_cost":0.002}}`)), Header: make(http.Header)}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = originalTransport })
			for range 3 {
				reconcileOpenRouterCostLedgers(context.Background())
			}
			recoveredLedger, err := model.GetOpenRouterCostLedger(info.RequestId)
			require.NoError(t, err)
			assert.Equal(t, model.OpenRouterCostStatusSettled, recoveredLedger.Status)
			assert.Equal(t, "subscription", recoveredLedger.BillingSource)
			assert.Equal(t, recoverySub.Id, recoveredLedger.SubscriptionId)
			var recoveredLogs []model.Log
			require.NoError(t, model.LOG_DB.Where("request_id = ? AND type = ?", info.RequestId, model.LogTypeConsume).Find(&recoveredLogs).Error)
			require.Len(t, recoveredLogs, 1)
			var other map[string]any
			require.NoError(t, common.UnmarshalJsonStr(recoveredLogs[0].Other, &other))
			assert.Equal(t, "subscription", other["billing_source"])
			assert.EqualValues(t, recoverySub.Id, other["subscription_id"])
			assert.EqualValues(t, recoverySub.PlanId, other["subscription_plan_id"])
			assert.EqualValues(t, 2000, other["subscription_consumed"])
		})
	}
}

func TestOpenRouterCostSubscriptionRefundAfterOverageAndResetDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			c, info := openRouterCostDatabase(t, dialect)
			plan, sub := seedOpenRouterCostSubscription(t, 1, 1, 1000, 900, false, model.SubscriptionResetNever, 0)
			require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", info.UserId).Update("quota", 0).Error)
			prepareOpenRouterCostSubscriptionRequest(t, c, info, "cost-subscription-with-ordinary-refund")
			reserved, err := model.PreConsumeUserSubscription("ordinary-reserved", info.UserId, "model", 0, 100)
			require.NoError(t, err)
			assert.EqualValues(t, 1000, reserved.AmountUsedAfter)
			require.NoError(t, model.UpdateOpenRouterCostLedgerResult(info.RequestId, "gen-mixed-refund", "0.0002", model.OpenRouterCostStatusPending, ""))
			charged, err := model.SettleOpenRouterCostLedger(info.RequestId, "0.0002", 200, nil)
			require.NoError(t, err)
			assert.True(t, charged)
			require.NoError(t, model.RefundSubscriptionPreConsume("ordinary-reserved"))
			var afterRefund model.UserSubscription
			require.NoError(t, model.DB.First(&afterRefund, sub.Id).Error)
			assert.EqualValues(t, 1100, afterRefund.AmountUsed, "an ordinary refund must reduce postpaid overage while preserving the actual-cost charge")

			// An old reservation refund after a reset must not reduce usage already
			// incurred in the new allowance cycle.
			_, err = model.AdminResetUserSubscriptionsByPlan(info.UserId, plan.Id, false)
			require.NoError(t, err)
			oldCycle, err := model.PreConsumeUserSubscription("ordinary-old-cycle", info.UserId, "model", 0, 100)
			require.NoError(t, err)
			assert.EqualValues(t, 100, oldCycle.AmountUsedAfter)
			_, err = model.AdminResetUserSubscriptionsByPlan(info.UserId, plan.Id, false)
			require.NoError(t, err)
			newCycle, err := model.PreConsumeUserSubscription("ordinary-new-cycle", info.UserId, "model", 0, 300)
			require.NoError(t, err)
			assert.EqualValues(t, 300, newCycle.AmountUsedAfter)
			require.NoError(t, model.RefundSubscriptionPreConsume("ordinary-old-cycle"))
			var afterOldRefund model.UserSubscription
			require.NoError(t, model.DB.First(&afterOldRefund, sub.Id).Error)
			assert.EqualValues(t, 300, afterOldRefund.AmountUsed)
		})
	}
}

func TestOpenRouterCostBillingDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			t.Run("settlement uses frozen group and allows debt exactly once", func(t *testing.T) {
				c, info := openRouterCostDatabase(t, dialect)
				prepareOpenRouterCostRequest(t, c, info)
				var before model.User
				require.NoError(t, model.DB.First(&before, 1).Error)
				assert.Equal(t, 1000, before.Quota, "admission must not reserve wallet credit")
				var beforeToken model.Token
				require.NoError(t, model.DB.First(&beforeToken, 1).Error)
				assert.Equal(t, 1000, beforeToken.RemainQuota, "admission must not reserve token credit")
				ledger, err := model.GetOpenRouterCostLedger(info.RequestId)
				require.NoError(t, err)
				assert.Equal(t, "wallet", ledger.BillingSource)
				assert.Zero(t, ledger.SubscriptionId, "legacy wallet ledgers keep zero-valued subscription snapshots")
				assert.Zero(t, ledger.SubscriptionQuotaCycleVersion)
				info.PriceData.GroupRatioInfo.GroupRatio = 9
				info.OpenRouterCostFinal = true
				stopOpenRouterCostHeartbeat(info)
				for range 2 {
					status, quota, _ := settleOpenRouterCostResponse(info, &dto.Usage{Cost: json.Number("0.002")})
					assert.Equal(t, model.OpenRouterCostStatusSettled, status)
					assert.Equal(t, 2000, quota)
				}
				var user model.User
				require.NoError(t, model.DB.First(&user, 1).Error)
				assert.Equal(t, -1000, user.Quota)
				assert.Equal(t, 2000, user.UsedQuota)
				assert.Equal(t, 1, user.RequestCount)
				var token model.Token
				require.NoError(t, model.DB.First(&token, 1).Error)
				assert.Equal(t, -1000, token.RemainQuota)
				assert.Equal(t, 2000, token.UsedQuota)
				assert.NotNil(t, PrepareOpenRouterCostBilling(c, info), "debt blocks subsequent wallet-funded requests")
				require.NoError(t, model.DB.Model(&user).Update("quota", gorm.Expr("quota + ?", 1500)).Error)
				require.NoError(t, model.DB.First(&user, 1).Error)
				assert.Equal(t, 500, user.Quota, "top-up credit repays debt before adding spendable balance")
			})
			t.Run("zero is free but missing or incomplete cost is pending", func(t *testing.T) {
				for _, tc := range []struct {
					name   string
					value  any
					final  bool
					status string
				}{
					{"explicit zero", float64(0), true, model.OpenRouterCostStatusSettled},
					{"missing", nil, true, model.OpenRouterCostStatusPending},
					{"invalid", "-1", true, model.OpenRouterCostStatusPending},
					{"incomplete zero", float64(0), false, model.OpenRouterCostStatusPending},
				} {
					t.Run(tc.name, func(t *testing.T) {
						c, info := openRouterCostDatabase(t, dialect)
						prepareOpenRouterCostRequest(t, c, info)
						info.OpenRouterGenerationID, info.OpenRouterCostFinal = "gen-cost-fixture", tc.final
						require.NoError(t, info.OpenRouterGenerationRecorder(info.OpenRouterGenerationID))
						stopOpenRouterCostHeartbeat(info)
						status, quota, _ := settleOpenRouterCostResponse(info, &dto.Usage{Cost: tc.value})
						assert.Equal(t, tc.status, status)
						assert.Zero(t, quota)
						ledger, err := model.GetOpenRouterCostLedger(info.RequestId)
						require.NoError(t, err)
						assert.Equal(t, tc.status, ledger.Status)
						var user model.User
						require.NoError(t, model.DB.First(&user, 1).Error)
						assert.Equal(t, 1000, user.Quota)
					})
				}
			})
			t.Run("deleting a token does not cancel its outstanding wallet charge", func(t *testing.T) {
				c, info := openRouterCostDatabase(t, dialect)
				prepareOpenRouterCostRequest(t, c, info)
				stopOpenRouterCostHeartbeat(info)
				require.NoError(t, model.DeleteTokenById(info.TokenId, info.UserId))
				_, err := model.GetTokenByIds(info.TokenId, info.UserId)
				require.ErrorIs(t, err, gorm.ErrRecordNotFound)
				info.OpenRouterCostFinal = true
				for range 2 {
					status, quota, _ := settleOpenRouterCostResponse(info, &dto.Usage{Cost: json.Number("0.002")})
					assert.Equal(t, model.OpenRouterCostStatusSettled, status)
					assert.Equal(t, 2000, quota)
				}
				var user model.User
				require.NoError(t, model.DB.First(&user, info.UserId).Error)
				assert.Equal(t, -1000, user.Quota)
				assert.Equal(t, 1, user.RequestCount)
				var token model.Token
				require.NoError(t, model.DB.Unscoped().First(&token, info.TokenId).Error)
				assert.True(t, token.DeletedAt.Valid, "settlement must not reactivate the token")
				assert.Equal(t, -1000, token.RemainQuota)
				assert.Equal(t, 2000, token.UsedQuota)
				_, err = model.GetTokenByIds(info.TokenId, info.UserId)
				assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
			})
			t.Run("an exhausted token is rejected even when a subscription can fund the request", func(t *testing.T) {
				c, info := openRouterCostDatabase(t, dialect)
				_, _ = seedOpenRouterCostSubscription(t, 1, 1, 1000, 0, true, model.SubscriptionResetNever, 0)
				info.UserSetting.BillingPreference = "subscription_only"
				require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("remain_quota", 0).Error)
				c.Set("token_quota", 1000)
				assert.NotNil(t, PrepareOpenRouterCostBilling(c, info), "stale context must not bypass the current token balance")
				var count int64
				require.NoError(t, model.DB.Model(&model.OpenRouterCostLedger{}).Count(&count).Error)
				assert.Zero(t, count)
			})
			t.Run("parallel settlement cannot debit the same generation twice", func(t *testing.T) {
				c, info := openRouterCostDatabase(t, dialect)
				prepareOpenRouterCostRequest(t, c, info)
				stopOpenRouterCostHeartbeat(info)
				require.NoError(t, model.UpdateOpenRouterCostLedgerResult(info.RequestId, "gen-cost-fixture", "0.002", model.OpenRouterCostStatusPending, ""))
				type result struct {
					charged bool
					err     error
				}
				results := make(chan result, 2)
				var workers sync.WaitGroup
				for range 2 {
					workers.Go(func() {
						charged, err := model.SettleOpenRouterCostLedger(info.RequestId, "0.002", 2000, nil)
						results <- result{charged, err}
					})
				}
				workers.Wait()
				close(results)
				charges := 0
				for result := range results {
					require.NoError(t, result.err)
					if result.charged {
						charges++
					}
				}
				assert.Equal(t, 1, charges)
				var user model.User
				require.NoError(t, model.DB.First(&user, 1).Error)
				assert.Equal(t, -1000, user.Quota)
				assert.Equal(t, 1, user.RequestCount)

			})
			t.Run("restart reconciliation repairs missing log without a second charge", func(t *testing.T) {
				c, info := openRouterCostDatabase(t, dialect)
				prepareOpenRouterCostRequest(t, c, info)
				info.OpenRouterGenerationID = "gen-cost-fixture"
				require.NoError(t, info.OpenRouterGenerationRecorder(info.OpenRouterGenerationID))
				persisted, err := model.GetOpenRouterCostLedger(info.RequestId)
				require.NoError(t, err)
				assert.Equal(t, "gen-cost-fixture", persisted.GenerationId, "headers are durable before the response finishes")
				stopOpenRouterCostHeartbeat(info)
				require.NoError(t, model.DeleteTokenById(info.TokenId, info.UserId))
				FinalizeOpenRouterCostFailure(info)
				originalTransport := http.DefaultTransport
				http.DefaultTransport = openRouterCostTransport(func(req *http.Request) (*http.Response, error) {
					assert.Equal(t, "/api/v1/generation", req.URL.Path)
					assert.Equal(t, "gen-cost-fixture", req.URL.Query().Get("id"))
					assert.Equal(t, "Bearer upstream-cost-fixture", req.Header.Get("Authorization"))
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"id":"gen-cost-fixture","total_cost":0.002}}`)), Header: make(http.Header)}, nil
				})
				t.Cleanup(func() { http.DefaultTransport = originalTransport })
				for range 3 {
					reconcileOpenRouterCostLedgers(context.Background())
				}
				ledger, err := model.GetOpenRouterCostLedger(info.RequestId)
				require.NoError(t, err)
				assert.Equal(t, model.OpenRouterCostStatusSettled, ledger.Status)
				assert.Equal(t, int64(2000), ledger.Quota)
				var logs []model.Log
				require.NoError(t, model.LOG_DB.Where("request_id = ? AND type = ?", info.RequestId, model.LogTypeConsume).Find(&logs).Error)
				require.Len(t, logs, 1)
				assert.Equal(t, 2000, logs[0].Quota)
				var other map[string]any
				require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
				assert.Equal(t, "settled", other["billing_status"])
				assert.NotContains(t, other, "upstream_cost_usd")
				var user model.User
				require.NoError(t, model.DB.First(&user, 1).Error)
				assert.Equal(t, -1000, user.Quota)
				assert.Equal(t, 1, user.RequestCount)
				var token model.Token
				require.NoError(t, model.DB.Unscoped().First(&token, info.TokenId).Error)
				assert.True(t, token.DeletedAt.Valid)
				assert.Equal(t, 2000, token.UsedQuota)
			})
			t.Run("foreground completion updates the recovered log instead of inserting twice", func(t *testing.T) {
				c, info := openRouterCostDatabase(t, dialect)
				info.PriceData.GroupRatioInfo.GroupSpecialRatio = 2
				info.PriceData.GroupRatioInfo.HasSpecialRatio = true
				prepareOpenRouterCostRequest(t, c, info)
				stopOpenRouterCostHeartbeat(info)
				require.NoError(t, model.UpdateOpenRouterCostLedgerResult(info.RequestId, "gen-cost-fixture", "", model.OpenRouterCostStatusPending, ""))
				syncOpenRouterCostLog(info.RequestId)
				info.OpenRouterCostFinal = true
				info.OpenRouterGenerationID = "gen-cost-fixture"
				PostTextConsumeQuota(c, info, &dto.Usage{PromptTokens: 14, CompletionTokens: 2, TotalTokens: 16, Cost: json.Number("0.002")}, nil)
				var logs []model.Log
				require.NoError(t, model.LOG_DB.Where("request_id = ? AND type = ?", info.RequestId, model.LogTypeConsume).Find(&logs).Error)
				require.Len(t, logs, 1)
				assert.Equal(t, 2000, logs[0].Quota)
				assert.Equal(t, 14, logs[0].PromptTokens)
				assert.Equal(t, 2, logs[0].CompletionTokens)
				var other map[string]any
				require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
				assert.Equal(t, "settled", other["billing_status"])
				publicLogs, err := model.GetLogByTokenId(info.TokenId)
				require.NoError(t, err)
				require.Len(t, publicLogs, 1)
				var publicOther map[string]any
				require.NoError(t, common.UnmarshalJsonStr(publicLogs[0].Other, &publicOther))
				assert.NotContains(t, publicOther, "admin_info")
				assert.NotContains(t, publicOther, "upstream_cost_usd")
				assert.NotContains(t, publicOther, "group_ratio")
				assert.NotContains(t, publicOther, "user_group_ratio")
				var totals struct {
					Count     int
					TokenUsed int
					Quota     int
				}
				require.NoError(t, model.DB.Model(&model.QuotaData{}).Select("sum(count) AS count, sum(token_used) AS token_used, sum(quota) AS quota").Where("user_id = ?", info.UserId).Scan(&totals).Error)
				assert.Equal(t, 1, totals.Count)
				assert.Equal(t, 16, totals.TokenUsed)
				assert.Equal(t, 2000, totals.Quota)
			})
			t.Run("saved final cost settles without a generation header", func(t *testing.T) {
				c, info := openRouterCostDatabase(t, dialect)
				prepareOpenRouterCostRequest(t, c, info)
				stopOpenRouterCostHeartbeat(info)
				require.NoError(t, model.UpdateOpenRouterCostLedgerResult(info.RequestId, "", "0.002", model.OpenRouterCostStatusPending, ""))
				reconcileOpenRouterCostLedgers(context.Background())
				ledger, err := model.GetOpenRouterCostLedger(info.RequestId)
				require.NoError(t, err)
				assert.Equal(t, model.OpenRouterCostStatusSettled, ledger.Status)
				var user model.User
				require.NoError(t, model.DB.First(&user, 1).Error)
				assert.Equal(t, -1000, user.Quota)
			})
			t.Run("log database outage does not prevent recovering and charging cost", func(t *testing.T) {
				c, info := openRouterCostDatabase(t, dialect)
				prepareOpenRouterCostRequest(t, c, info)
				stopOpenRouterCostHeartbeat(info)
				require.NoError(t, model.UpdateOpenRouterCostLedgerResult(info.RequestId, "gen-cost-fixture", "", model.OpenRouterCostStatusPending, ""))
				primaryLogDB := model.LOG_DB
				logPath := filepath.Join(t.TempDir(), "separate-log.sqlite")
				logConfig := &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "orcost_"}, Logger: gormlogger.Default.LogMode(gormlogger.Silent)}
				logDB, err := gorm.Open(sqlite.Open(logPath), logConfig)
				require.NoError(t, err)
				require.NoError(t, logDB.AutoMigrate(&model.Log{}))
				logSQL, err := logDB.DB()
				require.NoError(t, err)
				require.NoError(t, logSQL.Close())
				model.LOG_DB = logDB
				originalTransport := http.DefaultTransport
				http.DefaultTransport = openRouterCostTransport(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"id":"gen-cost-fixture","total_cost":0.002}}`)), Header: make(http.Header)}, nil
				})
				t.Cleanup(func() { http.DefaultTransport = originalTransport; model.LOG_DB = primaryLogDB })
				for range 2 {
					reconcileOpenRouterCostLedgers(context.Background())
				}
				ledger, err := model.GetOpenRouterCostLedger(info.RequestId)
				require.NoError(t, err)
				assert.Equal(t, model.OpenRouterCostStatusSettled, ledger.Status)
				assert.True(t, ledger.LogUpdatePending)
				var user model.User
				require.NoError(t, model.DB.First(&user, 1).Error)
				assert.Equal(t, -1000, user.Quota)
				assert.Equal(t, 1, user.RequestCount)
				recoveredLog, err := gorm.Open(sqlite.Open(logPath), logConfig)
				require.NoError(t, err)
				model.LOG_DB = recoveredLog
				recoveredSQL, err := recoveredLog.DB()
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, recoveredSQL.Close()) })
				require.NoError(t, model.DB.Model(&model.OpenRouterCostLedger{}).Where("request_id = ?", info.RequestId).Update("log_next_attempt_at", 0).Error)
				syncOpenRouterCostLog(info.RequestId)
				ledger, err = model.GetOpenRouterCostLedger(info.RequestId)
				require.NoError(t, err)
				assert.False(t, ledger.LogUpdatePending)
				var logs []model.Log
				require.NoError(t, recoveredLog.Where("request_id = ?", info.RequestId).Find(&logs).Error)
				require.Len(t, logs, 1)
				assert.Equal(t, 2000, logs[0].Quota)
				require.NoError(t, model.DB.First(&user, 1).Error)
				assert.Equal(t, -1000, user.Quota, "repairing the separate log cannot charge again")
			})
		})
	}
}

type openRouterCostTransport func(*http.Request) (*http.Response, error)

func (transport openRouterCostTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestOpenRouterCostDashboardDoesNotMixWithOrdinaryHourlyCache(t *testing.T) {
	c, info := openRouterCostDatabase(t, "sqlite", openRouterCostDatabaseOptions{plainTables: true})
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 1).Update("quota", 100000).Error)
	require.NoError(t, model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("remain_quota", 100000).Error)
	model.CacheQuotaDataLock.Lock()
	originalCache := model.CacheQuotaData
	model.CacheQuotaData = make(map[string]*model.QuotaData)
	model.CacheQuotaDataLock.Unlock()
	t.Cleanup(func() {
		model.CacheQuotaDataLock.Lock()
		model.CacheQuotaData = originalCache
		model.CacheQuotaDataLock.Unlock()
	})
	ordinary := model.QuotaDataLogParams{UserID: 1, Username: "cost-user", ModelName: info.OriginModelName, Quota: 50, CreatedAt: common.GetTimestamp(), TokenUsed: 10, UseGroup: "default", TokenID: 1, ChannelID: 1, NodeName: common.NodeName}
	model.LogQuotaData(ordinary)
	for _, requestID := range []string{"cost-request", "cost-request-second"} {
		info.RequestId = requestID
		c.Set(common.RequestIdKey, requestID)
		prepareOpenRouterCostRequest(t, c, info)
		info.OpenRouterCostFinal = true
		PostTextConsumeQuota(c, info, &dto.Usage{PromptTokens: 14, CompletionTokens: 2, TotalTokens: 16, Cost: json.Number("0.002")}, nil)
	}
	model.SaveQuotaDataCache()
	// A later foreground replay must not erase ordinary usage already flushed
	// into the same hourly dimensions.
	PostTextConsumeQuota(c, info, &dto.Usage{PromptTokens: 14, CompletionTokens: 2, TotalTokens: 16, Cost: json.Number("0.002")}, nil)
	model.LogQuotaData(ordinary)
	model.SaveQuotaDataCache()
	var totals struct {
		Count     int
		TokenUsed int
		Quota     int
	}
	require.NoError(t, model.DB.Model(&model.QuotaData{}).Select("sum(count) AS count, sum(token_used) AS token_used, sum(quota) AS quota").Where("user_id = ?", 1).Scan(&totals).Error)
	assert.Equal(t, 4, totals.Count)
	assert.Equal(t, 52, totals.TokenUsed)
	assert.Equal(t, 4100, totals.Quota)
}

func TestOpenRouterCostUserLogAPIsHideMultipliersAndKeepOrdinaryPricing(t *testing.T) {
	c, info := openRouterCostDatabase(t, "sqlite", openRouterCostDatabaseOptions{plainTables: true})
	info.PriceData.GroupRatioInfo.GroupSpecialRatio = 2
	info.PriceData.GroupRatioInfo.HasSpecialRatio = true
	prepareOpenRouterCostRequest(t, c, info)
	info.OpenRouterCostFinal = true
	PostTextConsumeQuota(c, info, &dto.Usage{PromptTokens: 14, CompletionTokens: 2, TotalTokens: 16, Cost: json.Number("0.002")}, nil)
	var stored model.Log
	require.NoError(t, model.LOG_DB.Where("request_id = ?", info.RequestId).First(&stored).Error)
	var adminOther map[string]any
	require.NoError(t, common.UnmarshalJsonStr(stored.Other, &adminOther))
	adminInfo, ok := adminOther["admin_info"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, float64(2), adminInfo["group_ratio"])
	assert.Equal(t, "0.002", adminInfo["upstream_cost_usd"])
	require.NoError(t, model.LOG_DB.Create(&model.Log{UserId: info.UserId, TokenId: info.TokenId, Type: model.LogTypeConsume, RequestId: "ordinary-pricing-request", Other: `{"group_ratio":3,"user_group_ratio":4}`}).Error)
	selfLogs, total, err := model.GetUserLogs(info.UserId, model.LogFilter{}, 0, 20)
	require.NoError(t, err)
	assert.Equal(t, int64(2), total)
	tokenLogs, err := model.GetLogByTokenId(info.TokenId)
	require.NoError(t, err)
	for _, logs := range [][]*model.Log{selfLogs, tokenLogs} {
		require.Len(t, logs, 2)
		for _, log := range logs {
			var other map[string]any
			require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
			if log.RequestId == "ordinary-pricing-request" {
				assert.Equal(t, float64(3), other["group_ratio"])
				assert.Equal(t, float64(4), other["user_group_ratio"])
				continue
			}
			assert.Equal(t, info.RequestId, log.RequestId)
			assert.NotContains(t, other, "admin_info")
			assert.NotContains(t, other, "upstream_cost_usd")
			assert.NotContains(t, other, "group_ratio")
			assert.NotContains(t, other, "user_group_ratio")
		}
	}
}

func TestOpenRouterCostRejectsInvalidAmountsAndBoundsQuota(t *testing.T) {
	for _, value := range []any{nil, "", "-0.01", math.NaN(), math.Inf(1), true, "1e-2147483647", "1e2147483647", strings.Repeat("9", 1024)} {
		_, valid := parseOpenRouterCost(value)
		assert.False(t, valid, "invalid cost %v", value)
	}
	quota, clamp, err := openRouterCostQuota("1e30", "2", 500000)
	require.NoError(t, err)
	require.NotNil(t, clamp)
	assert.Equal(t, int64(math.MaxInt32), quota)
	_, _, err = openRouterCostQuota("0.01", "-1", 500000)
	assert.Error(t, err)
}

func TestOpenRouterCostInvalidContextReturnsSetupError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		context *gin.Context
	}{
		{"nil context", nil},
		{"missing request", &gin.Context{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NotPanics(t, func() {
				assert.NotNil(t, PrepareTieredBillingForSelectedGroup(tc.context, &relaycommon.RelayInfo{OpenRouterCostBilling: true}))
			})
		})
	}
}
