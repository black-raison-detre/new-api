package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestOpenRouterSessionAccountAffinity(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const modelName = "openrouter-session-model"
	createChannelSelectAutoGroupsChannel(t, db, 4201, "default", modelName)
	createChannelSelectAutoGroupsChannel(t, db, 4202, "default", modelName)
	var first model.Channel
	require.NoError(t, db.First(&first, 4201).Error)
	first.Type = constant.ChannelTypeOpenRouter
	first.Key = "first-account\nsecond-account"
	first.ChannelInfo = model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyMode: constant.MultiKeyModePolling}
	require.NoError(t, db.Save(&first).Error)
	var alternative model.Channel
	require.NoError(t, db.First(&alternative, 4202).Error)
	alternative.Type = constant.ChannelTypeOpenRouter
	alternative.Priority = common.GetPointer(int64(1))
	require.NoError(t, db.Save(&alternative).Error)
	require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", alternative.Id).Update("priority", 1).Error)
	model.InitChannelCache()

	context := func(userID int, session string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"openrouter-session-model","messages":[{"role":"user","content":"hello"}]}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Request.Header.Set("x-session-id", session)
		common.SetContextKey(c, constant.ContextKeyUserId, userID)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenGroup, "default")
		c.Set("original_model", modelName)
		return c
	}
	firstRequest := context(71, t.Name())
	info := &relaycommon.RelayInfo{OriginModelName: modelName, ChannelMeta: &relaycommon.ChannelMeta{
		ChannelType: constant.ChannelTypeOpenRouter, ChannelId: first.Id, ApiKey: "second-account",
	}}
	RecordOpenRouterSessionAffinity(firstRequest, info)
	cacheKey := openRouterAccountAffinityKey(firstRequest, modelName, "default")
	t.Cleanup(func() { _, _ = getOpenRouterAccountAffinityCache().DeleteMany([]string{cacheKey}) })

	next := context(71, t.Name())
	selected, group, selectErr := SelectChannelForRequest(next, modelName, &RetryParam{Ctx: next, TokenGroup: "default", ModelName: modelName})
	require.Nil(t, selectErr)
	require.NotNil(t, selected)
	assert.Equal(t, first.Id, selected.Id, "a warm session retains its account despite an alternative's higher priority")
	assert.Equal(t, "default", group)
	key, index, bound := OpenRouterSessionCredential(next, selected)
	assert.True(t, bound)
	assert.Equal(t, "second-account", key)
	assert.Equal(t, 1, index)
	RequestPolicy(next).BeginAttempt(selected, "default")
	RequestPolicy(next).BeginAttempt(selected, "default")
	_, _, bound = OpenRouterSessionCredential(next, selected)
	assert.False(t, bound, "a retriable failure must release the preferred credential")
	retryKey, _, retryErr := selected.GetNextEnabledKey()
	require.Nil(t, retryErr)
	assert.Equal(t, "first-account", retryKey, "ordinary key polling can recover on a healthy account")

	otherUser := context(72, t.Name())
	selected, _, selectErr = SelectChannelForRequest(otherUser, modelName, &RetryParam{Ctx: otherUser, TokenGroup: "default", ModelName: modelName})
	require.Nil(t, selectErr)
	require.NotNil(t, selected)
	assert.Equal(t, alternative.Id, selected.Id, "identical caller session IDs do not share a gateway user's account binding")

	first.ChannelInfo.MultiKeyStatusList = map[int]int{1: common.ChannelStatusManuallyDisabled}
	require.NoError(t, db.Save(&first).Error)
	model.InitChannelCache()
	afterDisable := context(71, t.Name())
	selected, _, selectErr = SelectChannelForRequest(afterDisable, modelName, &RetryParam{Ctx: afterDisable, TokenGroup: "default", ModelName: modelName})
	require.Nil(t, selectErr)
	require.NotNil(t, selected)
	assert.Equal(t, alternative.Id, selected.Id, "a disabled account is not reused by session affinity")
	_, _, bound = OpenRouterSessionCredential(afterDisable, &first)
	assert.False(t, bound)
}

func setupChannelSelectAutoGroupsTest(t *testing.T) *gorm.DB {
	t.Helper()

	originalDB := model.DB
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalRetryTimes := common.RetryTimes
	originalAutoGroups := setting.AutoGroups2JsonString()
	originalUsableGroups := setting.UserUsableGroups2JSONString()
	originalGroupRatios := ratio_setting.GroupRatio2JSONString()
	originalMaxTokenAutoGroups := setting.GetMaxTokenAutoGroups()

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	model.DB = db
	common.MemoryCacheEnabled = true
	common.RetryTimes = 0

	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`[]`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","vip":"VIP"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":2}`))
	require.NoError(t, setting.UpdateMaxTokenAutoGroups("2"))

	t.Cleanup(func() {
		model.DB = originalDB
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.RetryTimes = originalRetryTimes
		require.NoError(t, setting.UpdateAutoGroupsByJsonString(originalAutoGroups))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(originalUsableGroups))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalGroupRatios))
		require.NoError(t, setting.UpdateMaxTokenAutoGroups(fmt.Sprintf("%d", originalMaxTokenAutoGroups)))

		if originalMemoryCacheEnabled && originalDB != nil &&
			originalDB.Migrator().HasTable(&model.Channel{}) && originalDB.Migrator().HasTable(&model.Ability{}) {
			model.InitChannelCache()
		}
		sqlDB, err := db.DB()
		if err == nil {
			require.NoError(t, sqlDB.Close())
		}
	})

	return db
}

func createChannelSelectAutoGroupsChannel(t *testing.T, db *gorm.DB, id int, group, modelName string) {
	t.Helper()
	priority := int64(0)
	weight := uint(100)
	require.NoError(t, db.Create(&model.Channel{
		Id:       id,
		Type:     constant.ChannelTypeOpenAI,
		Key:      fmt.Sprintf("key-%d", id),
		Status:   common.ChannelStatusEnabled,
		Name:     fmt.Sprintf("channel-%d", id),
		Weight:   &weight,
		Models:   modelName,
		Group:    group,
		Priority: &priority,
	}).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group:     group,
		Model:     modelName,
		ChannelId: id,
		Enabled:   true,
		Priority:  &priority,
		Weight:    weight,
	}).Error)
}

func TestCacheGetRandomSatisfiedChannelUsesTokenAutoGroupsWhenGlobalAutoIsEmpty(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const modelName = "auto-groups-runtime-model"
	createChannelSelectAutoGroupsChannel(t, db, 2101, "vip", modelName)
	createChannelSelectAutoGroupsChannel(t, db, 2102, "default", modelName)
	model.InitChannelCache()

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(ctx, constant.ContextKeyTokenAutoGroups, []string{"vip", "default"})
	common.SetContextKey(ctx, constant.ContextKeyTokenCrossGroupRetry, true)

	retry := 0
	param := &RetryParam{
		Ctx:         ctx,
		TokenGroup:  "auto",
		ModelName:   modelName,
		RequestPath: "/v1/chat/completions",
		Retry:       &retry,
	}

	first, selectedGroup, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, 2101, first.Id)
	assert.Equal(t, "vip", selectedGroup)
	assert.Equal(t, "vip", common.GetContextKeyString(ctx, constant.ContextKeyAutoGroup))
	assert.Empty(t, setting.GetAutoGroups(), "the selection must not depend on the global Auto list")

	param.IncreaseRetry()
	second, selectedGroup, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, 2102, second.Id)
	assert.Equal(t, "default", selectedGroup)
	assert.Equal(t, "default", common.GetContextKeyString(ctx, constant.ContextKeyAutoGroup))
}
