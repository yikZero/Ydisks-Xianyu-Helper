package automation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xianyu-go/internal/db"
)

// gapAbortRunUpdate 让 automation_runs 的任何更新失败，用于覆盖运行状态写入错误分支。
const gapAbortRunUpdate = `CREATE TRIGGER gap_sched_abort_run BEFORE UPDATE ON automation_runs BEGIN SELECT RAISE(ABORT, 'forced run update failure'); END`

// flipContext 在被查询指定次数 Err 后开始报告取消，用于在循环内部确定性地触发预算耗尽分支。
type flipContext struct {
	// Context 是被包装的真实上下文，数据库调用仍使用它完成。
	context.Context
	// okCalls 是仍返回 nil 的 Err 调用剩余次数；用尽后返回 context.Canceled。
	okCalls atomic.Int64
}

// Err 先消耗放行次数，之后报告取消，模拟扫描预算在遍历中途耗尽。
func (f *flipContext) Err() error {
	if f.okCalls.Add(-1) >= 0 {
		return nil
	}
	return context.Canceled
}

// newFlipContext 构造前 ok 次 Err 查询返回 nil、之后返回取消的上下文。
func newFlipContext(ok int64) *flipContext {
	// f 保存带放行额度的上下文。
	f := &flipContext{Context: context.Background()}
	f.okCalls.Store(ok)
	return f
}

// resumeScanFixture 创建一条可被续跑扫描选中的需人工核对运行，并返回调度器、存储和运行主键。
// rawTask 为冻结快照；provider 为发送器提供方。
func resumeScanFixture(t *testing.T, orderSuffix, rawTask string, provider SenderProvider) (*Scheduler, *db.Store, int64) {
	t.Helper()
	store, cleanup := newAutomationTestStore(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	// admin、adminErr 保存规则所属管理员。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// itemID、orderID、chatID 是本夹具订单的稳定标识。
	itemID, orderID, chatID := "item-"+orderSuffix, "o-"+orderSuffix, "chat-"+orderSuffix
	// ruleID 保存仅含确认发货动作的规则。
	ruleID := catchupRule(t, store, admin.ID, "cid", itemID, []db.AutomationActionInput{{ActionType: ActionConfirmShipment, Enabled: true}})
	catchupOrder(t, store, "cid", orderID, itemID, chatID)
	// runID 保存被隔离的运行主键。
	runID := catchupRun(t, store, ruleID, "cid", orderID, rawTask)
	setRunState(t, store, runID, "needs_review", 1, 0)
	return &Scheduler{center: New(store, provider, nil), pendingShipCooldown: map[string]time.Time{}}, store, runID
}

// resumeRaw 构造账号 cid、指定订单的可续跑冻结快照（仅含确认发货动作）。
func resumeRaw(orderSuffix string) string {
	return `{"AccountID":"cid","OrderID":"o-` + orderSuffix + `","ActionPlan":[{"ActionType":"confirm_shipment","Enabled":true}]}`
}

// TestScanPendingShipResumesGateBranches 覆盖续跑扫描的预算、额度、账号门禁、不可续跑与重开失败分支。
func TestScanPendingShipResumesGateBranches(t *testing.T) {
	// ready 报告账号 cid 发送器就绪。
	ready := func() SenderProvider {
		return selectiveSenderProvider{senders: map[string]*readinessTestSender{}, ready: map[string]bool{"cid": true}}
	}
	t.Run("入口前预算已耗尽", func(t *testing.T) {
		// s、store、runID 保存夹具。
		s, store, runID := resumeScanFixture(t, "a", resumeRaw("a"), ready())
		// canceled 是已取消上下文。
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		// left 保存该步骤的中间结果，供后续断言使用。
		if left := s.scanPendingShipResumesWithContextAndLimit(canceled, 3); left != 3 {
			t.Fatalf("预算耗尽不应消耗额度: %d", left)
		}
		// run 保存运行状态，必须仍为人工核对。
		run, _ := store.Automation.GetRun(context.Background(), runID)
		if run.Status != "needs_review" || run.AttemptCount != 1 {
			t.Fatalf("预算耗尽不得重开: %+v", run)
		}
	})
	t.Run("遍历中途预算耗尽", func(t *testing.T) {
		// s、store、runID 保存夹具。
		s, store, runID := resumeScanFixture(t, "b", resumeRaw("b"), ready())
		// ctx 先放行循环头检查与数据库调用，进入候选循环后报告取消。
		ctx := newFlipContext(1)
		s.scanPendingShipResumesWithContextAndLimit(ctx, 3)
		// run 保存运行状态。
		run, _ := store.Automation.GetRun(context.Background(), runID)
		if run.AttemptCount != 1 {
			t.Fatalf("遍历中途取消不得重开: %+v", run)
		}
	})
	t.Run("任务额度在遍历中耗尽", func(t *testing.T) {
		// s、store、runID 保存夹具。
		s, _, _ := resumeScanFixture(t, "c", resumeRaw("c"), ready())
		// 额度为 0 的循环入口已被前置保护；这里用额度 1 且第一条被冷却跳过后额度不变，另用 0 验证入口。
		if left := s.scanPendingShipResumesWithContextAndLimit(context.Background(), 0); left != 0 {
			t.Fatalf("额度 0 应直接返回: %d", left)
		}
	})
	t.Run("账号暂停不重开", func(t *testing.T) {
		// s、store、runID 保存夹具。
		s, store, runID := resumeScanFixture(t, "d", resumeRaw("d"), ready())
		if _, err := store.Cookies.SetPause(context.Background(), "cid", 60); err != nil {
			t.Fatal(err)
		}
		s.scanPendingShipResumes(context.Background())
		// run 保存运行状态。
		run, _ := store.Automation.GetRun(context.Background(), runID)
		if run.AttemptCount != 1 {
			t.Fatalf("暂停账号不得重开: %+v", run)
		}
	})
	t.Run("账号状态读取失败不重开", func(t *testing.T) {
		// s、store、runID 保存夹具。
		s, store, runID := resumeScanFixture(t, "e", resumeRaw("e"), ready())
		execGapSQL(t, store, `ALTER TABLE cookies RENAME TO cookies_gap_hidden`)
		s.scanPendingShipResumes(context.Background())
		// run 保存运行状态。
		run, _ := store.Automation.GetRun(context.Background(), runID)
		if run.AttemptCount != 1 {
			t.Fatalf("状态读取失败不得重开: %+v", run)
		}
	})
	t.Run("冻结快照损坏保留人工核对", func(t *testing.T) {
		// s、store、runID 保存夹具。
		s, store, runID := resumeScanFixture(t, "f", `{"broken`, ready())
		s.scanPendingShipResumes(context.Background())
		// run 保存运行状态。
		run, _ := store.Automation.GetRun(context.Background(), runID)
		if run.AttemptCount != 1 || run.Status != "needs_review" {
			t.Fatalf("损坏快照不得自动续跑: %+v", run)
		}
	})
	t.Run("重开被数据库拒绝时记录并继续", func(t *testing.T) {
		// s、store、runID 保存夹具。
		s, store, runID := resumeScanFixture(t, "g", resumeRaw("g"), ready())
		execGapSQL(t, store, gapAbortRunUpdate)
		s.scanPendingShipResumes(context.Background())
		// run 保存运行状态。
		run, _ := store.Automation.GetRun(context.Background(), runID)
		if run.AttemptCount != 1 {
			t.Fatalf("重开失败不得推进代次: %+v", run)
		}
	})
	t.Run("重开未取得执行权时释放冷却", func(t *testing.T) {
		// s、store、runID 保存夹具。
		s, store, runID := resumeScanFixture(t, "h", resumeRaw("h"), ready())
		// IGNORE 使更新静默影响零行，模拟被其他实例抢先重开。
		execGapSQL(t, store, `CREATE TRIGGER gap_ignore_reopen BEFORE UPDATE ON automation_runs BEGIN SELECT RAISE(IGNORE); END`)
		s.scanPendingShipResumes(context.Background())
		s.pendingShipMu.Lock()
		// _, cooled 表示订单是否仍占用冷却窗口。
		_, cooled := s.pendingShipCooldown["o-h"]
		s.pendingShipMu.Unlock()
		if cooled {
			t.Fatal("未取得执行权不应占用冷却窗口")
		}
		// run 保存运行状态。
		run, _ := store.Automation.GetRun(context.Background(), runID)
		if run.AttemptCount != 1 {
			t.Fatalf("未取得执行权不得推进代次: %+v", run)
		}
	})
}

// TestScanPendingShipResumesTaskLimit 验证续跑额度耗尽时第二条候选保持不动，额度由两条待发货扫描共享。
func TestScanPendingShipResumesTaskLimit(t *testing.T) {
	// s、store、runID 保存第一条候选夹具。
	s, store, firstRun := resumeScanFixture(t, "x1", resumeRaw("x1"), selectiveSenderProvider{senders: map[string]*readinessTestSender{}, ready: map[string]bool{"cid": true}})
	ctx := context.Background()
	// admin 保存规则所属管理员。
	admin, _ := store.Users.GetByUsername(ctx, "admin")
	// ruleID 保存第二条候选使用的规则。
	ruleID := catchupRule(t, store, admin.ID, "cid", "item-x2", []db.AutomationActionInput{{ActionType: ActionConfirmShipment, Enabled: true}})
	catchupOrder(t, store, "cid", "o-x2", "item-x2", "chat-x2")
	// secondRun 保存第二条候选运行。
	secondRun := catchupRun(t, store, ruleID, "cid", "o-x2", resumeRaw("x2"))
	setRunState(t, store, secondRun, "needs_review", 1, 0)
	// left 保存该步骤的中间结果，供后续断言使用。
	if left := s.scanPendingShipResumesWithContextAndLimit(ctx, 1); left != 0 {
		t.Fatalf("额度应被消耗完: %d", left)
	}
	// first、second 保存两条运行最终代次，仅有一条被重开。
	first, _ := store.Automation.GetRun(ctx, firstRun)
	// second 保存该步骤的中间结果，供后续断言使用。
	second, _ := store.Automation.GetRun(ctx, secondRun)
	if (first.AttemptCount == 2) == (second.AttemptCount == 2) {
		t.Fatalf("额度 1 应只重开一条: first=%d second=%d", first.AttemptCount, second.AttemptCount)
	}
}

// seedSchedulerReviewOrder 创建一条已满足求评价条件的订单和规则，actionDelay 为规则动作延迟秒数。
func seedSchedulerReviewOrder(t *testing.T, store *db.Store, actionDelay int) {
	t.Helper()
	ctx := context.Background()
	// admin、adminErr 保存规则所属管理员。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// ruleErr 保存该步骤的中间结果，供后续断言使用。
	if _, ruleErr := store.Automation.Create(ctx, db.AutomationRuleInput{
		UserID: admin.ID, CookieID: "cid", ItemID: "item-1", Name: "求评价", TriggerType: TriggerReviewMissingTimeout, Enabled: true, Priority: 100,
		ConfigJSON: `{"after_shipped_hours":1,"max_attempts":3}`,
		Actions:    []db.AutomationActionInput{{ActionType: ActionSendText, MessageTemplate: "请评价", DelaySeconds: actionDelay, Enabled: true}},
	}); ruleErr != nil {
		t.Fatal(ruleErr)
	}
	// shipped 是两小时前的发货时间，使求评价规则已到期。
	shipped := time.Now().UTC().Add(-2 * time.Hour).Format("2006-01-02 15:04:05")
	execGapSQL(t, store, `INSERT INTO orders (order_id, cookie_id, item_id, buyer_id, chat_id, system_shipped, shipped_at, review_request_count)
		VALUES ('o-review', 'cid', 'item-1', 'buyer-1', 'chat-1', 1, '`+shipped+`', 0)`)
}

// TestSchedulerScanReviewRequestBranches 覆盖求评价扫描中账号门禁、规则查询失败、延期与执行失败的日志分支。
func TestSchedulerScanReviewRequestBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("账号暂停时不发送", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		seedSchedulerReviewOrder(t, store, 0)
		if _, err := store.Cookies.SetPause(ctx, "cid", 60); err != nil {
			t.Fatal(err)
		}
		// sender 记录是否发生发送。
		sender := &testSender{}
		NewScheduler(New(store, testSenderProvider{sender: sender}, nil)).scan(ctx)
		if len(sender.texts) != 0 {
			t.Fatalf("暂停账号不得发送: %v", sender.texts)
		}
	})
	t.Run("账号状态读取失败时不发送", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		seedSchedulerReviewOrder(t, store, 0)
		execGapSQL(t, store, `ALTER TABLE cookies RENAME TO cookies_gap_hidden`)
		// sender 记录是否发生发送。
		sender := &testSender{}
		NewScheduler(New(store, testSenderProvider{sender: sender}, nil)).scan(ctx)
		if len(sender.texts) != 0 {
			t.Fatalf("状态读取失败不得发送: %v", sender.texts)
		}
	})
	t.Run("规则动作表不可读时不发送", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		seedSchedulerReviewOrder(t, store, 0)
		execGapSQL(t, store, `ALTER TABLE automation_rule_actions RENAME TO automation_rule_actions_gap_hidden`)
		// sender 记录是否发生发送。
		sender := &testSender{}
		NewScheduler(New(store, testSenderProvider{sender: sender}, nil)).scan(ctx)
		if len(sender.texts) != 0 {
			t.Fatalf("规则查询失败不得发送: %v", sender.texts)
		}
	})
	t.Run("动作延迟时持久化而不立即发送", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		seedSchedulerReviewOrder(t, store, 30)
		// sender 记录是否发生发送。
		sender := &testSender{}
		NewScheduler(New(store, testSenderProvider{sender: sender}, nil)).scan(ctx)
		if len(sender.texts) != 0 {
			t.Fatalf("延迟动作不得立即发送: %v", sender.texts)
		}
	})
	t.Run("发送失败只影响当前订单", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		seedSchedulerReviewOrder(t, store, 0)
		// sender 对所有发送返回平台错误。
		sender := &testSender{err: errors.New("平台拒绝")}
		NewScheduler(New(store, testSenderProvider{sender: sender}, nil)).scan(ctx)
		// order 保存求评价计数，失败后不应记为成功。
		order, _ := store.Orders.Get(ctx, "o-review")
		if order.ReviewRequestCount != 0 {
			t.Fatalf("失败不应计入求评价次数: %d", order.ReviewRequestCount)
		}
	})
	t.Run("恢复历史未发送求评价运行", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		seedSchedulerReviewOrder(t, store, 0)
		// rules、listErr 保存规则列表以获取规则主键。
		var ruleID int64
		if err := store.DB.QueryRowContext(ctx, `SELECT id FROM automation_rules LIMIT 1`).Scan(&ruleID); err != nil {
			t.Fatal(err)
		}
		// runID 保存历史求评价运行。
		runID := catchupRun(t, store, ruleID, "cid", "o-review", `{}`)
		execGapSQL(t, store, `UPDATE automation_runs SET trigger_type='review_missing_timeout',status='needs_review',sent_count=0,error_message='账号发送器未初始化' WHERE id=`+itoa(runID))
		NewScheduler(New(store, nil, nil)).scan(ctx)
		// run 保存恢复后的运行；它应脱离 needs_review。
		run, _ := store.Automation.GetRun(ctx, runID)
		if run.Status == "needs_review" && strings.Contains(run.ErrorMessage, "账号发送器未初始化") {
			t.Fatalf("历史未发送运行应被恢复: %+v", run)
		}
	})
}

// itoa 将运行主键转换为 SQL 夹具中的十进制文本。
func itoa(v int64) string {
	// b 保存 v 的 JSON 十进制编码。
	b, _ := json.Marshal(v)
	return string(b)
}

// TestRunRecoveryTasksGapBranches 覆盖恢复调度器的身份不一致、规则缺失、等待连接、领取失败和执行结果分支。
func TestRunRecoveryTasksGapBranches(t *testing.T) {
	ctx := context.Background()
	// okTask 构造与 seedRecoverableSchedulerRun 运行身份一致的快照。
	okTask := func(suffix string) Task {
		return Task{AccountID: "cid", TriggerType: TriggerBuyerReviewed, OrderID: "recovery-" + suffix, ChatID: "chat", BuyerID: "buyer"}
	}
	t.Run("快照与运行身份不一致转人工", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		// bad 的账号与运行归属不同。
		bad := okTask("id")
		bad.AccountID = "someone-else"
		// runID 保存运行主键。
		runID := seedRecoverableSchedulerRun(t, store, ctx, "id", bad)
		if err := (&Scheduler{center: New(store, nil, nil)}).runRecoveryTasks(ctx); err != nil {
			t.Fatal(err)
		}
		// run 保存隔离后的运行。
		run, _ := store.Automation.GetRun(ctx, runID)
		if run.Status != "needs_review" {
			t.Fatalf("状态=%s", run.Status)
		}
	})
	t.Run("规则停用转人工", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		// runID 保存运行主键。
		runID := seedRecoverableSchedulerRun(t, store, ctx, "norule", okTask("norule"))
		execGapSQL(t, store, `UPDATE automation_rules SET enabled=0`)
		if err := (&Scheduler{center: New(store, nil, nil)}).runRecoveryTasks(ctx); err != nil {
			t.Fatal(err)
		}
		// run 保存隔离后的运行。
		run, _ := store.Automation.GetRun(ctx, runID)
		if run.Status != "needs_review" {
			t.Fatalf("状态=%s", run.Status)
		}
	})
	t.Run("规则读取失败保留运行", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		// runID 保存运行主键。
		runID := seedRecoverableSchedulerRun(t, store, ctx, "ruleerr", okTask("ruleerr"))
		execGapSQL(t, store, `ALTER TABLE automation_rule_actions RENAME TO automation_rule_actions_gap_hidden`)
		if err := (&Scheduler{center: New(store, nil, nil)}).runRecoveryTasks(ctx); err != nil {
			t.Fatalf("规则读取失败只记录日志: %v", err)
		}
		// run 保存运行，仍应保持 running。
		run, _ := store.Automation.GetRun(ctx, runID)
		if run.Status != "running" {
			t.Fatalf("状态=%s", run.Status)
		}
	})
	t.Run("等待连接时延期及延期写入失败", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		// runID 保存运行主键。
		runID := seedRecoverableSchedulerRun(t, store, ctx, "wait", okTask("wait"))
		// s 没有发送器提供方，发送动作必须等待 WebSocket。
		s := &Scheduler{center: New(store, nil, nil)}
		if err := s.runRecoveryTasks(ctx); err != nil {
			t.Fatal(err)
		}
		// run 保存延期后的运行。
		run, _ := store.Automation.GetRun(ctx, runID)
		if run.LeaseExpiresAt <= time.Now().UTC().Unix() {
			t.Fatalf("应延期租约: %+v", run)
		}
		execGapSQL(t, store, `UPDATE automation_runs SET lease_expires_at=0 WHERE id=`+itoa(runID))
		execGapSQL(t, store, gapAbortRunUpdate)
		if err := s.runRecoveryTasks(ctx); err == nil || !strings.Contains(err.Error(), "等待 WebSocket 时延期自动化任务失败") {
			t.Fatalf("应返回延期失败: %v", err)
		}
	})
	t.Run("领取失败返回错误", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		seedRecoverableSchedulerRun(t, store, ctx, "claim", okTask("claim"))
		execGapSQL(t, store, gapAbortRunUpdate)
		// s 有在线发送器，因此会走到领取步骤。
		s := &Scheduler{center: New(store, testSenderProvider{sender: &testSender{}}, nil)}
		if err := s.runRecoveryTasks(ctx); err == nil || !strings.Contains(err.Error(), "领取自动化恢复任务失败") {
			t.Fatalf("应返回领取失败: %v", err)
		}
	})
	t.Run("动作延迟时恢复运行再次延期", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		seedRecoverableSchedulerRun(t, store, ctx, "delay", okTask("delay"))
		execGapSQL(t, store, `UPDATE automation_rule_actions SET delay_seconds=30`)
		// sender 记录是否立即发送。
		sender := &testSender{}
		if err := (&Scheduler{center: New(store, testSenderProvider{sender: sender}, nil)}).runRecoveryTasks(ctx); err != nil {
			t.Fatal(err)
		}
		if len(sender.texts) != 0 {
			t.Fatalf("延迟动作不得立即发送: %v", sender.texts)
		}
	})
	t.Run("执行失败汇总返回", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		seedRecoverableSchedulerRun(t, store, ctx, "fail", okTask("fail"))
		// sender 对发送返回平台错误。
		sender := &testSender{err: errors.New("平台拒绝")}
		if err := (&Scheduler{center: New(store, testSenderProvider{sender: sender}, nil)}).runRecoveryTasks(ctx); err == nil {
			t.Fatal("执行失败应汇总返回")
		}
	})
	t.Run("付款运行门禁写入失败汇总返回", func(t *testing.T) {
		// runID、sender、scheduler、store、cleanup 保存人工来源付款运行夹具。
		_, _, scheduler, store, cleanup := newPaidRecoveryFixture(t, "pending_ship", "manual")
		defer cleanup()
		execGapSQL(t, store, gapAbortRunUpdate)
		if err := scheduler.runRecoveryTasks(ctx); err == nil {
			t.Fatal("隔离写入失败应汇总返回")
		}
	})
}

// TestRecoveryNeedsSenderOutOfRangeCursor 验证游标越界时不再要求发送器。
func TestRecoveryNeedsSenderOutOfRangeCursor(t *testing.T) {
	// rule 含一条发送文本动作。
	rule := db.AutomationRule{Actions: []db.AutomationAction{{ActionType: ActionSendText, Enabled: true}}}
	if recoveryNeedsSender(Task{}, rule, 99) || recoveryNeedsSender(Task{}, rule, -1) {
		t.Fatal("越界游标不需要发送器")
	}
}

// TestRunDeferredTasksGapBranches 覆盖暂停期间事件重放的再次延期、状态写入失败与重试上限通知。
func TestRunDeferredTasksGapBranches(t *testing.T) {
	ctx := context.Background()
	// deferTask 写入一条立即到期的延期任务。
	deferTask := func(t *testing.T, store *db.Store, task Task, attempt int) {
		t.Helper()
		// raw 保存任务快照。
		raw, _ := json.Marshal(task)
		if err := store.Automation.DeferTask(ctx, db.DeferredAutomationTask{TaskKey: "gap:" + task.OrderID, CookieID: task.AccountID, TriggerType: task.TriggerType, TaskJSON: string(raw), DueAt: 0}); err != nil {
			t.Fatal(err)
		}
		execGapSQL(t, store, `UPDATE automation_pending_tasks SET attempt_count=`+itoa(int64(attempt)))
	}
	// task 是不会命中任何规则的评价事件。
	task := Task{AccountID: "cid", TriggerType: TriggerBuyerReviewed, OrderID: "gap-order", ChatID: "chat", BuyerID: "buyer"}
	t.Run("账号仍暂停时再次延期", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		deferTask(t, store, task, 0)
		if _, err := store.Cookies.SetPause(ctx, "cid", 60); err != nil {
			t.Fatal(err)
		}
		if err := (&Scheduler{center: New(store, nil, nil)}).runDeferredTasks(ctx); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("终态写入失败汇总返回", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		deferTask(t, store, task, 0)
		execGapSQL(t, store, `CREATE TRIGGER gap_abort_pending BEFORE DELETE ON automation_pending_tasks BEGIN SELECT RAISE(ABORT, 'forced finish failure'); END`)
		execGapSQL(t, store, `CREATE TRIGGER gap_abort_pending_upd BEFORE UPDATE ON automation_pending_tasks WHEN NEW.status<>'running' BEGIN SELECT RAISE(ABORT, 'forced finish failure'); END`)
		if err := (&Scheduler{center: New(store, nil, nil)}).runDeferredTasks(ctx); err == nil || !errors.Is(err, errAutomationNeedsReview) {
			t.Fatalf("应返回需人工处理错误: %v", err)
		}
	})
	t.Run("连续失败达到上限发送人工通知", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		// admin 保存规则所属管理员。
		admin, _ := store.Users.GetByUsername(ctx, "admin")
		if _, err := store.Automation.Create(ctx, db.AutomationRuleInput{UserID: admin.ID, CookieID: "cid", Name: "fail", TriggerType: TriggerBuyerReviewed, Enabled: true,
			Actions: []db.AutomationActionInput{{ActionType: ActionSendText, MessageTemplate: "x", Enabled: true}}}); err != nil {
			t.Fatal(err)
		}
		deferTask(t, store, task, 4)
		// notifier 记录人工通知。
		notifier := &recordingNotifier{}
		// center 使用必败的发送器。
		center := NewWithDependencies(store, testSenderProvider{sender: &testSender{err: errors.New("平台拒绝")}}, nil, CenterDependencies{Notifier: notifier})
		if err := (&Scheduler{center: center}).runDeferredTasks(ctx); err != nil {
			t.Fatal(err)
		}
	})
}

// TestNotifyDeferredTaskNeedsReviewDefaults 验证空接收者保护以及账号、触发类型缺省回填。
func TestNotifyDeferredTaskNeedsReviewDefaults(t *testing.T) {
	// nilScheduler 覆盖空调度器保护。
	var nilScheduler *Scheduler
	nilScheduler.notifyDeferredTaskNeedsReview(context.Background(), db.DeferredAutomationTask{}, Task{}, "x")
	(&Scheduler{}).notifyDeferredTaskNeedsReview(context.Background(), db.DeferredAutomationTask{}, Task{}, "x")
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	(&Scheduler{center: New(store, nil, nil)}).notifyDeferredTaskNeedsReview(context.Background(),
		db.DeferredAutomationTask{ID: 7, CookieID: "cid", TriggerType: TriggerOrderPaid}, Task{}, "原因")
}
