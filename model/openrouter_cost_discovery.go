package model

import (
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

const (
	ChannelBillingSourceLocal          = "local"
	ChannelBillingSourceOpenRouterCost = "openrouter_cost"
)

// BillingSource describes the channel's accounting policy without changing its
// settings or looking up a model's local price.
func (channel *Channel) BillingSource() string {
	if channel.Type != constant.ChannelTypeOpenRouter {
		return ChannelBillingSourceLocal
	}
	var settings dto.ChannelOtherSettings
	if common.UnmarshalJsonStr(channel.OtherSettings, &settings) == nil &&
		settings.OpenRouterCostBilling && !settings.IsOpenRouterEnterprise() {
		return ChannelBillingSourceOpenRouterCost
	}
	return ChannelBillingSourceLocal
}

func (channel *Channel) openRouterCostDiscoveryAvailable() bool {
	if channel == nil || channel.Status != common.ChannelStatusEnabled || channel.BillingSource() != ChannelBillingSourceOpenRouterCost {
		return false
	}
	if !channel.ChannelInfo.IsMultiKey {
		return strings.TrimSpace(channel.Key) != ""
	}
	available := false
	for i, key := range channel.GetKeys() {
		status, exists := channel.ChannelInfo.MultiKeyStatusList[i]
		if exists && status != common.ChannelStatusEnabled {
			continue
		}
		if strings.TrimSpace(key) == "" {
			return false
		}
		available = true
	}
	return available
}

// GetOpenRouterCostOnlyModels permits discovery without a local price only when
// all routes available to these groups use returned-cost billing. A mixed route
// still needs local pricing because normal selection can choose that channel.
// Reading credentials here never advances a multi-key polling cursor.
func GetOpenRouterCostOnlyModels(groups []string) (map[string]bool, error) {
	eligible := make(map[string]bool)
	if len(groups) == 0 {
		return eligible, nil
	}
	type billingRoute struct {
		group, name string
		available   bool
	}
	var routes []billingRoute
	if common.MemoryCacheEnabled {
		// Read only routing and key-status fields, never copy the live polling
		// cursor. Key-status writers take these locks in this order as well.
		channelStatusLock.Lock()
		channelSyncLock.RLock()
		for _, group := range groups {
			for name, ids := range group2model2channels[group] {
				for _, id := range ids {
					routes = append(routes, billingRoute{group, name, channelsIDM[id].openRouterCostDiscoveryAvailable()})
				}
			}
		}
		channelSyncLock.RUnlock()
		channelStatusLock.Unlock()
	} else {
		var rows []struct {
			ModelName  string
			RouteGroup string
			Channel
		}
		err := DB.Table("abilities").
			Select("abilities.model as model_name, abilities."+commonGroupCol+" as route_group, channels.*").
			Joins("JOIN channels ON abilities.channel_id = channels.id").
			Where("abilities.enabled = ? AND channels.status = ?", true, common.ChannelStatusEnabled).
			Where("abilities."+commonGroupCol+" IN ?", groups).
			Scan(&rows).Error
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			routes = append(routes, billingRoute{row.RouteGroup, row.ModelName, row.openRouterCostDiscoveryAvailable()})
		}
	}
	byGroup := make(map[string]map[string]bool)
	for _, route := range routes {
		if byGroup[route.group] == nil {
			byGroup[route.group] = make(map[string]bool)
		}
		previous, seen := byGroup[route.group][route.name]
		byGroup[route.group][route.name] = route.available && (!seen || previous)
		eligible[route.name] = true
	}
	for name := range eligible {
		normalized := ratio_setting.RoutingMatchModelName(name)
		for _, group := range groups {
			available, exists := byGroup[group][name]
			if !exists && normalized != name {
				// Match the selector's exact-name precedence and normalized
				// fallback independently within each effective Auto group.
				available, exists = byGroup[group][normalized]
			}
			if exists && !available {
				eligible[name] = false
				break
			}
		}
	}
	return eligible, nil
}
