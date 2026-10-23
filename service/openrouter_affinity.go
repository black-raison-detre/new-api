package service

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/cachex"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/samber/hot"
)

const openRouterAccountAffinityTTL = 10 * time.Minute

type openRouterAccountBinding struct {
	ChannelID     int    `json:"channel_id"`
	Credential    string `json:"credential"`
	Configuration string `json:"configuration"`
}

var openRouterAccountAffinityOnce sync.Once
var openRouterAccountAffinityCache *cachex.HybridCache[openRouterAccountBinding]

func getOpenRouterAccountAffinityCache() *cachex.HybridCache[openRouterAccountBinding] {
	openRouterAccountAffinityOnce.Do(func() {
		openRouterAccountAffinityCache = cachex.NewHybridCache[openRouterAccountBinding](cachex.HybridCacheConfig[openRouterAccountBinding]{
			Namespace:    cachex.Namespace("new-api:openrouter_account_affinity:v1"),
			Redis:        common.RDB,
			RedisCodec:   cachex.JSONCodec[openRouterAccountBinding]{},
			RedisEnabled: func() bool { return common.RedisEnabled && common.RDB != nil },
			Memory: func() *hot.HotCache[string, openRouterAccountBinding] {
				return hot.NewHotCache[string, openRouterAccountBinding](hot.LRU, 100_000).
					WithTTL(openRouterAccountAffinityTTL).WithJanitor().Build()
			},
		})
	})
	return openRouterAccountAffinityCache
}

func openRouterAccountAffinityKey(c *gin.Context, modelName, group string) string {
	if c == nil || c.Request == nil || common.GetContextKeyInt(c, constant.ContextKeyUserId) <= 0 {
		return ""
	}
	if !strings.Contains(c.ContentType(), "json") || c.Request.Method != "POST" {
		return ""
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return ""
	}
	body, err := storage.Bytes()
	if err != nil {
		return ""
	}
	conversation, err := relaycommon.OpenRouterConversationKey(body, c.GetHeader("x-session-id"))
	if err != nil || conversation == "" {
		return ""
	}
	return relaycommon.OpenRouterAffinityKey(common.GetContextKeyInt(c, constant.ContextKeyUserId), modelName, group, conversation)
}

func openRouterChannelConfiguration(channel *model.Channel) string {
	data, _ := common.Marshal([]string{channel.OtherSettings, channel.GetModelMapping(), channel.GetBaseURL()})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// getOpenRouterSessionChannel preserves the account that owns OpenRouter's native
// provider affinity. It is a preference; all current channel filters still apply.
func getOpenRouterSessionChannel(c *gin.Context, modelName, usingGroup string) (*model.Channel, string) {
	key := openRouterAccountAffinityKey(c, modelName, usingGroup)
	if key == "" {
		return nil, ""
	}
	c.Set("openrouter_account_affinity_key", key)
	binding, found, err := getOpenRouterAccountAffinityCache().Get(key)
	if err != nil || !found {
		return nil, ""
	}
	channel, err := model.CacheGetChannel(binding.ChannelID)
	if err != nil || channel == nil || channel.Type != constant.ChannelTypeOpenRouter ||
		channel.Status != common.ChannelStatusEnabled ||
		binding.Configuration != openRouterChannelConfiguration(channel) {
		_, _ = getOpenRouterAccountAffinityCache().DeleteMany([]string{key})
		return nil, ""
	}
	settings := channel.GetOtherSettings()
	if settings.IsOpenRouterEnterprise() {
		return nil, ""
	}
	if allowed, _ := model.ChannelSatisfiesFilters(channel, modelName, GetChannelConstraints(c).Filters); !allowed {
		return nil, ""
	}
	groups := []string{usingGroup}
	if usingGroup == "auto" {
		groups = GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup))
	}
	for _, group := range groups {
		if !model.IsChannelEnabledForGroupModel(group, modelName, channel.Id) {
			continue
		}
		if _, _, ok := openRouterBoundCredential(channel, binding); !ok {
			_, _ = getOpenRouterAccountAffinityCache().DeleteMany([]string{key})
			return nil, ""
		}
		c.Set("openrouter_account_binding", binding)
		if usingGroup == "auto" {
			common.SetContextKey(c, constant.ContextKeyAutoGroup, group)
		}
		return channel, group
	}
	return nil, ""
}

func openRouterBoundCredential(channel *model.Channel, binding openRouterAccountBinding) (string, int, bool) {
	if channel.Id != binding.ChannelID || openRouterChannelConfiguration(channel) != binding.Configuration {
		return "", 0, false
	}
	lock := model.GetChannelPollingLock(channel.Id)
	lock.Lock()
	defer lock.Unlock()
	keys := []string{channel.Key}
	if channel.ChannelInfo.IsMultiKey {
		keys = channel.GetKeys()
	}
	for index, key := range keys {
		if channel.ChannelInfo.IsMultiKey {
			if status, exists := channel.ChannelInfo.MultiKeyStatusList[index]; exists && status != common.ChannelStatusEnabled {
				continue
			}
		}
		if fmt.Sprintf("%x", sha256.Sum256([]byte(key))) == binding.Credential {
			return key, index, true
		}
	}
	return "", 0, false
}

// OpenRouterSessionCredential avoids rotating a conversation between accounts in
// a multi-key channel. Disabled, replaced, or removed credentials are never reused.
func OpenRouterSessionCredential(c *gin.Context, channel *model.Channel) (string, int, bool) {
	if channel.Type != constant.ChannelTypeOpenRouter || RequestPolicy(c).Attempts > 1 {
		return "", 0, false
	}
	value, exists := c.Get("openrouter_account_binding")
	if !exists {
		group := common.GetContextKeyString(c, constant.ContextKeyTokenGroup)
		if group == "" {
			group = common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
		}
		key := openRouterAccountAffinityKey(c, c.GetString("original_model"), group)
		if key == "" {
			return "", 0, false
		}
		binding, found, err := getOpenRouterAccountAffinityCache().Get(key)
		if err != nil || !found {
			return "", 0, false
		}
		return openRouterBoundCredential(channel, binding)
	}
	binding, ok := value.(openRouterAccountBinding)
	if !ok {
		return "", 0, false
	}
	return openRouterBoundCredential(channel, binding)
}

// RecordOpenRouterSessionAffinity records only successful inference. OpenRouter
// owns endpoint affinity; this cache keeps the corresponding upstream account.
func RecordOpenRouterSessionAffinity(c *gin.Context, info *relaycommon.RelayInfo) {
	if c == nil || info == nil || info.ChannelMeta == nil || info.ChannelType != constant.ChannelTypeOpenRouter ||
		info.ChannelOtherSettings.IsOpenRouterEnterprise() {
		return
	}
	key := c.GetString("openrouter_account_affinity_key")
	if key == "" {
		group := common.GetContextKeyString(c, constant.ContextKeyTokenGroup)
		if group == "" {
			group = common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
		}
		key = openRouterAccountAffinityKey(c, info.OriginModelName, group)
	}
	if key == "" || info.ChannelId <= 0 || info.ApiKey == "" {
		return
	}
	channel, err := model.CacheGetChannel(info.ChannelId)
	if err != nil || channel == nil {
		return
	}
	binding := openRouterAccountBinding{ChannelID: info.ChannelId,
		Credential:    fmt.Sprintf("%x", sha256.Sum256([]byte(info.ApiKey))),
		Configuration: openRouterChannelConfiguration(channel)}
	if err := getOpenRouterAccountAffinityCache().SetWithTTL(key, binding, openRouterAccountAffinityTTL); err != nil {
		common.SysLog(common.LogText("failed to save OpenRouter account affinity"))
	}
}
