package db

import (
	"context"
	"testing"
	"time"
)

// TestFaultInjectionAutomationTemplatesCards 在自动化规则、发货模板、卡密与运行生命周期的每个数据库操作点注入故障。
func TestFaultInjectionAutomationTemplatesCards(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("重型故障注入遍历在竞态检测模式下耗时过长，常规模式已覆盖")
	}
	runFaultScenarios(t, []faultScenario{
		{"TemplateLifecycle", func(t *testing.T, store *Store) func(context.Context) error {
			// userID 是模板所有者。
			userID, _ := seedAccount(t, store)
			// existing 是预置模板，供更新/删除路径使用。
			existing, err := store.DeliveryTemplates.Create(context.Background(), DeliveryTemplateInput{UserID: userID, Name: "旧模板", Enabled: true, Messages: []string{"A {{cards.main}}", "B"}})
			if err != nil {
				t.Fatal(err)
			}
			return func(ctx context.Context) error {
				// created 是新建模板主键。
				created, _ := store.DeliveryTemplates.Create(ctx, DeliveryTemplateInput{UserID: userID, Name: "新模板", Enabled: true, Messages: []string{"hello"}})
				_, _ = store.DeliveryTemplates.ListForUser(ctx, userID)
				_, _ = store.DeliveryTemplates.GetForUser(ctx, userID, existing)
				_ = store.DeliveryTemplates.Update(ctx, userID, existing, DeliveryTemplateInput{UserID: userID, Name: "改名", Enabled: false, Messages: []string{"X", "Y", "Z"}})
				_ = store.DeliveryTemplates.Delete(ctx, userID, created)
				return store.DeliveryTemplates.Delete(ctx, userID, existing)
			}
		}},
		{"RuleWithTemplateAndCard", func(t *testing.T, store *Store) func(context.Context) error {
			// userID、cookieID 是账号归属。
			userID, cookieID := seedAccount(t, store)
			// ctx 是准备阶段上下文。
			ctx := context.Background()
			// templateID 是被引用模板。
			templateID, err := store.DeliveryTemplates.Create(ctx, DeliveryTemplateInput{UserID: userID, Name: "规则模板", Enabled: true, Messages: []string{"{{cards.main}}"}})
			if err != nil {
				t.Fatal(err)
			}
			// cardID 是被绑定的文本卡密。
			cardID, err := store.Cards.Create(ctx, &CardFull{Name: "卡", Type: "text", TextContent: "T", Enabled: true, UserID: userID})
			if err != nil {
				t.Fatal(err)
			}
			// input 是含卡密动作、消息动作与模板动作的规则输入。
			input := makeAutomationRule(cookieID, userID, "item-x", "paid", true, 5,
				AutomationActionInput{ActionType: "send_card", CardID: cardID, DeliveryCount: 1, Enabled: true, SortOrder: 1},
				AutomationActionInput{ActionType: "send_msg", MessageTemplate: "hi", Enabled: true, SortOrder: 2},
				AutomationActionInput{ActionType: "send_template", DeliveryTemplateID: templateID, ConfigJSON: `{}`, Enabled: true, SortOrder: 3, TemplateBindings: []DeliveryTemplateBinding{{VariableKey: "main", CardID: cardID, DeliveryCount: 1}}, CustomVariables: map[string]string{"k": "v"}},
			)
			return func(ctx context.Context) error {
				_, _ = store.Automation.HasEnabledAdjustPriceRule(ctx, cookieID)
				_, _ = store.Automation.ExistsPublishRule(ctx, input)
				// ruleID 是新建规则。
				ruleID, _ := store.Automation.Create(ctx, input)
				_, _ = store.Automation.Get(ctx, ruleID)
				_, _ = store.Automation.Actions(ctx, ruleID)
				_, _ = store.Automation.Match(ctx, cookieID, "item-x", "paid")
				_, _ = store.Automation.ListForUser(ctx, userID)
				_, _, _ = store.Automation.ListPageForUser(ctx, AutomationRuleListFilter{UserID: userID, CookieID: cookieID, TriggerType: "paid", Search: "rule", Limit: 10})
				_, _ = store.Automation.CountByTriggerForUser(ctx, AutomationRuleListFilter{UserID: userID, CookieID: cookieID, Search: "rule"})
				_ = store.Automation.Update(ctx, userID, ruleID, input)
				return store.Automation.Delete(ctx, userID, ruleID)
			}
		}},
		{"RunLifecycle", func(t *testing.T, store *Store) func(context.Context) error {
			// userID、cookieID 是账号归属。
			userID, cookieID := seedAccount(t, store)
			// ctx 是准备阶段上下文。
			ctx := context.Background()
			// ruleID 是付款规则。
			ruleID, err := store.Automation.Create(ctx, makeAutomationRule(cookieID, userID, "item-r", "order_paid", true, 1))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Orders.Upsert(ctx, "ord-r", OrderUpsertOpts{CookieID: cookieID, ItemID: "item-r", BuyerID: "b", OrderStatus: "pending_ship"}); err != nil {
				t.Fatal(err)
			}
			return func(ctx context.Context) error {
				// lease 是租约截止秒数。
				lease := time.Now().Add(time.Hour).Unix()
				// runID 是新建运行。
				runID, _, _ := store.Automation.TryStartRun(ctx, AutomationRun{RuleID: ruleID, CookieID: cookieID, OrderID: "ord-r", ItemID: "item-r", TriggerType: "order_paid", TriggerKey: "k1", LeaseExpiresAt: lease})
				_, _, _ = store.Automation.TryStartRun(ctx, AutomationRun{RuleID: ruleID, CookieID: cookieID, OrderID: "ord-r", TriggerType: "order_paid", TriggerKey: "k1", LeaseExpiresAt: lease})
				_, _ = store.Automation.StartRunAction(ctx, runID, 1, 0, lease)
				_ = store.Automation.RenewRunLease(ctx, runID, 1, lease)
				_ = store.Automation.AdvanceRunAction(ctx, AutomationRunActionAdvance{RunID: runID, Attempt: 1, Cursor: 1, SentDelta: 1, DeliveryProof: &AutomationDeliveryProof{TradeText: "t", Messages: []AutomationDeliveryMessage{{Kind: "text", Content: "m"}}}})
				_ = store.Automation.AbortRunAction(ctx, runID, 1, 1)
				_, _ = store.Automation.GetRun(ctx, runID)
				_, _ = store.Automation.GetRunByRuleAndTrigger(ctx, ruleID, "k1")
				_, _ = store.Automation.GetLatestOrderDeliveryRun(ctx, cookieID, "ord-r")
				_ = store.Automation.QuarantineRunResultWithProof(ctx, runID, 1, 1, "原因", &AutomationDeliveryProof{TradeText: "t"})
				_, _, _ = store.Automation.ListIssues(ctx, userID)
				_ = store.Automation.ResolveRunIssue(ctx, userID, runID, "retry")
				_, _ = store.Automation.ReopenRunForRecovery(ctx, runID, 1, lease)
				_, _ = store.Automation.ClaimRecoveryRun(ctx, runID, lease)
				_ = store.Automation.PostponeRecoveryRun(ctx, runID, 1, time.Now().Unix())
				_, _ = store.Automation.DueRecoveryRuns(ctx, 10)
				_, _, _ = store.Automation.ClaimDeliveryReplay(ctx, runID, 1, lease)
				_ = store.Automation.ReleaseDeliveryReplay(ctx, runID, 1, "failed", "原因")
				_ = store.Automation.UpdateDeliveryReplayProof(ctx, runID, 1, AutomationDeliveryProof{TradeText: "t"})
				_ = store.Automation.CompleteDeliveryReplay(ctx, runID, 1)
				_, _ = store.Automation.CancelObsoletePaidRecoveryRun(ctx, AutomationRun{ID: runID, RuleID: ruleID, CookieID: cookieID, OrderID: "ord-r", TriggerType: "order_paid", TriggerKey: "k1"}, "已变化")
				_, _ = store.Automation.PendingShipOrdersWithoutPaidRunAfter(ctx, "", 10)
				_, _ = store.Automation.PendingShipResumableRunsAfter(ctx, "", 3, 10)
				_ = store.Automation.QuarantineRun(ctx, runID, 1, "隔离")
				return store.Automation.FinishRun(ctx, runID, 1, "failed", 0, "错误")
			}
		}},
		{"DeferredTasks", func(t *testing.T, store *Store) func(context.Context) error {
			// userID、cookieID 是账号归属。
			userID, cookieID := seedAccount(t, store)
			// ctx 是准备阶段上下文。
			ctx := context.Background()
			if err := store.Orders.Upsert(ctx, "ord-d", OrderUpsertOpts{CookieID: cookieID, ItemID: "i", BuyerID: "b", OrderStatus: "completed"}); err != nil {
				t.Fatal(err)
			}
			return func(ctx context.Context) error {
				_ = store.Automation.DeferTask(ctx, DeferredAutomationTask{TaskKey: "dk", CookieID: cookieID, TriggerType: "buyer_reviewed", TaskJSON: `{}`, DueAt: 1})
				// tasks 是已到期被领取的任务。
				tasks, _ := store.Automation.ClaimDueDeferredTasks(ctx, 5)
				// first 是第一条被领取任务，无任务时为零值。
				var first DeferredAutomationTask
				if len(tasks) > 0 {
					first = tasks[0]
				}
				_ = store.Automation.RenewDeferredTaskLease(ctx, first.ID, first.ClaimVersion, time.Now().Add(time.Hour).Unix())
				_ = store.Automation.FinishDeferredTask(ctx, first.ID, first.ClaimVersion, false, "失败")
				_, _, _ = store.Automation.ListIssues(ctx, userID)
				_ = store.Automation.ResolveDeferredIssue(ctx, userID, first.ID, true)
				_ = store.Automation.WakeCredentialBlocked(ctx, cookieID)
				_ = store.Automation.MarkOrderEventTime(ctx, "ord-d", "buyer_reviewed_at")
				_ = store.Automation.IncrementReviewRequest(ctx, "ord-d")
				_, _ = store.Automation.DueReviewRequestOrders(ctx, 5)
				_, _ = store.Automation.RecoverDefinitelyUnsentReviewRuns(ctx)
				return nil
			}
		}},
		{"CardBatchData", func(t *testing.T, store *Store) func(context.Context) error {
			// userID 是卡密所有者。
			userID, _ := seedAccount(t, store)
			// cardID 是批量数据卡密。
			cardID, err := store.Cards.Create(context.Background(), &CardFull{Name: "批量", Type: "data", DataContent: "a\nb\nc", Enabled: true, UserID: userID})
			if err != nil {
				t.Fatal(err)
			}
			return func(ctx context.Context) error {
				_, _ = store.Cards.ConsumeBatchData(ctx, cardID)
				_ = store.Cards.RestoreBatchData(ctx, cardID, "a")
				// snapshot 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
				_, snapshot, _ := store.Cards.FirstBatchData(ctx, cardID)
				_ = store.Cards.CommitFirstBatchData(ctx, cardID, snapshot)
				_, _ = store.Cards.AppendBatchData(ctx, cardID, "d\ne")
				_, _ = store.Cards.AvailableDataStock(ctx, cardID)
				_, _ = store.Cards.ExistsOwned(ctx, cardID, userID)
				_, _ = store.Cards.Get(ctx, cardID)
				_, _ = store.Cards.GetSummary(ctx, cardID)
				_, _ = store.Cards.AllForUser(ctx, userID)
				_, _ = store.Cards.AllForUserSummary(ctx, userID)
				_ = store.Cards.Update(ctx, &CardFull{ID: cardID, Name: "改", Type: "data", DataContent: "x", Enabled: true, UserID: userID})
				_, _ = store.Cards.UpdateIfDataUnchanged(ctx, &CardFull{ID: cardID, Name: "改2", Type: "data", DataContent: "y", Enabled: true, UserID: userID}, "x")
				_ = store.Cards.UpdateDataMetadata(ctx, &CardFull{ID: cardID, Name: "改3", Type: "data", Enabled: true, UserID: userID})
				return store.Cards.Delete(ctx, cardID)
			}
		}},
	})
}

// TestFaultInjectionItemsOrdersMessaging 在商品、订单、批量发布、通知、设置、AI/默认回复与续期仓储的每个数据库操作点注入故障。
func TestFaultInjectionItemsOrdersMessaging(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("重型故障注入遍历在竞态检测模式下耗时过长，常规模式已覆盖")
	}
	runFaultScenarios(t, []faultScenario{
		{"Items", func(t *testing.T, store *Store) func(context.Context) error {
			// userID、cookieID 是账号归属。
			userID, cookieID := seedAccount(t, store)
			return func(ctx context.Context) error {
				// row 是待写入商品。
				row := ItemInfoRow{CookieID: cookieID, ItemID: "it-1", ItemTitle: "标题", IsMultiSpec: true}
				_ = store.Items.Upsert(ctx, &row)
				_ = store.Items.UpsertBasic(ctx, &ItemInfoRow{CookieID: cookieID, ItemID: "it-2", ItemTitle: "T2"})
				_, _ = store.Items.SyncFromRemote(ctx, cookieID, []ItemInfoRow{{ItemID: "it-1", ItemTitle: "A", IsMultiSpec: true}, {ItemID: "it-3", ItemTitle: "C"}})
				_, _ = store.Items.SavePageFromRemote(ctx, cookieID, []ItemInfoRow{{ItemID: "it-4", ItemTitle: "D", IsMultiSpec: true}})
				_ = store.Items.SetMultiSpec(ctx, cookieID, "it-1", true)
				_ = store.Items.SetMultiQuantity(ctx, cookieID, "it-1", true)
				_, _ = store.Items.AllForCookie(ctx, cookieID)
				_, _ = store.Items.GetByCookieItem(ctx, cookieID, "it-1")
				_, _ = store.Items.ExistsByCookieItem(ctx, cookieID, "it-1")
				_, _ = store.Items.ListForUser(ctx, userID, cookieID)
				_, _ = store.Items.Get(ctx, cookieID, "it-1")
				_ = store.Items.IsMultiSpec(ctx, cookieID, "it-1")
				_, _ = store.Items.MultiSpecFlags(ctx, cookieID, []string{"it-1", "it-4"})
				_ = store.Items.MultiQuantityDelivery(ctx, cookieID, "it-1")
				_, _ = store.Items.AllTitles(ctx)
				_ = store.ItemReps.Set(ctx, cookieID, "it-1", "回复")
				_, _ = store.ItemReps.Get(ctx, cookieID, "it-1")
				_, _ = store.ItemReps.AllForUser(ctx, cookieID)
				_ = store.ItemReps.Delete(ctx, cookieID, "it-1")
				return store.Items.Delete(ctx, cookieID, "it-1")
			}
		}},
		{"Orders", func(t *testing.T, store *Store) func(context.Context) error {
			// userID、cookieID 是账号归属。
			userID, cookieID := seedAccount(t, store)
			// status、amount 是补丁字段取值。
			status, amount := "completed", "9.90"
			return func(ctx context.Context) error {
				_ = store.Orders.Upsert(ctx, "o1", OrderUpsertOpts{CookieID: cookieID, ItemID: "i", BuyerID: "b", OrderStatus: "pending_ship", Amount: "1.00", Quantity: "1", ChatID: "c"})
				_ = store.Orders.Patch(ctx, "o1", OrderPatch{OrderStatus: &status, Amount: &amount})
				_ = store.Orders.UpsertMany(ctx, []BatchOrderUpsert{{OrderID: "o2", Options: OrderUpsertOpts{CookieID: cookieID, ItemID: "i", BuyerID: "b", OrderStatus: "pending_ship"}}})
				_, _ = store.Orders.Get(ctx, "o1")
				_, _ = store.Orders.FindByIDs(ctx, []string{"o1", "o2"})
				_, _ = store.Orders.FindByIDsForAccount(ctx, cookieID, []string{"o1"})
				_, _, _ = store.Orders.ListForUser(ctx, OrderListFilter{UserID: userID, CookieID: cookieID, Status: "completed", Search: "o", Limit: 10})
				_, _ = store.Orders.ByCookie(ctx, cookieID, 10)
				_, _ = store.Orders.ByCookiePage(ctx, cookieID, 10, 0)
				_, _ = store.Orders.ByCookieCursor(ctx, cookieID, 10, "", "")
				_, _ = store.Orders.FindLatestPendingByChat(ctx, cookieID, "c", "b", "i")
				// ownership 是订单归属快照。
				ownership, _ := store.Orders.FindOwnership(ctx, userID, "o2")
				_ = store.Orders.RecoverSoldOwnership(ctx, userID, cookieID, ownership, OrderUpsertOpts{CookieID: cookieID, ItemID: "i", BuyerID: "b", OrderStatus: "pending_ship"})
				_, _ = store.Orders.SoftDeleteMissingForCookie(ctx, cookieID, map[string]struct{}{"o1": {}})
				_, _ = store.Orders.SoftDelete(ctx, "o1")
				_, _ = store.Reconciliations.CreatePending(ctx, "o2", cookieID, "kind", "msg")
				// pending 是待对账记录。
				pending, _ := store.Reconciliations.ListPending(ctx, 10)
				if len(pending) > 0 {
					_ = store.Reconciliations.RecordAttempt(ctx, pending[0].ID, "尝试")
					_ = store.Reconciliations.MarkResolved(ctx, pending[0].ID, "完成")
				}
				// 订单刷新作业生命周期。
				_ = store.OrderRefreshJobs.Create(ctx, &OrderRefreshJob{ID: "job1", UserID: userID, CookieID: cookieID, FilterStatus: "all", Status: "pending"})
				_, _ = store.OrderRefreshJobs.Get(ctx, userID, "job1")
				_, _ = store.OrderRefreshJobs.Claim(ctx, "job1", "tok", time.Now().Add(time.Hour).Unix())
				_, _ = store.OrderRefreshJobs.Recoverable(ctx, time.Now().Unix()+7200, 10)
				_, _ = store.OrderRefreshJobs.RequeueExpired(ctx, "job1", time.Now().Unix()+7200)
				_, _ = store.OrderRefreshJobs.Complete(ctx, "job1", "tok", "completed", "{}", "")
				_, err := store.OrderRefreshJobs.Cancel(ctx, userID, "job1")
				return err
			}
		}},
		{"PublishBatches", func(t *testing.T, store *Store) func(context.Context) error {
			// userID、cookieID 是账号归属。
			userID, cookieID := seedAccount(t, store)
			return func(ctx context.Context) error {
				// batch 是待创建的批次。
				batch := &ItemPublishBatch{ID: "batch-1", UserID: userID, DefaultCookieID: cookieID, Filename: "f.xlsx", UploadDir: "/tmp/x", LocationJSON: `{}`, Status: "pending"}
				_ = store.PublishBatches.Create(ctx, batch, []ItemPublishBatchRow{{RowNo: 1, CookieID: cookieID, Title: "a", Price: "1", Quantity: 1, ImagesJSON: `[]`, Status: "pending"}, {RowNo: 2, CookieID: cookieID, Title: "b", Price: "2", Quantity: 1, ImagesJSON: `[]`, Status: "pending"}})
				_, _ = store.PublishBatches.Get(ctx, userID, "batch-1")
				_, _ = store.PublishBatches.ListForUser(ctx, userID, 10)
				_, _ = store.PublishBatches.Rows(ctx, "batch-1")
				_, _ = store.PublishBatches.PendingRows(ctx, "batch-1", false)
				_, _ = store.PublishBatches.PendingRows(ctx, "batch-1", true)
				_, _ = store.PublishBatches.ClaimBatch(ctx, "batch-1", "tok", time.Now().Add(time.Hour).Unix())
				_, _ = store.PublishBatches.RenewBatchLease(ctx, "batch-1", "tok", time.Now().Add(2*time.Hour).Unix())
				_, _ = store.PublishBatches.ReservePublishSlot(ctx, "batch-1", "tok", 0, time.Now().UnixMilli())
				_, _ = store.PublishBatches.ClaimRow(ctx, 1, "tok")
				_, _ = store.PublishBatches.MarkClaimedRemoteStarted(ctx, 1, "tok")
				_, _ = store.PublishBatches.SaveClaimedRemoteResult(ctx, 1, "tok", "it", "http://u", "{}")
				_, _ = store.PublishBatches.MarkClaimedRowSuccess(ctx, 1, "tok", "it", "http://u", "{}")
				_, _ = store.PublishBatches.ClaimRow(ctx, 2, "tok")
				_, _ = store.PublishBatches.MarkClaimedRowFailed(ctx, 2, "tok", "失败", "publish")
				_ = store.PublishBatches.MarkRowRunning(ctx, 2)
				_ = store.PublishBatches.MarkRowFailedKind(ctx, 2, "失败", "validation")
				_ = store.PublishBatches.MarkRowFailed(ctx, 2, "失败")
				_ = store.PublishBatches.MarkRowSuccess(ctx, 2, "it2", "http://u2", "{}")
				_ = store.PublishBatches.ResetFailed(ctx, "batch-1")
				_ = store.PublishBatches.MarkRunningFailed(ctx, "batch-1", "中断")
				_ = store.PublishBatches.MarkUnfinishedFailed(ctx, "batch-1", "未完成")
				_ = store.PublishBatches.Recount(ctx, "batch-1")
				_, _, _ = store.PublishBatches.FinalizeBatch(ctx, "batch-1", "tok")
				_, _ = store.PublishBatches.BatchStatus(ctx, "batch-1")
				_, _ = store.PublishBatches.FinishBatchStatus(ctx, "batch-1", "tok", "completed")
				_, _ = store.PublishBatches.FailClaimedBatch(ctx, "batch-1", "tok")
				_ = store.PublishBatches.SetBatchStatus(ctx, "batch-1", "running")
				_ = store.PublishBatches.ResetInterrupted(ctx, "batch-1")
				_, _ = store.PublishBatches.Recoverable(ctx, time.Now().Unix()+7200, 10)
				_, _, _ = store.PublishBatches.RequestCancel(ctx, "batch-1")
				_, _ = store.PublishBatches.FinalizeCanceled(ctx, "batch-1", "tok")
				_, _, _ = store.PublishBatches.FinalizeInterrupted(ctx, "batch-1", "tok", "中断")
				_, _ = store.PublishBatches.FinalizeExpiredCancellation(ctx, "batch-1", time.Now().Unix()+7200)
				_, _ = store.PublishBatches.ExpiredUploads(ctx, "2999-01-01 00:00:00", 10)
				_ = store.PublishBatches.ClearUploadDir(ctx, "batch-1")
				return store.PublishBatches.Delete(ctx, userID, "batch-1")
			}
		}},
		{"NotificationsAndSettings", func(t *testing.T, store *Store) func(context.Context) error {
			// userID、cookieID 是账号归属。
			userID, cookieID := seedAccount(t, store)
			// ctx 是准备阶段上下文。
			ctx := context.Background()
			// channelID 是预置通知渠道。
			channelID, err := store.Notifications.CreateChannel(ctx, &NotificationChannelRow{Name: "渠道", Type: "webhook", Config: `{"url":"http://127.0.0.1:1/x"}`, Enabled: true, UserID: userID})
			if err != nil {
				t.Fatal(err)
			}
			return func(ctx context.Context) error {
				_, _ = store.Notifications.CreateChannel(ctx, &NotificationChannelRow{Name: "渠道2", Type: "webhook", Config: `{"url":"http://127.0.0.1:1/y"}`, Enabled: true, UserID: userID})
				_, _ = store.Notifications.AllChannelsForUser(ctx, userID)
				_, _ = store.Notifications.ListChannelSummariesForUser(ctx, userID)
				_, _ = store.Notifications.GetChannelRowForUser(ctx, channelID, userID)
				_, _ = store.Notifications.GetChannel(ctx, channelID)
				_, _ = store.Notifications.OwnsChannel(ctx, channelID, userID)
				_ = store.Notifications.UpdateChannel(ctx, &NotificationChannelRow{ID: channelID, Name: "改", Type: "webhook", Config: `{"url":"http://127.0.0.1:1/z"}`, Enabled: true})
				_ = store.Notifications.UpdateChannelForUser(ctx, &NotificationChannelRow{ID: channelID, Name: "改2", Type: "webhook", Config: `{"url":"http://127.0.0.1:1/w"}`, Enabled: true}, userID)
				_ = store.Notifications.SetBindings(ctx, cookieID, []int64{channelID})
				_ = store.Notifications.SetSingleBinding(ctx, cookieID, channelID, true)
				_, _ = store.Notifications.AccountBindings(ctx, cookieID)
				_, _ = store.Notifications.AccountChannels(ctx, cookieID)
				_, _ = store.Notifications.ListBindingsForUser(ctx, userID)
				_ = store.Notifications.EnqueueOutbox(ctx, []NotificationOutboxInput{{ChannelID: channelID, EventType: "evt", Body: "体", IdempotencyKey: "idem-1"}})
				// claimed 是被领取的出站消息。
				claimed, _ := store.Notifications.ClaimOutbox(ctx, "wtok", time.Now().Add(time.Hour), 5)
				if len(claimed) > 0 {
					_, _ = store.Notifications.RetryOutbox(ctx, claimed[0].ID, "wtok", "稍后", time.Now().Unix(), false)
					_, _ = store.Notifications.MarkOutboxUncertain(ctx, claimed[0].ID, "wtok", "不确定")
					_, _ = store.Notifications.CompleteOutbox(ctx, claimed[0].ID, "wtok")
				}
				_, _ = store.Notifications.ListUncertainOutboxForUser(ctx, userID, 10)
				_, _ = store.Notifications.ListUncertainOutboxForAdmin(ctx, 10)
				_, _ = store.Notifications.CountUncertainOutboxForUser(ctx, userID)
				_, _ = store.Notifications.CountUncertainOutboxForAdmin(ctx)
				_ = store.Notifications.DeleteBinding(ctx, userID, 1)
				_ = store.Notifications.DeleteAccountBindings(ctx, userID, cookieID)
				_ = store.Notifications.DeleteChannelForUser(ctx, channelID, userID)
				_ = store.Notifications.DeleteChannel(ctx, channelID)
				// 系统设置。
				_ = store.Settings.Set(ctx, "k1", "v1")
				_ = store.Settings.SetMany(ctx, map[string]string{"k2": "v2", "k3": "v3"})
				_ = store.Settings.ApplyChanges(ctx, map[string]string{"k4": "v4"}, map[string]SensitiveSettingChange{"smtp_password": {Action: "replace", Value: "fake-secret"}})
				_ = store.Settings.ApplyChanges(ctx, nil, map[string]SensitiveSettingChange{"smtp_password": {Action: "clear"}})
				_, _ = store.Settings.Get(ctx, "k1")
				_, _ = store.Settings.All(ctx)
				_, _ = store.Settings.Redacted(ctx)
				_, _ = store.Settings.Public(ctx)
				_, _ = store.ReadSensitiveSetting(ctx, userID, "smtp_password", "read", "settings")
				_, _ = store.ReadSensitiveSettingForAccount(ctx, cookieID, "smtp_password", "read", "settings")
				_ = store.SecurityAudit.Add(ctx, SecurityAuditLog{UserID: userID, Action: "read", Resource: "settings", Keys: []string{"k"}, Outcome: "ok"})
				_, _ = store.SecurityAudit.ListByUser(ctx, userID, 10)
				_, _ = store.UserSettings.AllForUser(ctx, userID)
				return nil
			}
		}},
		{"AIAndReplies", func(t *testing.T, store *Store) func(context.Context) error {
			// userID、cookieID 是账号归属。
			userID, cookieID := seedAccount(t, store)
			return func(ctx context.Context) error {
				_ = store.AIReply.UpsertSettings(ctx, cookieID, AIReplySettings{AIEnabled: true, AutoAdjustPriceEnabled: false, ModelName: "m", APIKey: "fake-key", BaseURL: "http://127.0.0.1:1", MaxBargainRounds: 3, AIMode: "reply"})
				_, _ = store.AIReply.IsEnabled(ctx, cookieID)
				_, _, _ = store.AIReply.PricingMode(ctx, cookieID)
				_, _ = store.AIReply.Get(ctx, cookieID)
				_, _ = store.AIReply.ListForUser(ctx, userID)
				_ = store.AIReply.ReplacePendingQuote(ctx, AIBargainQuote{CookieID: cookieID, ChatID: "c", BuyerID: "b", ItemID: "i", PriceCents: 100}, time.Now().Add(time.Hour).Unix())
				// quote 是被领取的议价报价。
				quote, _ := store.AIReply.ClaimPendingQuote(ctx, cookieID, "c", "b", "i", "o", time.Now().Unix())
				if quote != nil {
					_ = store.AIReply.FinishQuote(ctx, quote.ID, "used", "")
				}
				_ = store.AIReply.AddConversation(ctx, cookieID, "c", "u", "i", AIConversationMessage{Role: "user", Content: "hi"})
				_ = store.AIReply.AddConversationExchange(ctx, cookieID, "c", "u", "i", AIConversationMessage{Role: "user", Content: "hi"}, AIConversationMessage{Role: "assistant", Content: "yo", BargainCount: 1})
				_, _ = store.AIReply.ConversationHistory(ctx, cookieID, "c", "i", 5)
				_, _ = store.AIReply.CurrentBargainCount(ctx, cookieID, "c", "i")
				_ = store.DefaultReps.Upsert(ctx, cookieID, DefaultReply{Enabled: true, ReplyContent: "r", ReplyOnce: true})
				_, _ = store.DefaultReps.Get(ctx, cookieID)
				_, _ = store.DefaultReps.ListForUser(ctx, userID)
				_, _, _ = store.DefaultReps.ClaimRecord(ctx, cookieID, "c", true, true)
				_ = store.DefaultReps.MarkPartSent(ctx, cookieID, "c", "text")
				_ = store.DefaultReps.MarkRecordUncertain(ctx, cookieID, "c", "不确定")
				_ = store.DefaultReps.MarkRecordFailed(ctx, cookieID, "c", "失败")
				_ = store.DefaultReps.MarkRecordSent(ctx, cookieID, "c")
				_ = store.DefaultReps.AddRecord(ctx, cookieID, "c2")
				_, _ = store.DefaultReps.Record(ctx, cookieID, "c")
				_ = store.DefaultReps.HasRecord(ctx, cookieID, "c")
				_ = store.DefaultReps.ClearRecords(ctx, cookieID)
				return store.DefaultReps.Delete(ctx, cookieID)
			}
		}},
		{"RenewalAndTokens", func(t *testing.T, store *Store) func(context.Context) error {
			// cookieID 是账号。
			_, cookieID := seedAccount(t, store)
			return func(ctx context.Context) error {
				_ = store.Cookies.UpdateRenewalCookie(ctx, cookieID, "cv=renewed", `{"k":"v"}`, 100)
				_ = store.Renewal.UpsertCookieRefreshSchedule(ctx, CookieRefreshSchedule{CookieID: cookieID, ExpireAt: 200, ConsecutiveFailures: 1, LastError: "e", LastStatus: "failed"})
				_, _ = store.Renewal.GetCookieRefreshSchedule(ctx, cookieID)
				_ = store.Renewal.AddAPICookieRenewLog(ctx, RenewalLog{BatchID: "b", CookieID: cookieID, Status: "success", UpdatedCookieNames: []string{"a"}, UpdatedCookieCount: 1})
				_ = store.Renewal.AddBrowserCookieRenewLog(ctx, RenewalLog{BatchID: "b", CookieID: cookieID, Status: "failed", ErrorMessage: "e"})
				_ = store.Renewal.AddLoginRenewLog(ctx, RenewalLog{BatchID: "b", CookieID: cookieID, Status: "success"})
				_, _ = store.Renewal.RecentAPICookieRenewStatuses(ctx, cookieID, 5)
				_, _ = store.Renewal.RecentBrowserCookieRenewStatuses(ctx, cookieID, 5)
				_ = store.Renewal.CleanupLogs(ctx, 1)
				_, _ = store.Cookies.ActiveRenewalRuntimeAccounts(ctx)
				_, _ = store.Cookies.GetRenewalRuntimeAccount(ctx, cookieID)
				_ = store.Tokens.SaveBound(ctx, cookieID, "dev-1", "fake-token", 1, "fp")
				_ = store.Tokens.Save(ctx, cookieID, "dev-1", "fake-token", 2)
				_, _ = store.Tokens.Get(ctx, cookieID)
				_, _ = store.Tokens.GetOrCreateDeviceID(ctx, cookieID, "dev-2")
				return store.Tokens.Clear(ctx, cookieID)
			}
		}},
		{"SecretsMigration", func(t *testing.T, store *Store) func(context.Context) error {
			// userID、cookieID 是账号归属。
			userID, cookieID := seedAccount(t, store)
			// ctx 是准备阶段上下文。
			ctx := context.Background()
			// 写入历史明文形态数据，使迁移逐项触及各表。
			for _, statement := range []struct {
				query string
				args  []any
			}{
				{`INSERT INTO cards (name,type,api_config,user_id) VALUES (?,?,?,?)`, []any{"legacy-api", "api", `{"url":"https://example.com","headers":{"Authorization":"legacy-fake"}}`, userID}},
				{`INSERT INTO notification_channels (name,type,config,enabled,user_id) VALUES (?,?,?,?,?)`, []any{"legacy", "webhook", `{"url":"http://127.0.0.1:1/legacy"}`, 1, userID}},
				{`INSERT INTO system_settings (key,value) VALUES (?,?)`, []any{"smtp_password", "legacy-fake-secret"}},
			} {
				// 个别表结构在方言间有差异时忽略失败，不影响迁移入口的执行。
				_, _ = store.DB.ExecContext(ctx, statement.query, statement.args...)
			}
			_ = cookieID
			return func(ctx context.Context) error { return store.EncryptLegacySecrets(ctx) }
		}},
	})
}
