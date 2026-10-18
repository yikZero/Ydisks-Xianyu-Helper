package automation

import (
	"context"
	"strings"
	"testing"
	"time"

	"xianyu-go/internal/db"
)

// execGapSQL 在测试库上执行一条夹具改写语句，用于制造订单缺失、表不可读或写入被拒绝等故障。
// store 是测试数据库聚合入口，query 是待执行的 SQLite 语句。
func execGapSQL(t *testing.T, store *db.Store, query string) {
	t.Helper()
	// execErr 保存夹具改写语句的执行错误，失败时测试无法证明目标分支。
	if _, execErr := store.DB.ExecContext(context.Background(), query); execErr != nil {
		t.Fatalf("执行夹具语句失败: %v (%s)", execErr, query)
	}
}

// paidRecoveryCase 汇总一次 paidRecoveryReady 调用所需的运行、任务与调度器。
type paidRecoveryCase struct {
	// runID 是夹具创建的付款运行主键。
	runID int64
	// run 是调用时传入的扫描快照。
	run db.AutomationRun
	// task 是运行创建时冻结的付款任务快照。
	task Task
	// scheduler 是被测恢复调度器。
	scheduler *Scheduler
	// store 是隔离测试数据库。
	store *db.Store
	// sender 记录是否发生越权发送。
	sender *testSender
}

// newPaidRecoveryCase 基于付款恢复夹具构造被测输入，source 为运行来源，status 为本地订单状态。
func newPaidRecoveryCase(t *testing.T, status, source string) paidRecoveryCase {
	t.Helper()
	// runID、sender、scheduler、store、cleanup 保存付款恢复夹具各组成部分。
	runID, sender, scheduler, store, cleanup := newPaidRecoveryFixture(t, status, source)
	t.Cleanup(cleanup)
	// run、runErr 保存扫描快照及读取错误。
	run, runErr := store.Automation.GetRun(context.Background(), runID)
	if runErr != nil {
		t.Fatal(runErr)
	}
	return paidRecoveryCase{
		runID: runID, run: *run, scheduler: scheduler, store: store, sender: sender,
		task: Task{Source: source, AccountID: "cid", OrderRole: OrderRoleSeller, TriggerType: TriggerOrderPaid, OrderID: run.OrderID},
	}
}

// assertRunStatus 断言运行当前状态，用于确认恢复门禁没有越权执行。
func (c paidRecoveryCase) assertRunStatus(t *testing.T, want string) db.AutomationRun {
	t.Helper()
	// got、getErr 保存门禁调用后的运行记录。
	got, getErr := c.store.Automation.GetRun(context.Background(), c.runID)
	if getErr != nil || got.Status != want {
		t.Fatalf("运行状态=%+v err=%v，期望 %s", got, getErr, want)
	}
	if len(c.sender.texts) != 0 {
		t.Fatalf("恢复门禁不应发送任何消息: %v", c.sender.texts)
	}
	return *got
}

// TestPaidRecoveryReadyGates 覆盖付款恢复门禁的全部放行、延期、取消与人工核对分支。
func TestPaidRecoveryReadyGates(t *testing.T) {
	ctx := context.Background()
	// abortUpdate 让 automation_runs 的任何更新失败，用于覆盖延期、取消、隔离的写入错误。
	const abortUpdate = `CREATE TRIGGER gap_abort_run_update BEFORE UPDATE ON automation_runs BEGIN SELECT RAISE(ABORT, 'forced run update failure'); END`

	t.Run("非付款触发直接放行", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "pending_ship", "ws")
		c.task.TriggerType = TriggerBuyerReviewed
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if !ok || err != nil {
			t.Fatalf("非付款触发应放行: ok=%v err=%v", ok, err)
		}
	})
	t.Run("来源可信且开关开启时放行", func(t *testing.T) {
		// source 保存该步骤的中间结果，供后续断言使用。
		for _, source := range []string{"ws", "scheduler"} {
			// c 保存当前来源的夹具。
			c := newPaidRecoveryCase(t, "pending_ship", source)
			ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
			if !ok || err != nil {
				t.Fatalf("%s 来源应放行: ok=%v err=%v", source, ok, err)
			}
		}
	})
	t.Run("缺少本地订单转人工", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "pending_ship", "ws")
		execGapSQL(t, c.store, `DELETE FROM orders`)
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if ok || err != nil {
			t.Fatalf("缺单应拒绝且不报错: ok=%v err=%v", ok, err)
		}
		c.assertRunStatus(t, "needs_review")
	})
	t.Run("订单读取失败延期", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "pending_ship", "ws")
		execGapSQL(t, c.store, `ALTER TABLE orders RENAME TO orders_gap_hidden`)
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if ok || err != nil {
			t.Fatalf("读取失败应延期: ok=%v err=%v", ok, err)
		}
		// got 保存延期后的运行，租约必须被推到未来。
		got := c.assertRunStatus(t, "running")
		if got.LeaseExpiresAt <= time.Now().UTC().Unix() {
			t.Fatalf("租约未延期: %+v", got)
		}
	})
	t.Run("订单读取失败且延期写入失败", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "pending_ship", "ws")
		execGapSQL(t, c.store, `ALTER TABLE orders RENAME TO orders_gap_hidden`)
		execGapSQL(t, c.store, abortUpdate)
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if ok || err == nil || !strings.Contains(err.Error(), "延期付款恢复运行失败") {
			t.Fatalf("应返回合并错误: ok=%v err=%v", ok, err)
		}
	})
	t.Run("订单归属变化转人工", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "pending_ship", "ws")
		// admin、adminErr 保存新账号归属管理员，满足订单到账号的外键约束。
		admin, adminErr := c.store.Users.GetByUsername(ctx, "admin")
		if adminErr != nil {
			t.Fatal(adminErr)
		}
		// saveErr 保存该步骤的中间结果，供后续断言使用。
		if saveErr := c.store.Cookies.Save(ctx, "other-account", "unb=999; _m_h5_tk=tk_2;", admin.ID); saveErr != nil {
			t.Fatal(saveErr)
		}
		execGapSQL(t, c.store, `UPDATE orders SET cookie_id='other-account'`)
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if ok || err != nil {
			t.Fatalf("归属变化应拒绝: ok=%v err=%v", ok, err)
		}
		c.assertRunStatus(t, "needs_review")
	})
	t.Run("系统已发货取消旧运行", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "pending_ship", "ws")
		execGapSQL(t, c.store, `UPDATE orders SET system_shipped=1, order_status='shipped'`)
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if ok || err != nil {
			t.Fatalf("已发货应拒绝: ok=%v err=%v", ok, err)
		}
		c.assertRunStatus(t, "canceled")
	})
	t.Run("扫描快照陈旧时不取消", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "canceled", "ws")
		// 快照尝试次数与数据库不一致，表示运行已被其他实例续租。
		c.run.AttemptCount += 5
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if ok || err != nil {
			t.Fatalf("陈旧快照应静默拒绝: ok=%v err=%v", ok, err)
		}
		c.assertRunStatus(t, "running")
	})
	t.Run("取消写入失败返回错误", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "canceled", "ws")
		execGapSQL(t, c.store, abortUpdate)
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if ok || err == nil || !strings.Contains(err.Error(), "取消已失效付款恢复运行") {
			t.Fatalf("应返回取消错误: ok=%v err=%v", ok, err)
		}
	})
	t.Run("未知角色WS运行缺少商品证据转人工", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "pending_ship", "ws")
		c.task.OrderRole = ""
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if ok || err != nil {
			t.Fatalf("缺少卖家证据应拒绝: ok=%v err=%v", ok, err)
		}
		c.assertRunStatus(t, "needs_review")
	})
	t.Run("未知角色WS运行有商品证据时放行", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "pending_ship", "ws")
		c.task.OrderRole = ""
		// itemErr 保存该步骤的中间结果，供后续断言使用。
		if itemErr := c.store.Items.Upsert(ctx, &db.ItemInfoRow{CookieID: "cid", ItemID: "recovery-item", ItemTitle: "恢复商品"}); itemErr != nil {
			t.Fatal(itemErr)
		}
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if !ok || err != nil {
			t.Fatalf("有卖家证据应放行: ok=%v err=%v", ok, err)
		}
	})
	t.Run("未知角色核验读取失败延期", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "pending_ship", "ws")
		c.task.OrderRole = ""
		execGapSQL(t, c.store, `ALTER TABLE item_info RENAME TO item_info_gap_hidden`)
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if ok || err != nil {
			t.Fatalf("核验失败应延期: ok=%v err=%v", ok, err)
		}
		c.assertRunStatus(t, "running")
	})
	t.Run("未知角色核验读取失败且延期失败", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "pending_ship", "ws")
		c.task.OrderRole = ""
		execGapSQL(t, c.store, `ALTER TABLE item_info RENAME TO item_info_gap_hidden`)
		execGapSQL(t, c.store, abortUpdate)
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if ok || err == nil || !strings.Contains(err.Error(), "核对未知角色付款运行失败") {
			t.Fatalf("应返回合并错误: ok=%v err=%v", ok, err)
		}
	})
	t.Run("自动发货开关读取失败延期", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "pending_ship", "ws")
		execGapSQL(t, c.store, `ALTER TABLE cookies RENAME TO cookies_gap_hidden`)
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if ok || err != nil {
			t.Fatalf("开关读取失败应延期: ok=%v err=%v", ok, err)
		}
		c.assertRunStatus(t, "running")
	})
	t.Run("自动发货开关读取失败且延期失败", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "pending_ship", "ws")
		execGapSQL(t, c.store, `ALTER TABLE cookies RENAME TO cookies_gap_hidden`)
		execGapSQL(t, c.store, abortUpdate)
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if ok || err == nil || !strings.Contains(err.Error(), "读取付款恢复自动发货开关失败") {
			t.Fatalf("应返回合并错误: ok=%v err=%v", ok, err)
		}
	})
	t.Run("自动发货关闭且延期失败", func(t *testing.T) {
		c := newPaidRecoveryCase(t, "pending_ship", "ws")
		execGapSQL(t, c.store, `UPDATE cookies SET auto_confirm=0`)
		execGapSQL(t, c.store, abortUpdate)
		ok, err := c.scheduler.paidRecoveryReady(ctx, c.task, c.run)
		if ok || err == nil || !strings.Contains(err.Error(), "自动发货已关闭") {
			t.Fatalf("应返回延期错误: ok=%v err=%v", ok, err)
		}
	})
}
