package automation

import (
	"context"
	"testing"

	"xianyu-go/internal/db"
)

// seedSimplifiedEventFixture 写入卖家商品和一笔指定状态的订单，供简化系统事件（只含会话标识）测试复用。
func seedSimplifiedEventFixture(t *testing.T, store *db.Store, orderStatus string) {
	t.Helper()
	ctx := context.Background()
	// itemErr 保存本账号卖家商品归属事实的写入错误。
	if itemErr := store.Items.Upsert(ctx, &db.ItemInfoRow{CookieID: "cid", ItemID: "simple-item", ItemTitle: "简化事件商品"}); itemErr != nil {
		t.Fatal(itemErr)
	}
	// orderErr 保存订单事实写入错误；会话、买家、商品与简化事件一致。
	if orderErr := store.Orders.Upsert(ctx, "simple-order", db.OrderUpsertOpts{
		CookieID: "cid", ItemID: "simple-item", BuyerID: "simple-buyer", ChatID: "simple-chat",
		OrderStatus: orderStatus, Quantity: "1", Amount: "9.90",
	}); orderErr != nil {
		t.Fatal(orderErr)
	}
}

// countPendingAndRuns 返回延期任务数和自动化运行数，用于断言简化事件没有产生副作用。
func countPendingAndRuns(t *testing.T, store *db.Store) (pending, runs int) {
	t.Helper()
	ctx := context.Background()
	// queryErr 保存延期任务计数查询错误。
	if queryErr := store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM automation_pending_tasks`).Scan(&pending); queryErr != nil {
		t.Fatal(queryErr)
	}
	// queryErr 保存运行计数查询错误。
	if queryErr := store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM automation_runs`).Scan(&runs); queryErr != nil {
		t.Fatal(queryErr)
	}
	return pending, runs
}

// simplifiedPaidTask 构造只含账号和会话标识的简化付款系统事件，不含订单号、商品和角色。
func simplifiedPaidTask() Task {
	return Task{Source: "ws", AccountID: "cid", TriggerType: TriggerOrderPaid, ChatID: "simple-chat"}
}

// TestSimplifiedPaidEventAfterShippedOrderIsResolvedWithoutDeferral 验证完整事件已让订单发货后，
// 同会话的简化付款事件被安全收口：不延期、不创建运行、不重复发货、也就不会进入死信产生人工处理误报。
func TestSimplifiedPaidEventAfterShippedOrderIsResolvedWithoutDeferral(t *testing.T) {
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	seedSimplifiedEventFixture(t, store, "shipped")
	// sender 记录任何不应发生的买家消息副作用。
	sender := &testSender{}
	// center 使用真实事实记录和运行协调器。
	center := New(store, testSenderProvider{sender: sender}, nil)
	// handleErr 保存简化事件处理结果，期望为 nil。
	if handleErr := center.HandleTask(context.Background(), simplifiedPaidTask()); handleErr != nil {
		t.Fatalf("已发货订单的简化事件不应返回错误: %v", handleErr)
	}
	// pending、runs 是简化事件处理后的延期任务数与运行数。
	pending, runs := countPendingAndRuns(t, store)
	if pending != 0 || runs != 0 || len(sender.texts) != 0 {
		t.Fatalf("简化事件应安全收口: pending=%d runs=%d messages=%v", pending, runs, sender.texts)
	}
}

// TestSimplifiedPaidEventStillDeliversPendingShipOrder 验证订单仍待发货时，简化付款事件照常按会话回填并发卡（原有行为回归）。
func TestSimplifiedPaidEventStillDeliversPendingShipOrder(t *testing.T) {
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	ctx := context.Background()
	seedSimplifiedEventFixture(t, store, "pending_ship")
	// admin、adminErr 提供卡密组和规则的管理用户归属。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil || admin == nil {
		t.Fatalf("读取测试管理员失败: %v", adminErr)
	}
	// cardID、cardErr 保存本地一次性卡密组及创建错误。
	cardID, cardErr := store.Cards.Create(ctx, &db.CardFull{Name: "简化事件卡", Type: "text", TextContent: "SIMPLE-CARD", Enabled: true, UserID: admin.ID})
	if cardErr != nil {
		t.Fatal(cardErr)
	}
	// ruleErr 保存商品级付款发卡规则创建错误。
	if _, ruleErr := store.Automation.Create(ctx, db.AutomationRuleInput{UserID: admin.ID, CookieID: "cid", ItemID: "simple-item", Name: "简化事件规则", TriggerType: TriggerOrderPaid, Enabled: true,
		Actions: []db.AutomationActionInput{{ActionType: ActionSendCard, CardID: cardID, DeliveryCount: 1, Enabled: true}}}); ruleErr != nil {
		t.Fatal(ruleErr)
	}
	// sender 记录实际发出的卡密内容。
	sender := &testSender{}
	// center 使用真实事实记录和运行协调器。
	center := New(store, testSenderProvider{sender: sender}, nil)
	// handleErr 保存简化事件处理结果，期望为 nil。
	if handleErr := center.HandleTask(ctx, simplifiedPaidTask()); handleErr != nil {
		t.Fatalf("待发货订单的简化事件应正常处理: %v", handleErr)
	}
	if len(sender.texts) != 1 || sender.texts[0] != "SIMPLE-CARD" {
		t.Fatalf("简化事件未按待发货订单发卡: %v", sender.texts)
	}
}

// TestSimplifiedPaidEventWithoutRecentOrderStillDefers 验证没有任何近期订单时，简化事件仍写入延期队列等待订单同步（原有行为回归）。
func TestSimplifiedPaidEventWithoutRecentOrderStillDefers(t *testing.T) {
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// center 使用真实事实记录和运行协调器。
	center := New(store, testSenderProvider{sender: &testSender{}}, nil)
	// handleErr 保存简化事件处理结果，期望为 nil。
	if handleErr := center.HandleTask(context.Background(), simplifiedPaidTask()); handleErr != nil {
		t.Fatalf("无订单的简化事件延期不应返回错误: %v", handleErr)
	}
	// pending、runs 是延期任务数与运行数。
	pending, runs := countPendingAndRuns(t, store)
	if pending != 1 || runs != 0 {
		t.Fatalf("无订单时应延期等待同步: pending=%d runs=%d", pending, runs)
	}
}

// TestSimplifiedPaidEventOutsideWindowStillDefers 验证订单更新时间超出回溯窗口时不会被误关联，保持延期行为。
func TestSimplifiedPaidEventOutsideWindowStillDefers(t *testing.T) {
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	ctx := context.Background()
	seedSimplifiedEventFixture(t, store, "shipped")
	// execErr 保存把订单更新时间改到窗口外的写入错误。
	if _, execErr := store.DB.ExecContext(ctx, `UPDATE orders SET updated_at='2000-01-01 00:00:00' WHERE order_id=?`, "simple-order"); execErr != nil {
		t.Fatal(execErr)
	}
	// center 使用真实事实记录和运行协调器。
	center := New(store, testSenderProvider{sender: &testSender{}}, nil)
	// handleErr 保存简化事件处理结果，期望为 nil。
	if handleErr := center.HandleTask(ctx, simplifiedPaidTask()); handleErr != nil {
		t.Fatalf("窗口外简化事件延期不应返回错误: %v", handleErr)
	}
	// pending、runs 是延期任务数与运行数。
	pending, runs := countPendingAndRuns(t, store)
	if pending != 1 || runs != 0 {
		t.Fatalf("窗口外订单不应被关联: pending=%d runs=%d", pending, runs)
	}
}

// TestSimplifiedOrderCreatedResolvesToExistingOrderKey 验证简化拍下事件只在订单号和商品都缺失时按近期订单补齐，
// 补齐后的幂等键与完整事件相同，因此不会产生第二笔订单或延期任务。
func TestSimplifiedOrderCreatedResolvesToExistingOrderKey(t *testing.T) {
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	ctx := context.Background()
	seedSimplifiedEventFixture(t, store, "shipped")
	// center 使用真实事实记录和运行协调器。
	center := New(store, testSenderProvider{sender: &testSender{}}, nil)
	// simplified 是只含会话标识的拍下事件。
	simplified := Task{Source: "ws", AccountID: "cid", TriggerType: TriggerOrderCreated, ChatID: "simple-chat"}
	// resolved、resolveErr 是按会话补齐后的任务及错误。
	resolved, resolveErr := center.resolvePaidTaskOrder(ctx, simplified)
	if resolveErr != nil || resolved.OrderID != "simple-order" || resolved.ItemID != "simple-item" {
		t.Fatalf("简化拍下事件应补齐已有订单: task=%+v err=%v", resolved, resolveErr)
	}
	// got、want 分别是补齐后任务的实际幂等键和与完整事件一致的期望键。
	if got, want := buildTriggerKey(resolved), "order_created:simple-order"; got != want {
		t.Fatalf("补齐后幂等键=%q，期望 %q", got, want)
	}
	// withItem 已带商品，说明不是只含会话标识的简化事件，不应被改写。
	withItem := simplified
	withItem.ItemID = "other-item"
	// unchanged、unchangedErr 是已有商品标识的拍下事件处理结果，必须原样返回。
	unchanged, unchangedErr := center.resolvePaidTaskOrder(ctx, withItem)
	if unchangedErr != nil || unchanged.OrderID != "" {
		t.Fatalf("已带商品的拍下事件不应按会话改写: task=%+v err=%v", unchanged, unchangedErr)
	}
	// handleErr 保存简化事件处理结果，期望为 nil。
	if handleErr := center.HandleTask(ctx, simplified); handleErr != nil {
		t.Fatalf("简化拍下事件不应返回错误: %v", handleErr)
	}
	// pending 是处理后的延期任务数。
	pending, _ := countPendingAndRuns(t, store)
	if pending != 0 {
		t.Fatalf("简化拍下事件不应进入延期队列: pending=%d", pending)
	}
}
