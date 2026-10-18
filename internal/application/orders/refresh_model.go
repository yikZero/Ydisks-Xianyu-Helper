package orders

import (
	"strings"
	"time"
)

// refreshSoldOrderChanged 判断平台订单字段是否发生业务变化。
func refreshSoldOrderChanged(existing *Order, remote RefreshSoldOrder) bool {
	if existing == nil {
		return true
	}
	return (remote.OrderStatus != "" && remote.OrderStatus != "unknown" && NormalizeOrderStatus(existing.OrderStatus) != remote.OrderStatus) ||
		(remote.CreatedAt != "" && !sameRefreshOrderTime(existing.CreatedAt, remote.CreatedAt)) ||
		(remote.ItemID != "" && existing.ItemID != remote.ItemID) ||
		(remote.BuyerID != "" && existing.BuyerID != remote.BuyerID) ||
		(remote.Quantity != "" && existing.Quantity != remote.Quantity) ||
		(remote.Amount != "" && existing.Amount != remote.Amount) ||
		(remote.ReceiverName != "" && existing.ReceiverName != remote.ReceiverName) ||
		(remote.ReceiverPhone != "" && existing.ReceiverPhone != remote.ReceiverPhone) ||
		(remote.ReceiverAddr != "" && existing.ReceiverAddress != remote.ReceiverAddr) ||
		(remote.ReceiverCity != "" && existing.ReceiverCity != remote.ReceiverCity) ||
		(remote.IsBargain && existing.IsBargain == 0)
}

// sameRefreshOrderTime 比较数据库和平台可能使用的 UTC 文本、RFC3339 文本及带偏移时间。
func sameRefreshOrderTime(existing, remote string) bool {
	existing = strings.TrimSpace(existing)
	remote = strings.TrimSpace(remote)
	if existing == remote {
		return true
	}
	// existingTime、remoteTime 保存统一到 UTC 后的两个时间值。
	// existingTime、existingOK 保存本地订单时间及其可解析状态。
	existingTime, existingOK := parseRefreshOrderTime(existing)
	// remoteTime、remoteOK 保存平台订单时间及其可解析状态。
	remoteTime, remoteOK := parseRefreshOrderTime(remote)
	return existingOK && remoteOK && existingTime.Equal(remoteTime)
}

// parseRefreshOrderTime 解析订单刷新链路支持的有时区和无时区时间格式。
func parseRefreshOrderTime(raw string) (time.Time, bool) {
	for /* layout 表示当前尝试的订单时间布局。 */ _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		// parsed、err 保存当前布局的解析结果。
		parsed, err := time.ParseInLocation(layout, raw, time.UTC)
		if err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}
