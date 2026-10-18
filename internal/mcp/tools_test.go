package mcp

import (
	"strings"
	"testing"
)

// TestSelectDeliveryRuleMirrorsAutomationMatching 验证规则选择与自动化中心一致：商品级优先、优先级与 ID 排序、
// 跳过停用和未就绪规则，没有商品级规则时才使用确认适用全部商品的账号级规则。
func TestSelectDeliveryRuleMirrorsAutomationMatching(t *testing.T) {
	rules := []ruleRow{
		{ID: 1, CookieID: "a", ItemID: "", TriggerType: triggerOrderPaid, Enabled: true, Priority: 1, SKUMigrationStatus: "ready", ConfigJSON: `{"allow_all_items":true}`},
		{ID: 2, CookieID: "a", ItemID: "", TriggerType: triggerOrderPaid, Enabled: true, Priority: 0, SKUMigrationStatus: "ready", ConfigJSON: `{}`},
		{ID: 5, CookieID: "a", ItemID: "i", TriggerType: triggerOrderPaid, Enabled: true, Priority: 100, SKUMigrationStatus: "ready"},
		{ID: 4, CookieID: "a", ItemID: "i", TriggerType: triggerOrderPaid, Enabled: true, Priority: 100, SKUMigrationStatus: "ready"},
		{ID: 3, CookieID: "a", ItemID: "i", TriggerType: triggerOrderPaid, Enabled: false, Priority: 1, SKUMigrationStatus: "ready"},
		{ID: 6, CookieID: "a", ItemID: "i", TriggerType: triggerOrderPaid, Enabled: true, Priority: 1, SKUMigrationStatus: "pending"},
	}
	if rule := selectDeliveryRule(rules, "a", "i"); rule == nil || rule.ID != 4 {
		t.Fatalf("商品级应选优先级相同时 ID 最小的启用就绪规则，got %+v", rule)
	}
	if rule := selectDeliveryRule(rules, "a", "other"); rule == nil || rule.ID != 1 {
		t.Fatalf("没有商品级规则时只用确认全商品的账号级规则，got %+v", rule)
	}
	if rule := selectDeliveryRule(rules, "b", "i"); rule != nil {
		t.Fatalf("其他账号不应命中，got %+v", rule)
	}
}

// TestActionMatchesSpec 覆盖无规格动作、账号级全商品规则、规格分隔符归一和损坏的规格配置。
func TestActionMatchesSpec(t *testing.T) {
	plain := actionRow{ConfigJSON: `{}`}
	blue := actionRow{ConfigJSON: `{"spec_name":"颜色;尺码","spec_value":"蓝;L"}`}
	if !actionMatchesSpec(plain, false, "", "") || actionMatchesSpec(plain, false, "颜色", "蓝") || !actionMatchesSpec(plain, true, "颜色", "蓝") {
		t.Fatal("无规格动作只匹配无规格订单，除非账号级全商品规则")
	}
	if !actionMatchesSpec(blue, false, "颜色；尺码", "蓝；L") || actionMatchesSpec(blue, false, "颜色；尺码", "蓝；M") {
		t.Fatal("规格应完整相等且兼容中英文分号")
	}
	if !actionMatchesSpec(plain, false, " ", "") {
		t.Fatal("空白规格按无规格处理")
	}
	broken := actionRow{ConfigJSON: `{bad`}
	if actionMatchesSpec(broken, true, "", "") || actionMatchesSpec(broken, false, "", "") {
		t.Fatal("规格配置损坏的动作不应匹配任何订单")
	}
}

// TestHelpersFormatAPIErrorsAndPreview 覆盖错误信封解析、库存行统计和文本截断。
func TestHelpersFormatAPIErrorsAndPreview(t *testing.T) {
	if got := apiErrorMessage([]byte(`{"success":false,"message":"商品不属于当前用户"}`)); got != "商品不属于当前用户" {
		t.Fatalf("message=%q", got)
	}
	if got := apiErrorMessage([]byte(`{"code":"x","message":"已发布但本地保存失败","details":{"item_id":"p1"}}`)); got != `已发布但本地保存失败 {"item_id":"p1"}` {
		t.Fatalf("应附带恢复所需的 details: %q", got)
	}
	if got := apiErrorMessage([]byte(`{"detail":"非统一信封"}`)); got != `{"detail":"非统一信封"}` {
		t.Fatalf("非统一信封应原样返回，got %q", got)
	}
	if got := apiErrorMessage([]byte(strings.Repeat("x", 300))); len([]rune(got)) != 201 {
		t.Fatalf("非 JSON 正文应截断，len=%d", len([]rune(got)))
	}
	if lines := stockLines("a\n\n b \r\nc\rd"); len(lines) != 4 || lines[1] != "b" || lines[3] != "d" {
		t.Fatalf("lines=%q", lines)
	}
	if got := preview("一二三四五", 3); got != "一二三…" {
		t.Fatalf("preview=%q", got)
	}
	if summary := cardSummary(cardRow{ID: 1, Type: "data", DataContent: "x\ny"}); summary["stock"] != 2 || summary["data_content"] != nil {
		t.Fatalf("data 卡摘要只给库存条数: %v", summary)
	}
}
