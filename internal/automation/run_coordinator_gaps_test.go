package automation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"xianyu-go/internal/db"
)

// coordGap 汇总运行协调器缺口测试的夹具：数据库、规则、运行快照与可注入外部动作的协调器。
type coordGap struct {
	// store 是隔离测试数据库，测试通过它安装故障触发器并核对运行终态。
	store *db.Store
	// ruleID 是夹具规则主键，运行必须引用它。
	ruleID int64
	// runID 是已创建并处于 running 的运行主键。
	runID int64
	// run 是该运行的最新快照，携带代次与动作游标。
	run *db.AutomationRun
	// coord 是注入了外部动作替身的运行协调器。
	coord automationRunCoordinator
	// task 是与规则动作规格匹配的付款发货任务。
	task Task
}

// newCoordGap 创建 send_card + confirm_shipment 规则及 running 运行，exec 决定每个动作的外部结果。
func newCoordGap(t *testing.T, exec func(context.Context, Task, db.AutomationAction) (actionExecutionResult, error)) *coordGap {
	t.Helper()
	store, cleanup := newAutomationTestStore(t)
	t.Cleanup(cleanup)
	// admin、adminErr 是规则归属用户。
	admin, adminErr := store.Users.GetByUsername(context.Background(), "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleID 是 send_card + confirm_shipment 两步付款发货规则。
	ruleID := uncertainTailRule(t, store, admin.ID, "gap-acc", []db.AutomationActionInput{
		{ActionType: ActionSendCard, DeliveryCount: 1, Enabled: true, ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`},
		{ActionType: ActionConfirmShipment, Enabled: true},
	})
	// runID 是新建运行主键。
	runID := uncertainTailRun(t, store, ruleID, "gap-acc", "o-gap")
	return &coordGap{
		store: store, ruleID: ruleID, runID: runID,
		run:   uncertainTailRunningRun(t, store, runID),
		coord: newUncertainTailCoordinator(store, exec),
		task: Task{TriggerType: TriggerOrderPaid, AccountID: "gap-acc", OrderID: "o-gap", ChatID: "chat-gap", BuyerID: "buyer-gap", ItemID: "item-uncertain",
			SpecName: "颜色", SpecValue: "黑"},
	}
}

// plan 返回两步动作计划：发卡后确认发货。
func (g *coordGap) plan() []db.AutomationAction {
	return []db.AutomationAction{
		{ActionType: ActionSendCard, Enabled: true, CardID: 1, DeliveryCount: 1, ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`},
		{ActionType: ActionConfirmShipment, Enabled: true},
	}
}

// okExec 让所有外部动作确定成功，发卡动作计 1 条已送达。
func okExec(_ context.Context, _ Task, action db.AutomationAction) (actionExecutionResult, error) {
	if action.ActionType == ActionSendCard {
		return actionExecutionResult{sent: 1}, nil
	}
	return actionExecutionResult{}, nil
}

// TestExecuteRunActionLoopGapBranches 覆盖动作循环的延迟、检查点、隔离写入失败与门禁分支。
func TestExecuteRunActionLoopGapBranches(t *testing.T) {
	ctx := context.Background()
	// abortReview 让运行进入 needs_review 的写入失败，模拟人工核对状态无法落库。
	const abortReview = `CREATE TRIGGER gap_abort_review BEFORE UPDATE ON automation_runs WHEN NEW.status='needs_review' BEGIN SELECT RAISE(ABORT, 'forced review write failure'); END`
	// abortCursor 让检查点游标推进失败，模拟动作已执行但检查点保存失败。
	const abortCursor = `CREATE TRIGGER gap_abort_cursor BEFORE UPDATE OF action_cursor ON automation_runs WHEN NEW.action_cursor<>OLD.action_cursor BEGIN SELECT RAISE(ABORT, 'forced cursor failure'); END`
	// uncertainExec 让发卡动作返回送达结果不确定，其余动作成功。
	uncertainExec := func(_ context.Context, _ Task, action db.AutomationAction) (actionExecutionResult, error) {
		if action.ActionType == ActionSendCard {
			return actionExecutionResult{}, uncertainAction(errors.New("响应中断"))
		}
		return actionExecutionResult{}, nil
	}

	t.Run("延迟秒数读取失败", func(t *testing.T) {
		// g 保存夹具。
		g := newCoordGap(t, okExec)
		g.coord.actionDelaySeconds = func(context.Context, db.AutomationAction) (int, error) { return 0, errors.New("卡组不可读") }
		// loopErr 是动作循环对本场景故障注入的返回结果，用于断言是否转人工核对或被拒绝。
		if _, _, loopErr := g.coord.executeRunActions(ctx, g.task, g.ruleID, g.run, g.plan(), false); loopErr == nil {
			t.Fatal("应返回延迟读取错误")
		}
	})
	t.Run("延迟续租与持久化失败", func(t *testing.T) {
		// g 保存夹具；所有动作延迟 30 秒。
		g := newCoordGap(t, okExec)
		g.coord.actionDelaySeconds = func(context.Context, db.AutomationAction) (int, error) { return 30, nil }
		g.coord.deferTask = func(context.Context, Task, int64) error { return errors.New("延迟队列不可写") }
		// deferred 表示任务是否被持久化延期等待后续重放；loopErr 是动作循环对本场景故障注入的返回结果，用于断言是否转人工核对或被拒绝。
		if _, deferred, loopErr := g.coord.executeRunActions(ctx, g.task, g.ruleID, g.run, g.plan(), false); deferred || loopErr == nil || !strings.Contains(loopErr.Error(), "延迟队列不可写") {
			t.Fatalf("deferred=%v err=%v", deferred, loopErr)
		}
		execGapSQL(t, g.store, `CREATE TRIGGER gap_abort_lease BEFORE UPDATE OF lease_expires_at ON automation_runs BEGIN SELECT RAISE(ABORT, 'forced lease failure'); END`)
		// loopErr 是动作循环对本场景故障注入的返回结果，用于断言是否转人工核对或被拒绝。
		if _, _, loopErr := g.coord.executeRunActions(ctx, g.task, g.ruleID, g.run, g.plan(), false); loopErr == nil || !strings.Contains(loopErr.Error(), "forced lease failure") {
			t.Fatalf("err=%v", loopErr)
		}
	})
	t.Run("动作检查点被其他worker占用", func(t *testing.T) {
		// g 保存夹具；快照代次与数据库不一致表示已被抢占。
		g := newCoordGap(t, okExec)
		// stale 是代次落后的运行快照。
		stale := *g.run
		stale.AttemptCount += 7
		// loopErr 是动作循环对本场景故障注入的返回结果，用于断言是否转人工核对或被拒绝。
		if _, _, loopErr := g.coord.executeRunActions(ctx, g.task, g.ruleID, &stale, g.plan(), true); loopErr == nil {
			t.Fatal("应报告动作被其他 worker 领取或失权")
		}
	})
	t.Run("占用检查点写入失败", func(t *testing.T) {
		// g 保存夹具。
		g := newCoordGap(t, okExec)
		execGapSQL(t, g.store, `CREATE TRIGGER gap_abort_start BEFORE UPDATE ON automation_runs WHEN NEW.action_started=1 BEGIN SELECT RAISE(ABORT, 'forced start failure'); END`)
		// loopErr 是动作循环对本场景故障注入的返回结果，用于断言是否转人工核对或被拒绝。
		if _, _, loopErr := g.coord.executeRunActions(ctx, g.task, g.ruleID, g.run, g.plan(), true); loopErr == nil || !strings.Contains(loopErr.Error(), "forced start failure") {
			t.Fatalf("err=%v", loopErr)
		}
	})
	t.Run("明确未执行但清理占用失败再隔离失败", func(t *testing.T) {
		// g 保存夹具；发卡动作明确未发送。
		g := newCoordGap(t, func(context.Context, Task, db.AutomationAction) (actionExecutionResult, error) {
			return actionExecutionResult{}, fmt.Errorf("%w: 离线", ErrMessageNotSent)
		})
		execGapSQL(t, g.store, `CREATE TRIGGER gap_abort_abort BEFORE UPDATE ON automation_runs WHEN OLD.action_started=1 AND NEW.action_started=0 AND NEW.status<>'needs_review' BEGIN SELECT RAISE(ABORT, 'forced abort failure'); END`)
		execGapSQL(t, g.store, abortReview)
		// loopErr 是动作循环对本场景故障注入的返回结果，用于断言是否转人工核对或被拒绝。
		if _, _, loopErr := g.coord.executeRunActions(ctx, g.task, g.ruleID, g.run, g.plan(), true); !errors.Is(loopErr, errAutomationNeedsReview) {
			t.Fatalf("err=%v", loopErr)
		}
	})
	t.Run("结果不确定且人工核对写入失败", func(t *testing.T) {
		// g 保存夹具；只剩发卡动作，无法放行。
		g := newCoordGap(t, uncertainExec)
		execGapSQL(t, g.store, abortReview)
		// single 只含发卡动作。
		single := g.plan()[:1]
		// loopErr 是动作循环对本场景故障注入的返回结果，用于断言是否转人工核对或被拒绝。
		if _, _, loopErr := g.coord.executeRunActions(ctx, g.task, g.ruleID, g.run, single, true); !errors.Is(loopErr, errAutomationQuarantine) {
			t.Fatalf("err=%v", loopErr)
		}
	})
	t.Run("检查点推进失败后隔离成功与失败", func(t *testing.T) {
		// g 保存夹具。
		g := newCoordGap(t, okExec)
		execGapSQL(t, g.store, abortCursor)
		// loopErr 是动作循环对本场景故障注入的返回结果，用于断言是否转人工核对或被拒绝。
		if _, _, loopErr := g.coord.executeRunActions(ctx, g.task, g.ruleID, g.run, g.plan(), true); !errors.Is(loopErr, errAutomationNeedsReview) || errors.Is(loopErr, errAutomationQuarantine) {
			t.Fatalf("隔离成功时 err=%v", loopErr)
		}
		// h 是第二个夹具，隔离写入也失败。
		h := newCoordGap(t, okExec)
		execGapSQL(t, h.store, abortCursor)
		execGapSQL(t, h.store, abortReview)
		// loopErr 是动作循环对本场景故障注入的返回结果，用于断言是否转人工核对或被拒绝。
		if _, _, loopErr := h.coord.executeRunActions(ctx, h.task, h.ruleID, h.run, h.plan(), true); !errors.Is(loopErr, errAutomationQuarantine) {
			t.Fatalf("隔离失败时 err=%v", loopErr)
		}
	})
	t.Run("放行幂等尾部时检查点推进失败", func(t *testing.T) {
		// g 保存夹具；发卡不确定后推进检查点失败，应退回隔离。
		g := newCoordGap(t, uncertainExec)
		execGapSQL(t, g.store, abortCursor)
		// loopErr 是动作循环对本场景故障注入的返回结果，用于断言是否转人工核对或被拒绝。
		if _, _, loopErr := g.coord.executeRunActions(ctx, g.task, g.ruleID, g.run, g.plan(), true); !errors.Is(loopErr, errAutomationNeedsReview) {
			t.Fatalf("err=%v", loopErr)
		}
	})
	t.Run("放行尾部收口时人工核对写入失败", func(t *testing.T) {
		// g 保存夹具；发卡不确定，确认发货被放行，最终收口隔离失败。
		g := newCoordGap(t, uncertainExec)
		execGapSQL(t, g.store, abortReview)
		// loopErr 是动作循环对本场景故障注入的返回结果，用于断言是否转人工核对或被拒绝。
		if _, _, loopErr := g.coord.executeRunActions(ctx, g.task, g.ruleID, g.run, g.plan(), true); !errors.Is(loopErr, errAutomationQuarantine) {
			t.Fatalf("err=%v", loopErr)
		}
	})
	t.Run("动作执行前账号门禁", func(t *testing.T) {
		// g 保存夹具。
		g := newCoordGap(t, okExec)
		g.coord.accountAutomationAllowed = func(context.Context, string) (bool, error) { return false, errors.New("账号状态不可读") }
		// gateErr 是动作执行前账号门禁返回的错误，用于断言暂停或读取失败时不触达外部系统。
		if _, gateErr := g.coord.executeActionNow(ctx, g.task, g.plan()[0], shipmentDeliveryProof{}); gateErr == nil {
			t.Fatal("门禁读取失败应报错")
		}
		g.coord.accountAutomationAllowed = func(context.Context, string) (bool, error) { return false, nil }
		// gateErr 是动作执行前账号门禁返回的错误，用于断言暂停或读取失败时不触达外部系统。
		if _, gateErr := g.coord.executeActionNow(ctx, g.task, g.plan()[0], shipmentDeliveryProof{}); gateErr == nil || !strings.Contains(gateErr.Error(), "暂停或停用") {
			t.Fatalf("err=%v", gateErr)
		}
	})
	t.Run("待核对标记写入空任务安全返回", func(t *testing.T) {
		markPendingUncertainty(nil, "原因", errors.New("x"))
	})
}

// TestExecuteRuleGapBranches 覆盖 executeRule 的跳过、恢复、部分成功隔离与求评价计数失败分支。
func TestExecuteRuleGapBranches(t *testing.T) {
	ctx := context.Background()
	// abortReview 让运行进入 needs_review 的写入失败。
	const abortReview = `CREATE TRIGGER gap_abort_review BEFORE UPDATE ON automation_runs WHEN NEW.status='needs_review' BEGIN SELECT RAISE(ABORT, 'forced review write failure'); END`

	t.Run("缺少触发键时跳过", func(t *testing.T) {
		// g 保存夹具。
		g := newCoordGap(t, okExec)
		// rule 是夹具规则。
		rule, ruleErr := g.store.Automation.Get(ctx, g.ruleID)
		if ruleErr != nil {
			t.Fatal(ruleErr)
		}
		// execErr 是 executeRule 对本场景的执行结果，用于断言运行是否进入人工核对或被跳过。
		if execErr := g.coord.executeRule(ctx, Task{AccountID: "gap-acc", TriggerType: TriggerBuyerReviewed}, *rule); execErr != nil {
			t.Fatalf("缺触发键应静默跳过: %v", execErr)
		}
	})
	t.Run("恢复运行不存在或已非执行中", func(t *testing.T) {
		// g 保存夹具。
		g := newCoordGap(t, okExec)
		// rule 是夹具规则。
		rule, _ := g.store.Automation.Get(ctx, g.ruleID)
		// resume 是携带不存在运行号的恢复任务。
		resume := g.task
		resume.Raw = map[string]any{"automation_run_id": int64(987654)}
		// execErr 是 executeRule 对本场景的执行结果，用于断言运行是否进入人工核对或被跳过。
		if execErr := g.coord.executeRule(ctx, resume, *rule); execErr == nil {
			t.Fatal("运行不存在应报错")
		}
		execGapSQL(t, g.store, `UPDATE automation_runs SET status='failed' WHERE id=`+itoa(g.runID))
		resume.Raw = map[string]any{"automation_run_id": g.runID}
		// execErr 是 executeRule 对本场景的执行结果，用于断言运行是否进入人工核对或被跳过。
		if execErr := g.coord.executeRule(ctx, resume, *rule); execErr != nil {
			t.Fatalf("非 running 运行应跳过: %v", execErr)
		}
	})
	t.Run("部分成功后失败转人工核对", func(t *testing.T) {
		// calls 记录已执行动作序号，使第一个发卡成功、第二个失败。
		calls := 0
		// g 保存夹具。
		g := newCoordGap(t, func(_ context.Context, _ Task, action db.AutomationAction) (actionExecutionResult, error) {
			calls++
			if calls == 1 {
				return actionExecutionResult{sent: 1}, nil
			}
			return actionExecutionResult{}, errors.New("库存读取失败")
		})
		// notified 记录收到的人工核对通知。
		var notified []string
		g.coord.hasNotifier = func() bool { return true }
		g.coord.notifyResult = func(_ context.Context, _ Task, _ int64, status string, _ int, _ string) {
			notified = append(notified, status)
		}
		// rule 是两个发卡动作的付款规则。
		rule := db.AutomationRule{ID: g.ruleID, TriggerType: TriggerOrderPaid, ItemID: "item-uncertain", Actions: []db.AutomationAction{
			{ActionType: ActionSendCard, Enabled: true, CardID: 1, DeliveryCount: 1, ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`},
			{ActionType: ActionSendCard, Enabled: true, CardID: 1, DeliveryCount: 1, ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`},
		}}
		// fresh 使用新的订单号避免与夹具运行冲突。
		fresh := g.task
		fresh.OrderID = "o-partial"
		// execErr 是 executeRule 对本场景的执行结果，用于断言运行是否进入人工核对或被跳过。
		if execErr := g.coord.executeRule(ctx, fresh, rule); !errors.Is(execErr, errAutomationNeedsReview) {
			t.Fatalf("err=%v", execErr)
		}
		if len(notified) != 1 || notified[0] != "needs_review" {
			t.Fatalf("notified=%v", notified)
		}
	})
	t.Run("部分成功后人工核对写入失败", func(t *testing.T) {
		// calls 记录已执行动作序号。
		calls := 0
		// g 保存夹具。
		g := newCoordGap(t, func(context.Context, Task, db.AutomationAction) (actionExecutionResult, error) {
			calls++
			if calls == 1 {
				return actionExecutionResult{sent: 1}, nil
			}
			return actionExecutionResult{}, errors.New("库存读取失败")
		})
		execGapSQL(t, g.store, abortReview)
		// rule 是两个发卡动作的付款规则。
		rule := db.AutomationRule{ID: g.ruleID, TriggerType: TriggerOrderPaid, ItemID: "item-uncertain", Actions: []db.AutomationAction{
			{ActionType: ActionSendCard, Enabled: true, CardID: 1, DeliveryCount: 1, ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`},
			{ActionType: ActionSendCard, Enabled: true, CardID: 1, DeliveryCount: 1, ConfigJSON: `{"spec_name":"颜色","spec_value":"黑"}`},
		}}
		// fresh 使用新的订单号。
		fresh := g.task
		fresh.OrderID = "o-partial-2"
		// execErr 是 executeRule 对本场景的执行结果，用于断言运行是否进入人工核对或被跳过。
		if execErr := g.coord.executeRule(ctx, fresh, rule); !errors.Is(execErr, errAutomationQuarantine) {
			t.Fatalf("err=%v", execErr)
		}
	})
	t.Run("求评价消息已发但计数保存失败", func(t *testing.T) {
		// withQuarantineFailure 表示本轮是否同时让人工核对状态写入失败，以覆盖双重失败分支。
		for _, withQuarantineFailure := range []bool{false, true} {
			// g 保存夹具。
			g := newCoordGap(t, okExec)
			// notified 记录通知状态。
			var notified []string
			g.coord.hasNotifier = func() bool { return true }
			g.coord.notifyResult = func(_ context.Context, _ Task, _ int64, status string, _ int, _ string) {
				notified = append(notified, status)
			}
			execGapSQL(t, g.store, `INSERT INTO orders (order_id, cookie_id, item_id, buyer_id, chat_id, system_shipped, shipped_at, review_request_count)
				VALUES ('o-review-gap', 'gap-acc', 'item-uncertain', 'b', 'c', 1, '`+time.Now().UTC().Add(-2*time.Hour).Format("2006-01-02 15:04:05")+`', 0)`)
			execGapSQL(t, g.store, `CREATE TRIGGER gap_abort_orders BEFORE UPDATE OF review_request_count ON orders WHEN NEW.review_request_count>OLD.review_request_count BEGIN SELECT RAISE(FAIL, 'forced counter failure'); END`)
			if withQuarantineFailure {
				execGapSQL(t, g.store, abortReview)
			}
			// rule 是求评价规则，单个文本动作确定成功。
			rule := db.AutomationRule{ID: g.ruleID, TriggerType: TriggerReviewMissingTimeout, Actions: []db.AutomationAction{{ActionType: ActionSendText, Enabled: true, MessageTemplate: "请评价"}}}
			// review 是求评价任务。
			review := Task{Source: "scheduler", TriggerType: TriggerReviewMissingTimeout, AccountID: "gap-acc", OrderID: "o-review-gap", ChatID: "c", BuyerID: "b", ItemID: "item-uncertain", Raw: map[string]any{"attempt": 1}}
			// execErr 保存执行结果。
			execErr := g.coord.executeRule(ctx, review, rule)
			if withQuarantineFailure {
				if !errors.Is(execErr, errAutomationQuarantine) {
					t.Fatalf("隔离失败时 err=%v", execErr)
				}
				continue
			}
			if !errors.Is(execErr, errAutomationNeedsReview) || len(notified) != 1 {
				t.Fatalf("err=%v notified=%v", execErr, notified)
			}
		}
	})
}
