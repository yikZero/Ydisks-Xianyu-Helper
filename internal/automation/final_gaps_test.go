package automation

import (
	"context"
	"strings"
	"testing"
	"time"

	"xianyu-go/internal/db"
)

// TestEventFactRecorderGapBranches 覆盖事件事实记录中成功小刀阶段与订单完成时间写入失败。
func TestEventFactRecorderGapBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("成功小刀阶段记录失败", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		execGapSQL(t, store, `ALTER TABLE bargain_free_shipping_stages RENAME TO bargain_stage_gap_hidden`)
		// paid 是已确认砍价的付款事件。
		paid := Task{AccountID: "cid", TriggerType: TriggerOrderPaid, OrderID: "rec-bargain", ItemID: "i", BuyerID: "b", ChatID: "c", IsBargain: true}
		// recordErr 是事件事实记录组件在本场景下返回的错误，用于断言阶段事实写入失败不被吞掉。
		if recordErr := newEventFactRecorder(store).record(ctx, paid); recordErr == nil || !strings.Contains(recordErr.Error(), "成功小刀阶段") {
			t.Fatalf("err=%v", recordErr)
		}
	})
	t.Run("订单完成时间记录失败", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		execGapSQL(t, store, `CREATE TRIGGER gap_abort_completed BEFORE UPDATE OF completed_at ON orders BEGIN SELECT RAISE(ABORT, 'forced completed_at failure'); END`)
		// done 是买家确认收货事件。
		done := Task{AccountID: "cid", TriggerType: TriggerOrderCompleted, OrderID: "rec-done", ItemID: "i", BuyerID: "b", ChatID: "c"}
		// recordErr 是事件事实记录组件在本场景下返回的错误，用于断言阶段事实写入失败不被吞掉。
		if recordErr := newEventFactRecorder(store).record(ctx, done); recordErr == nil || !strings.Contains(recordErr.Error(), "订单完成时间") {
			t.Fatalf("err=%v", recordErr)
		}
	})
	t.Run("updateKey回填订单号", func(t *testing.T) {
		// f 保存逐项补齐的事实。
		var f rawFields
		supplementEventFactByKey(&f, "updateKey", "chat-7:order-7")
		if f.chatID != "chat-7" || f.orderID != "order-7" {
			t.Fatalf("f=%+v", f)
		}
	})
}

// TestCenterSmallGapBranches 覆盖发送器就绪判断的空保护与卡密延迟读取失败。
func TestCenterSmallGapBranches(t *testing.T) {
	if (*Center)(nil).accountSenderReady("cid") || (&Center{}).accountSenderReady("cid") {
		t.Fatal("没有发送器提供方时不应就绪")
	}
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// center 使用真实卡密仓储。
	center := New(store, nil, nil)
	// delayErr 是读取卡密延迟配置失败时的错误。
	if _, delayErr := center.actionDelaySeconds(context.Background(), db.AutomationAction{ActionType: ActionSendCard, CardID: 987654}); delayErr == nil {
		t.Fatal("卡密组不存在应报错")
	}
}

// TestManualDeliveryRuleGapBranches 覆盖人工发货的账号启用状态读取、快照序列化和运行创建失败。
func TestManualDeliveryRuleGapBranches(t *testing.T) {
	t.Run("启用状态读取失败", func(t *testing.T) {
		// ctx、store、center、order、cleanup 保存人工发货夹具。
		ctx, store, center, _, _, order, cleanup := newManualDeliveryFixture(t, "gap-status")
		defer cleanup()
		execGapSQL(t, store, `DROP TABLE cookie_status`)
		// prepareErr 是人工发货输入校验与订单补全的返回错误。
		if _, prepareErr := center.prepareManualDeliveryTask(ctx, order); prepareErr == nil {
			t.Fatal("启用状态读取失败应报错")
		}
	})
	t.Run("运行快照序列化与创建失败", func(t *testing.T) {
		// ctx、store、center、order、cleanup 保存人工发货夹具。
		ctx, store, center, _, _, order, cleanup := newManualDeliveryFixture(t, "gap-start")
		defer cleanup()
		// rules、matchErr 是夹具商品命中的付款规则。
		rules, matchErr := center.rules.match(ctx, Task{AccountID: "cid", ItemID: "manual-item", TriggerType: TriggerOrderPaid})
		if matchErr != nil || len(rules) == 0 {
			t.Fatalf("rules=%v err=%v", rules, matchErr)
		}
		// task 是人工任务，Raw 中放入无法序列化的值。
		task := Task{Source: "manual", AccountID: "cid", OrderID: order.OrderID, ItemID: "manual-item", ChatID: "chat", BuyerID: "buyer", TriggerType: TriggerOrderPaid, Quantity: "1", Raw: map[string]any{"bad": make(chan int)}}
		// ruleErr 是人工发货规则执行的返回错误，用于断言快照序列化与运行创建失败被如实上报。
		if _, ruleErr := center.executeManualDeliveryRule(ctx, task, rules[0], "manual_delivery:gap"); ruleErr == nil || !strings.Contains(ruleErr.Error(), "保存完整发货运行快照") {
			t.Fatalf("err=%v", ruleErr)
		}
		task.Raw = nil
		execGapSQL(t, store, `CREATE TRIGGER gap_abort_run_insert BEFORE INSERT ON automation_runs BEGIN SELECT RAISE(ABORT, 'forced run insert failure'); END`)
		// ruleErr 是人工发货规则执行的返回错误，用于断言快照序列化与运行创建失败被如实上报。
		if _, ruleErr := center.executeManualDeliveryRule(ctx, task, rules[0], "manual_delivery:gap"); ruleErr == nil || !strings.Contains(ruleErr.Error(), "forced run insert failure") {
			t.Fatalf("err=%v", ruleErr)
		}
	})
}

// TestPendingShipDeliveryScanGapBranches 覆盖付款兜底扫描的预算、账号开关、冷却与空状态保护；兜底从不调用免拼接口。
func TestPendingShipDeliveryScanGapBranches(t *testing.T) {
	// ready 报告账号发送器就绪，使扫描能走到冷却判断。
	ready := func(account string) SenderProvider {
		return selectiveSenderProvider{senders: map[string]*readinessTestSender{}, ready: map[string]bool{account: true}}
	}
	// fixture 创建一条已越过实时窗口的待发货订单和对应规则。
	fixture := func(t *testing.T, account string) (*Scheduler, *db.Store) {
		t.Helper()
		store, cleanup := newAutomationTestStore(t)
		t.Cleanup(cleanup)
		// admin 是规则归属用户。
		admin, _ := store.Users.GetByUsername(context.Background(), "admin")
		catchupAccount(t, store, admin.ID, account)
		catchupRule(t, store, admin.ID, account, "item-gap", []db.AutomationActionInput{catchupCardAction(catchupCard(t, store, admin.ID))})
		catchupOrder(t, store, account, "o-"+account, "item-gap", "chat-"+account)
		return &Scheduler{center: New(store, ready(account), nil), pendingShipCooldown: map[string]time.Time{}}, store
	}

	t.Run("入口前预算已耗尽", func(t *testing.T) {
		// s 保存调度器。
		s, _ := fixture(t, "scan-a")
		// canceled 是已取消上下文。
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		// left 是扫描结束后尚未消耗的待发货任务额度。
		if left := s.scanPendingShipDeliveriesWithContextAndLimit(canceled, 3); left != 3 {
			t.Fatalf("left=%d", left)
		}
	})
	t.Run("空上下文直接返回", func(t *testing.T) {
		// s 保存调度器。
		s, _ := fixture(t, "scan-b")
		// nilCtx 是未提供的上下文。
		var nilCtx context.Context
		s.scanPendingShipDeliveries(nilCtx)
		s.scanPendingShipResumes(nilCtx)
	})
	t.Run("自动发货总开关关闭时不领取冷却", func(t *testing.T) {
		// s、store 保存调度器与数据库。
		s, store := fixture(t, "scan-c")
		execGapSQL(t, store, `UPDATE cookies SET auto_confirm=0 WHERE id='scan-c'`)
		s.scanPendingShipDeliveriesWithContextAndLimit(context.Background(), 3)
		s.pendingShipMu.Lock()
		// cooled 表示订单是否被占用冷却窗口。
		_, cooled := s.pendingShipCooldown["o-scan-c"]
		s.pendingShipMu.Unlock()
		if cooled {
			t.Fatal("开关关闭不得占用冷却窗口")
		}
	})
	t.Run("账号状态读取失败时不处理", func(t *testing.T) {
		// s、store 保存调度器与数据库。
		s, store := fixture(t, "scan-d")
		execGapSQL(t, store, `DROP TABLE cookie_status`)
		s.scanPendingShipDeliveriesWithContextAndLimit(context.Background(), 3)
	})
	t.Run("冷却窗口内的订单被跳过", func(t *testing.T) {
		// s 保存调度器，订单已预占冷却。
		s, store := fixture(t, "scan-e")
		s.pendingShipCooldown["o-scan-e"] = time.Now()
		s.scanPendingShipDeliveriesWithContextAndLimit(context.Background(), 3)
		// runs 是订单产生的付款运行数，冷却内应为 0。
		var runs int
		// scanErr 是读取数据库统计值时的扫描错误，应为空以保证断言可信。
		if scanErr := store.DB.QueryRow(`SELECT COUNT(*) FROM automation_runs WHERE order_id='o-scan-e'`).Scan(&runs); scanErr != nil || runs != 0 {
			t.Fatalf("runs=%d err=%v", runs, scanErr)
		}
	})
	t.Run("零值调度器延迟初始化冷却表", func(t *testing.T) {
		// s 没有冷却表。
		s := &Scheduler{}
		if !s.claimPendingShipAttempt("o-zero") || s.pendingShipCooldown == nil {
			t.Fatal("应延迟创建冷却表并领取")
		}
	})
}
