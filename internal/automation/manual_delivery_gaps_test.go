package automation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"xianyu-go/internal/db"
)

// seedManualPriorRun 为订单写入一条已有的付款发货运行，并精确设置状态、错误、发送数和加密快照。
// triggerKey 区分自动与人工幂等键；返回运行主键。
func seedManualPriorRun(t *testing.T, store *db.Store, orderID, triggerKey, status, errMsg string, sent int, proof db.AutomationDeliveryProof) int64 {
	t.Helper()
	ctx := context.Background()
	// ruleID 是夹具规则主键，运行必须引用现有规则。
	var ruleID int64
	if err := store.DB.QueryRowContext(ctx, `SELECT id FROM automation_rules LIMIT 1`).Scan(&ruleID); err != nil {
		t.Fatal(err)
	}
	// runID、started、startErr 保存运行创建结果。
	runID, started, startErr := store.Automation.TryStartRun(ctx, db.AutomationRun{
		RuleID: ruleID, CookieID: "cid", ItemID: "manual-item", OrderID: orderID, BuyerID: "buyer", ChatID: "chat",
		TriggerType: TriggerOrderPaid, TriggerKey: triggerKey, RawEventJSON: `{}`, LeaseExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if startErr != nil || !started {
		t.Fatalf("创建先前运行失败: %v %v", started, startErr)
	}
	execGapSQL(t, store, `UPDATE automation_runs SET action_started=1 WHERE id=`+itoa(runID))
	if err := store.Automation.UpdateDeliveryReplayProof(ctx, runID, 1, proof); err != nil {
		t.Fatal(err)
	}
	// escaped 是单引号转义后的错误文本，夹具文本不含外部输入。
	escaped := strings.ReplaceAll(errMsg, "'", "''")
	execGapSQL(t, store, `UPDATE automation_runs SET action_started=0,status='`+status+`',error_message='`+escaped+`',sent_count=`+itoa(int64(sent))+`,lease_expires_at=0 WHERE id=`+itoa(runID))
	return runID
}

// TestPrepareManualDeliveryTaskValidation 覆盖人工发货输入与账号状态校验。
func TestPrepareManualDeliveryTaskValidation(t *testing.T) {
	// nilCenter 覆盖空中心保护。
	var nilCenter *Center
	if _, err := nilCenter.prepareManualDeliveryTask(context.Background(), &db.Order{}); err == nil {
		t.Fatal("空中心应报错")
	}
	// cases 逐项覆盖订单字段校验，mutate 修改基础订单，want 是期望错误片段。
	cases := []struct {
		name   string
		mutate func(*db.Order)
		want   string
	}{
		{"缺订单号", func(o *db.Order) { o.OrderID = " " }, "订单ID"},
		{"缺商品", func(o *db.Order) { o.ItemID = "" }, "商品ID"},
		{"缺会话", func(o *db.Order) { o.ChatID = "" }, "chat_id"},
		{"缺买家", func(o *db.Order) { o.BuyerID = "" }, "buyer_id"},
	}
	// tc 表示当前校验用例。
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ctx、center、order、cleanup 保存人工发货夹具。
			ctx, _, center, _, _, order, cleanup := newManualDeliveryFixture(t, "prep-order")
			defer cleanup()
			tc.mutate(order)
			if _, err := center.prepareManualDeliveryTask(ctx, order); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %s", err, tc.want)
			}
		})
	}
	t.Run("账号暂停", func(t *testing.T) {
		// ctx、store、center、order、cleanup 保存人工发货夹具。
		ctx, store, center, _, _, order, cleanup := newManualDeliveryFixture(t, "prep-pause")
		defer cleanup()
		if _, err := store.Cookies.SetPause(ctx, "cid", 60); err != nil {
			t.Fatal(err)
		}
		if _, err := center.prepareManualDeliveryTask(ctx, order); err == nil || !strings.Contains(err.Error(), "暂停") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("账号停用", func(t *testing.T) {
		// ctx、store、center、order、cleanup 保存人工发货夹具。
		ctx, store, center, _, _, order, cleanup := newManualDeliveryFixture(t, "prep-disabled")
		defer cleanup()
		execGapSQL(t, store, `INSERT OR REPLACE INTO cookie_status (cookie_id, enabled) VALUES ('cid', 0)`)
		if _, err := center.prepareManualDeliveryTask(ctx, order); err == nil || !strings.Contains(err.Error(), "停用") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("账号读取失败", func(t *testing.T) {
		// ctx、store、center、order、cleanup 保存人工发货夹具。
		ctx, store, center, _, _, order, cleanup := newManualDeliveryFixture(t, "prep-cookies")
		defer cleanup()
		execGapSQL(t, store, `ALTER TABLE cookies RENAME TO cookies_gap_hidden`)
		if _, err := center.prepareManualDeliveryTask(ctx, order); err == nil {
			t.Fatal("应报账号读取错误")
		}
	})
	t.Run("缺账号ID", func(t *testing.T) {
		// ctx、center、order、cleanup 保存人工发货夹具。
		ctx, _, center, _, _, order, cleanup := newManualDeliveryFixture(t, "prep-nocookie")
		defer cleanup()
		order.CookieID = ""
		if _, err := center.prepareManualDeliveryTask(ctx, order); err == nil {
			t.Fatal("缺账号应报错")
		}
	})
	t.Run("订单事实写入失败", func(t *testing.T) {
		// ctx、store、center、order、cleanup 保存人工发货夹具。
		ctx, store, center, _, _, order, cleanup := newManualDeliveryFixture(t, "prep-upsert")
		defer cleanup()
		execGapSQL(t, store, `ALTER TABLE orders RENAME TO orders_gap_hidden`)
		if _, err := center.prepareManualDeliveryTask(ctx, order); err == nil {
			t.Fatal("订单事实不可写应报错")
		}
	})
}

// TestHandlePriorManualDeliveryMatrix 覆盖已有运行对人工完整发货的放行、补发与拒绝矩阵。
func TestHandlePriorManualDeliveryMatrix(t *testing.T) {
	// proof 是带一条文本消息的有效快照。
	proof := db.AutomationDeliveryProof{TradeText: "CARD", Messages: []db.AutomationDeliveryMessage{{Kind: "text", Content: "CARD"}}}
	// cases 描述先前运行的特征与期望：wantHandled 表示是否已作出最终结论，wantErr 是错误片段（空表示无错误）。
	cases := []struct {
		name        string
		key         string
		status      string
		errMsg      string
		sent        int
		proof       db.AutomationDeliveryProof
		wantHandled bool
		wantErr     string
	}{
		{"自动运行仍在执行", "auto", "running", "", 0, db.AutomationDeliveryProof{}, true, "仍在执行"},
		{"结果不确定禁止补发", "auto", "failed", db.NoRetryErrorPrefix + "x", 0, db.AutomationDeliveryProof{}, true, "不确定"},
		{"补取占用未收口", "auto", "failed", "x", 0, db.AutomationDeliveryProof{RefillPending: true, TradeText: "x"}, true, "补取卡密"},
		{"失败且有快照走补发校验", "auto", "failed", "x", 1, proof, true, "逐单位"},
		{"成功且有快照拒绝", "auto", "success", "", 1, proof, true, "已存在成功"},
		{"成功无快照允许重新发货", "auto", "success", "", 1, db.AutomationDeliveryProof{}, false, ""},
		{"人工审核无快照拒绝", "auto", "needs_review", "x", 0, db.AutomationDeliveryProof{}, true, "没有可安全重发"},
		{"已发送但无快照拒绝", "auto", "failed", "x", 2, db.AutomationDeliveryProof{}, true, "没有可安全重发"},
		{"失败未发送允许重新开始", "auto", "failed", "x", 0, db.AutomationDeliveryProof{}, false, ""},
		{"人工键且运行中", "manual_delivery:prior-order", "running", "", 0, db.AutomationDeliveryProof{}, true, "正在执行"},
	}
	// tc 表示当前矩阵用例。
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ctx、store、center、order、cleanup 保存人工发货夹具。
			ctx, store, center, _, _, order, cleanup := newManualDeliveryFixture(t, "prior-order")
			defer cleanup()
			seedManualPriorRun(t, store, order.OrderID, tc.key, tc.status, tc.errMsg, tc.sent, tc.proof)
			// task 是人工任务，账号与订单对应先前运行。
			task := Task{AccountID: "cid", OrderID: order.OrderID}
			// handled、sent、err 保存矩阵结论。
			handled, sent, err := center.handlePriorManualDelivery(ctx, task, "manual_delivery:prior-order")
			if handled != tc.wantHandled || sent != 0 {
				t.Fatalf("handled=%v sent=%d err=%v", handled, sent, err)
			}
			if (tc.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err=%v want %q", err, tc.wantErr)
			}
		})
	}
	t.Run("读取先前运行失败", func(t *testing.T) {
		// ctx、store、center、order、cleanup 保存人工发货夹具。
		ctx, store, center, _, _, order, cleanup := newManualDeliveryFixture(t, "prior-readerr")
		defer cleanup()
		execGapSQL(t, store, `ALTER TABLE automation_runs RENAME TO automation_runs_gap_hidden`)
		handled, _, err := center.handlePriorManualDelivery(ctx, Task{AccountID: "cid", OrderID: order.OrderID}, "k")
		if !handled || err == nil {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
	})
}

// TestHandleExistingManualDeliveryMatrix 覆盖同一人工幂等键运行的各状态处理结论。
func TestHandleExistingManualDeliveryMatrix(t *testing.T) {
	// center 无需存储即可处理不触发补发的分支。
	center := &Center{}
	// proof 是有效但缺少逐单位记录的快照。
	proof := db.AutomationDeliveryProof{TradeText: "x"}
	// cases 描述运行形态与期望错误片段。
	cases := []struct {
		name string
		run  db.AutomationRun
		want string
	}{
		{"执行中", db.AutomationRun{Status: "running"}, "正在执行"},
		{"补取占用", db.AutomationRun{Status: "failed", DeliveryProof: db.AutomationDeliveryProof{RefillPending: true}}, "补取卡密"},
		{"失败未发送", db.AutomationRun{Status: "failed"}, "尚未发送"},
		{"失败已发送无快照", db.AutomationRun{Status: "failed", SentCount: 1}, "不确定"},
		{"人工核对无快照", db.AutomationRun{Status: "needs_review"}, "不确定"},
		{"已完成", db.AutomationRun{Status: "success"}, "已完成完整发货"},
	}
	// tc 表示当前用例。
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// run 是被测运行副本。
			run := tc.run
			handled, _, err := center.handleExistingManualDelivery(context.Background(), Task{}, &run)
			if !handled || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("handled=%v err=%v want %s", handled, err, tc.want)
			}
		})
	}
	t.Run("有快照时进入补发领取", func(t *testing.T) {
		// ctx、store、center2、order、cleanup 保存人工发货夹具。
		ctx, store, center2, _, _, order, cleanup := newManualDeliveryFixture(t, "existing-proof")
		defer cleanup()
		// runID 保存带快照的失败人工运行。
		runID := seedManualPriorRun(t, store, order.OrderID, "manual_delivery:existing-proof", "failed", "x", 1, proof)
		// run 保存从数据库读取的自动化运行记录，用于断言状态与代次。
		run, _ := store.Automation.GetRun(ctx, runID)
		_, _, err := center2.handleExistingManualDelivery(ctx, Task{AccountID: "cid", OrderID: order.OrderID}, run)
		if err == nil {
			t.Fatal("缺少逐单位记录应拒绝")
		}
	})
}

// TestClaimAndReplayDeliveryProofGuards 覆盖补发领取与重发阶段的前置保护。
func TestClaimAndReplayDeliveryProofGuards(t *testing.T) {
	// ctx、store、center、sender、order、cleanup 保存人工发货夹具。
	ctx, store, center, sender, _, order, cleanup := newManualDeliveryFixture(t, "replay-guard")
	defer cleanup()
	// task 是补发任务。
	task := Task{AccountID: "cid", OrderID: order.OrderID, ChatID: "chat", BuyerID: "buyer"}
	if _, err := center.claimAndReplayDeliveryProof(ctx, task, nil); err == nil {
		t.Fatal("空运行应报错")
	}
	if _, err := center.claimAndReplayDeliveryProof(ctx, task, &db.AutomationRun{DeliveryProof: db.AutomationDeliveryProof{RefillPending: true}}); err == nil {
		t.Fatal("补取占用应报错")
	}
	if _, err := center.claimAndReplayDeliveryProof(ctx, task, &db.AutomationRun{}); err == nil {
		t.Fatal("无快照应报错")
	}
	if _, err := center.replayDeliveryProof(ctx, task, nil); err == nil {
		t.Fatal("空运行应报错")
	}
	// proof 是带逐单位记录的有效快照。
	proof := db.AutomationDeliveryProof{TradeText: "CARD", Messages: []db.AutomationDeliveryMessage{{Kind: "text", Content: "CARD"}}, ExpectedUnits: 1, PreparedUnits: 1}
	// newRunning 创建一条持有执行权的运行并返回其快照对象。
	newRunning := func(key string, p db.AutomationDeliveryProof) *db.AutomationRun {
		// id 是新运行主键。
		id := seedManualPriorRun(t, store, order.OrderID+key, key, "failed", "x", 1, p)
		execGapSQL(t, store, `UPDATE automation_runs SET status='running',action_started=1,lease_expires_at=`+itoa(time.Now().Add(time.Minute).Unix())+` WHERE id=`+itoa(id))
		// run 保存带加密快照解密结果的运行。
		run, err := store.Automation.GetRun(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		run.DeliveryProof = p
		return run
	}
	t.Run("领取失败运行不存在", func(t *testing.T) {
		// run 引用不存在的运行主键。
		run := &db.AutomationRun{ID: 999999, AttemptCount: 1, RawEventJSON: `{}`, DeliveryProof: proof}
		if _, err := center.claimAndReplayDeliveryProof(ctx, task, run); err == nil {
			t.Fatal("应返回计划或领取错误")
		}
	})
	t.Run("租约失效", func(t *testing.T) {
		// run 引用不存在的运行，续租必然失败。
		run := &db.AutomationRun{ID: 999998, AttemptCount: 1, DeliveryProof: proof}
		if _, err := center.replayDeliveryProof(ctx, task, run); err == nil {
			t.Fatal("续租失败应报错")
		}
	})
	t.Run("快照缺逐单位记录", func(t *testing.T) {
		// run 的快照缺少 ExpectedUnits。
		run := newRunning("k-nounits", db.AutomationDeliveryProof{TradeText: "x"})
		if _, err := center.replayDeliveryProof(ctx, task, run); err == nil || !strings.Contains(err.Error(), "逐单位") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("快照单位数无效", func(t *testing.T) {
		// run 的已知单位超过期望单位。
		run := newRunning("k-badunits", db.AutomationDeliveryProof{TradeText: "x", ExpectedUnits: 1, PreparedUnits: 2})
		if _, err := center.replayDeliveryProof(ctx, task, run); err == nil || !strings.Contains(err.Error(), "单位数量无效") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("消息内容为空", func(t *testing.T) {
		// run 含一条空内容消息。
		run := newRunning("k-empty", db.AutomationDeliveryProof{TradeText: "x", ExpectedUnits: 1, PreparedUnits: 1, Messages: []db.AutomationDeliveryMessage{{Kind: "text", Content: " "}}})
		if _, err := center.replayDeliveryProof(ctx, task, run); err == nil || !strings.Contains(err.Error(), "为空") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("消息类型无效", func(t *testing.T) {
		// run 含未知类型消息。
		run := newRunning("k-kind", db.AutomationDeliveryProof{TradeText: "x", ExpectedUnits: 1, PreparedUnits: 1, Messages: []db.AutomationDeliveryMessage{{Kind: "video", Content: "v"}}})
		if _, err := center.replayDeliveryProof(ctx, task, run); err == nil || !strings.Contains(err.Error(), "类型无效") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("发送失败不重新取卡", func(t *testing.T) {
		// run 含可发送文本，但发送器失败。
		run := newRunning("k-send", db.AutomationDeliveryProof{TradeText: "x", ExpectedUnits: 1, PreparedUnits: 1, Messages: []db.AutomationDeliveryMessage{{Kind: "text", Content: "CARD"}}})
		sender.err = errors.New("平台拒绝")
		defer func() { sender.err = nil }()
		if _, err := center.replayDeliveryProof(ctx, task, run); err == nil || !strings.Contains(err.Error(), "原样补发订单内容失败") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("旧版快照按图片后文本恢复并因账号暂停拒绝", func(t *testing.T) {
		// run 为旧版快照，仅含图片与文本字段。
		run := newRunning("k-legacy", db.AutomationDeliveryProof{TradeText: "x", PicList: []string{"http://img"}, ExpectedUnits: 1, PreparedUnits: 1})
		if _, err := store.Cookies.SetPause(ctx, "cid", 60); err != nil {
			t.Fatal(err)
		}
		if _, err := center.replayDeliveryProof(ctx, task, run); err == nil || !strings.Contains(err.Error(), "暂停") {
			t.Fatalf("err=%v", err)
		}
	})
}

// TestExecuteManualDeliveryRulesBranches 覆盖规则匹配失败、无规则、无匹配动作与重复运行的分支。
func TestExecuteManualDeliveryRulesBranches(t *testing.T) {
	t.Run("无匹配规则", func(t *testing.T) {
		// ctx、center、cleanup 保存人工发货夹具。
		ctx, _, center, _, _, _, cleanup := newManualDeliveryFixture(t, "rules-none")
		defer cleanup()
		if _, err := center.executeManualDeliveryRules(ctx, Task{AccountID: "cid", ItemID: "other-item", TriggerType: TriggerOrderPaid}, "k"); err == nil || !strings.Contains(err.Error(), "未匹配到付款后自动发货规则") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("规则查询失败", func(t *testing.T) {
		// ctx、store、center、cleanup 保存人工发货夹具。
		ctx, store, center, _, _, _, cleanup := newManualDeliveryFixture(t, "rules-err")
		defer cleanup()
		execGapSQL(t, store, `ALTER TABLE automation_rule_actions RENAME TO automation_rule_actions_gap_hidden`)
		if _, err := center.executeManualDeliveryRules(ctx, Task{AccountID: "cid", ItemID: "manual-item", TriggerType: TriggerOrderPaid}, "k"); err == nil {
			t.Fatal("应返回规则查询错误")
		}
	})
	t.Run("规格不匹配任何卡密动作", func(t *testing.T) {
		// ctx、center、cleanup 保存人工发货夹具。
		ctx, _, center, _, _, _, cleanup := newManualDeliveryFixture(t, "rules-spec")
		defer cleanup()
		// task 携带规则动作没有配置的规格。
		task := Task{AccountID: "cid", ItemID: "manual-item", TriggerType: TriggerOrderPaid, SpecName: "颜色", SpecValue: "黑", OrderID: "rules-spec"}
		_, err := center.executeManualDeliveryRules(ctx, task, "manual_delivery:rules-spec")
		if err != nil && !strings.Contains(err.Error(), "规格") && !strings.Contains(err.Error(), "卡密") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("已有另一条运行占用", func(t *testing.T) {
		// ctx、store、center、order、cleanup 保存人工发货夹具。
		ctx, store, center, _, _, order, cleanup := newManualDeliveryFixture(t, "rules-dup")
		defer cleanup()
		// 自动运行占用同订单的付款发货幂等位置。
		seedManualPriorRun(t, store, order.OrderID, "ws-key", "success", "", 1, db.AutomationDeliveryProof{})
		// rule、ruleErr 保存夹具规则。
		rules, matchErr := center.rules.match(ctx, Task{AccountID: "cid", ItemID: "manual-item", TriggerType: TriggerOrderPaid})
		if matchErr != nil || len(rules) == 0 {
			t.Fatalf("规则=%v err=%v", rules, matchErr)
		}
		execGapSQL(t, store, `UPDATE automation_runs SET trigger_key='manual_delivery:rules-dup' WHERE trigger_key='ws-key'`)
		// task 是人工任务；运行已存在，应走已有运行处理。
		task := Task{Source: "manual", AccountID: "cid", OrderID: order.OrderID, ItemID: "manual-item", ChatID: "chat", BuyerID: "buyer", TriggerType: TriggerOrderPaid, Quantity: "1"}
		if _, err := center.executeManualDeliveryRule(ctx, task, rules[0], "manual_delivery:rules-dup"); err == nil {
			t.Fatal("已完成的人工运行应拒绝重复发货")
		}
	})
	if got := buildManualDeliveryTriggerKey(Task{}); got != "" {
		t.Fatalf("空订单键=%q", got)
	}
}

// seedQuarantinedManualReplay 创建带指定快照、已进入人工核对的人工发货运行，供补发路径直接复用。
// units 是原始计划中的卡密单位数；返回运行主键。
func seedQuarantinedManualReplay(t *testing.T, ctx context.Context, store *db.Store, order *db.Order, proof db.AutomationDeliveryProof, units int) int64 {
	t.Helper()
	// ruleID 是夹具唯一付款发货规则主键。
	var ruleID int64
	if err := store.DB.QueryRowContext(ctx, `SELECT id FROM automation_rules WHERE cookie_id=? AND item_id=?`, order.CookieID, order.ItemID).Scan(&ruleID); err != nil {
		t.Fatal(err)
	}
	// runID、started、startErr 保存人工幂等运行创建结果。
	runID, started, startErr := store.Automation.TryStartRun(ctx, db.AutomationRun{
		RuleID: ruleID, CookieID: order.CookieID, ItemID: order.ItemID, OrderID: order.OrderID, TriggerType: TriggerOrderPaid,
		TriggerKey: buildManualDeliveryTriggerKey(Task{OrderID: order.OrderID}), LeaseExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	if startErr != nil || !started {
		t.Fatalf("创建人工运行失败: %v %v", started, startErr)
	}
	// current 保存运行代次，动作检查点必须复用。
	current, getErr := store.Automation.GetRun(ctx, runID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if ok, err := store.Automation.StartRunAction(ctx, runID, current.AttemptCount, 0, time.Now().Add(time.Minute).Unix()); err != nil || !ok {
		t.Fatalf("占用动作检查点失败: %v %v", ok, err)
	}
	saveManualReplayTestPlan(t, ctx, store, runID, units)
	if err := store.Automation.QuarantineRunResultWithProof(ctx, runID, current.AttemptCount, 1, "消息回显超时", &proof); err != nil {
		t.Fatal(err)
	}
	return runID
}

// TestManualReplayVariants 覆盖旧版图文快照、图片消息、收口失败、领取冲突和门禁读取失败。
func TestManualReplayVariants(t *testing.T) {
	t.Run("旧版快照按图片后文本补发并确认发货", func(t *testing.T) {
		// ctx、store、center、sender、mtopMock、order、cleanup 保存人工发货夹具。
		ctx, store, center, sender, mtopMock, order, cleanup := newManualDeliveryFixture(t, "variant-legacy")
		defer cleanup()
		seedQuarantinedManualReplay(t, ctx, store, order, db.AutomationDeliveryProof{TradeText: "LEGACY-TEXT", PicList: []string{"http://img/1.png"}, ExpectedUnits: 1, PreparedUnits: 1}, 1)
		// sent、err 保存补发结果。
		sent, err := center.ManualFullDelivery(ctx, order)
		if err != nil || sent != 2 || len(sender.texts) != 1 || sender.texts[0] != "LEGACY-TEXT" || mtopMock.consignCalls != 1 {
			t.Fatalf("sent=%d texts=%v consign=%d err=%v", sent, sender.texts, mtopMock.consignCalls, err)
		}
	})
	t.Run("补发后收口失败保留人工核对", func(t *testing.T) {
		// ctx、store、center、sender、order、cleanup 保存人工发货夹具。
		ctx, store, center, sender, _, order, cleanup := newManualDeliveryFixture(t, "variant-complete")
		defer cleanup()
		seedQuarantinedManualReplay(t, ctx, store, order, db.AutomationDeliveryProof{TradeText: "T", Messages: []db.AutomationDeliveryMessage{{Kind: "text", Content: "T"}}, ExpectedUnits: 1, PreparedUnits: 1}, 1)
		execGapSQL(t, store, `CREATE TRIGGER gap_reject_success BEFORE UPDATE OF status ON automation_runs WHEN NEW.status='success' BEGIN SELECT RAISE(ABORT, 'forced complete failure'); END`)
		if _, err := center.ManualFullDelivery(ctx, order); err == nil || !strings.Contains(err.Error(), "保存运行结果失败") {
			t.Fatalf("err=%v", err)
		}
		if len(sender.texts) != 1 {
			t.Fatalf("消息只应发送一次: %v", sender.texts)
		}
	})
	t.Run("领取补发权失败", func(t *testing.T) {
		// ctx、store、center、order、cleanup 保存人工发货夹具。
		ctx, store, center, _, _, order, cleanup := newManualDeliveryFixture(t, "variant-claim")
		defer cleanup()
		// proof 是完整快照。
		proof := db.AutomationDeliveryProof{TradeText: "T", Messages: []db.AutomationDeliveryMessage{{Kind: "text", Content: "T"}}, ExpectedUnits: 1, PreparedUnits: 1}
		// runID 保存人工核对运行。
		runID := seedQuarantinedManualReplay(t, ctx, store, order, proof, 1)
		// run 保存从数据库读取的自动化运行记录，用于断言状态与代次。
		run, _ := store.Automation.GetRun(ctx, runID)
		run.DeliveryProof = proof
		// stale 是代次已落后的扫描快照，领取必须返回未取得。
		stale := *run
		stale.AttemptCount += 3
		if _, err := center.claimAndReplayDeliveryProof(ctx, Task{AccountID: "cid", OrderID: order.OrderID}, &stale); err == nil || !strings.Contains(err.Error(), "正在补发") {
			t.Fatalf("err=%v", err)
		}
		execGapSQL(t, store, gapAbortRunUpdate)
		if _, err := center.claimAndReplayDeliveryProof(ctx, Task{AccountID: "cid", OrderID: order.OrderID}, run); err == nil || !strings.Contains(err.Error(), "领取订单内容补发") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("账号门禁读取失败", func(t *testing.T) {
		// ctx、store、center、order、cleanup 保存人工发货夹具。
		ctx, store, center, _, _, order, cleanup := newManualDeliveryFixture(t, "variant-gate")
		defer cleanup()
		// proof 是完整快照。
		proof := db.AutomationDeliveryProof{TradeText: "T", Messages: []db.AutomationDeliveryMessage{{Kind: "text", Content: "T"}}, ExpectedUnits: 1, PreparedUnits: 1}
		// runID 保存运行主键。
		runID := seedQuarantinedManualReplay(t, ctx, store, order, proof, 1)
		execGapSQL(t, store, `UPDATE automation_runs SET status='running',action_started=1 WHERE id=`+itoa(runID))
		// run 保存从数据库读取的自动化运行记录，用于断言状态与代次。
		run, _ := store.Automation.GetRun(ctx, runID)
		run.DeliveryProof = proof
		execGapSQL(t, store, `ALTER TABLE cookies RENAME TO cookies_gap_hidden`)
		if _, err := center.replayDeliveryProof(ctx, Task{AccountID: "cid", OrderID: order.OrderID}, run); err == nil {
			t.Fatal("门禁读取失败应报错")
		}
	})
	t.Run("图片消息补发", func(t *testing.T) {
		// ctx、store、center、order、cleanup 保存人工发货夹具。
		ctx, store, center, _, mtopMock, order, cleanup := newManualDeliveryFixture(t, "variant-image")
		defer cleanup()
		seedQuarantinedManualReplay(t, ctx, store, order, db.AutomationDeliveryProof{Messages: []db.AutomationDeliveryMessage{{Kind: "image", Content: "http://img/2.png"}}, PicList: []string{"http://img/2.png"}, ExpectedUnits: 1, PreparedUnits: 1}, 1)
		if sent, err := center.ManualFullDelivery(ctx, order); err != nil || sent != 1 || mtopMock.consignCalls != 1 {
			t.Fatalf("sent=%d err=%v", sent, err)
		}
	})
}

// TestRefillPartialDeliveryProofGuards 覆盖补取缺失卡密前的计划、卡组与 API 卡保护。
func TestRefillPartialDeliveryProofGuards(t *testing.T) {
	// ctx、store、center、order、cleanup 保存人工发货夹具。
	ctx, store, center, _, _, order, cleanup := newManualDeliveryFixture(t, "refill-guard")
	defer cleanup()
	// partial 是两单位计划中仅完成一单位的快照。
	partial := db.AutomationDeliveryProof{TradeText: "A", Messages: []db.AutomationDeliveryMessage{{Kind: "text", Content: "A"}}, ExpectedUnits: 2, PreparedUnits: 1}
	// task 是补发任务。
	task := Task{AccountID: "cid", OrderID: order.OrderID}
	t.Run("计划损坏", func(t *testing.T) {
		// run 没有原始发货计划。
		run := &db.AutomationRun{DeliveryProof: partial, RawEventJSON: `{}`}
		if _, err := center.refillPartialDeliveryProof(ctx, task, run); err == nil {
			t.Fatal("计划损坏应报错")
		}
	})
	// runID 保存带两单位计划的运行。
	runID := seedQuarantinedManualReplay(t, ctx, store, order, partial, 2)
	// run 保存运行并带上解密快照。
	run, _ := store.Automation.GetRun(ctx, runID)
	run.DeliveryProof = partial
	t.Run("API卡密禁止自动续取", func(t *testing.T) {
		execGapSQL(t, store, `UPDATE cards SET type='api'`)
		if _, err := center.refillPartialDeliveryProof(ctx, task, run); err == nil || !strings.Contains(err.Error(), "API 卡密") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("卡组不存在", func(t *testing.T) {
		execGapSQL(t, store, `PRAGMA foreign_keys=OFF`)
		execGapSQL(t, store, `DELETE FROM cards`)
		if _, err := center.refillPartialDeliveryProof(ctx, task, run); err == nil || !strings.Contains(err.Error(), "读取补发卡密组") {
			t.Fatalf("err=%v", err)
		}
	})
}

// TestManualFullDeliveryNilReceiver 验证空接收者与空订单直接报错且收尾日志保护不会崩溃。
func TestManualFullDeliveryNilReceiver(t *testing.T) {
	// nilCenter 是空中心。
	var nilCenter *Center
	if _, err := nilCenter.ManualFullDelivery(context.Background(), nil); err == nil {
		t.Fatal("空中心应报错")
	}
}
