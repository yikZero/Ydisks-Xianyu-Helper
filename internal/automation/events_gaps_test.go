package automation

import "testing"

// TestEventFactsGapBranches 覆盖事件事实提取与角色判定中尚未触达的防御分支。
func TestEventFactsGapBranches(t *testing.T) {
	t.Run("提醒链接补齐会话号", func(t *testing.T) {
		// raw 只在 message["4"] 中给出带 sid 的提醒链接，会话号应由链接回填。
		raw := map[string]any{"4": map[string]any{"reminderUrl": "https://example.invalid/order?sid=chat-1&itemId=i1&peerUserId=b1"}}
		// f 保存解析出的事实。
		f := fieldsFromRaw(raw)
		if f.chatID != "chat-1" || f.itemID != "i1" || f.buyerID != "b1" {
			t.Fatalf("链接回填异常: %+v", f)
		}
	})
	t.Run("空指针保护与非法卡片", func(t *testing.T) {
		mergeNoticeFields(nil, map[string]any{"a": 1})
		// f 是用于验证空通知不会改写字段的事实。
		f := rawFields{text: "keep"}
		mergeNoticeFields(&f, nil)
		if f.text != "keep" {
			t.Fatal("空通知不应改写字段")
		}
		// title、button 保存非法 JSON 卡片的解析结果。
		title, button := extractCardSignals("{broken")
		if title != "" || button != "" {
			t.Fatalf("非法卡片应返回空: %q %q", title, button)
		}
		supplementEventFactByKey(nil, "orderid", "1")
	})
	t.Run("按字段名补齐会话与任务名", func(t *testing.T) {
		// f 保存逐项补齐的事实。
		var f rawFields
		supplementEventFactByKey(&f, "sessionId", "chat-9@goofish")
		if f.chatID != "chat-9" {
			t.Fatalf("会话号=%q", f.chatID)
		}
		supplementEventFactByKey(&f, "bizTag", `{"taskName":"买家已付款"}`)
		if f.taskName == "" {
			t.Fatalf("任务名未补齐: %+v", f)
		}
		supplementEventFactByKey(&f, "orderid", 12345)
	})
	t.Run("买家角色不触发卖家事件", func(t *testing.T) {
		// f 是买家侧交易卡片。
		f := rawFields{orderRole: "buyer", reminderContent: "[我已付款，等待你发货]"}
		if isOrderPaidEvent(f) || isOrderCreatedEvent(f) {
			t.Fatal("买家角色不得进入卖家自动化")
		}
	})
	t.Run("链接角色解析失败", func(t *testing.T) {
		if got := orderRoleFromURL("http://[::1"); got != "" {
			t.Fatalf("非法链接应返回空角色: %q", got)
		}
	})
}
