package automation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/mtop"
)

var (
	errAutomationDeferred    = errors.New("自动化动作已持久化等待执行")
	errAutomationNeedsReview = errors.New("自动化动作结果需要人工核对")
	errAutomationQuarantine  = errors.New("自动化人工核对状态保存失败")
	errActionNotPerformed    = errors.New("自动化外部动作明确未执行")
	// ErrMessageNotSent 记录当前操作失败原因发生在调用 WebSocket 发送之前，可以安全重试。
	// engine 通过 errors.Is 标记“连接尚未就绪”等运行时状态，避免被误判为
	// 远端可能已经收到消息。
	ErrMessageNotSent = errors.New("自动化消息确定未发送")
)

type preparationError struct{ err error }

func (e *preparationError) Error() string { return e.err.Error() }

func (e *preparationError) Unwrap() error { return e.err }

type uncertainActionError struct{ err error }

func (e *uncertainActionError) Error() string { return e.err.Error() }

func (e *uncertainActionError) Unwrap() error { return e.err }

func uncertainAction(err error) error {
	if err == nil {
		return nil
	}
	var existing *uncertainActionError
	if errors.As(err, &existing) {
		return err
	}
	return &uncertainActionError{err: err}
}

// SenderProvider 根据账号 ID 提供当前在线账号的发送能力。
// 计划任务和 WS 事件都复用这个抽象，避免自动化中心直接依赖 account.Manager。
type SenderProvider interface {
	Sender(cookieID string) (MessageSender, bool)
}

// MessageSender 是自动化动作需要的最小发送接口。
type MessageSender interface {
	SendText(ctx context.Context, chatID, toUserID, text string) error
	SendImage(ctx context.Context, chatID, toUserID, imageURL string, cardID int64, width, height int) error
	UpdateCookie(cookieStr string)
}

// automationReadySender 由能够报告实时连接状态的发送器选择性实现。
// 未实现的测试或外部发送器保持向后兼容，视为已经就绪。
type automationReadySender interface {
	AutomationReady() bool
}

// OrderDetailFetcher 查询闲鱼订单详情。自动发货必须先拿到订单规格和购买数量，
// 再按规则里的规格映射发货。
type OrderDetailFetcher interface {
	FetchOrderDetail(ctx context.Context, cookieID, orderID, itemID, buyerID, cookieStr string) (*OrderDetail, error)
}

// CredentialRecoverer 仅在平台明确返回 Session 失效时执行一次账号恢复；Token 过期由 MTOP 客户端内部刷新。
type CredentialRecoverer interface {
	RecoverExpiredCredential(ctx context.Context, cookieID string) bool
}

// Notifier 发货结果通知器（多渠道）。可选依赖，未注入时跳过通知；实现必须按自动化运行和终态持久化去重。
type Notifier interface {
	// NotifyAutomationRun 将自动化运行终态写入可持久化去重的通知链路；runID 和 status 共同标识同一次结果。
	NotifyAutomationRun(ctx context.Context, runID int64, accountID, buyerID, itemID, status, message, chatID string)
}

// Center 是统一自动化处理中心。
// 它只接收已经分类好的系统事件或计划任务，不处理用户消息；用户消息由 engine 的回复链处理。
type Center struct {
	// facts 记录已解析的自动化事件事实，不执行规则动作。
	facts eventFactRecorder
	// rules 查询适用规则，不执行规则动作。
	rules ruleMatcher
	// planner 生成不可变动作计划，不执行外部 I/O。
	planner actionPlanner
	// runs 协调自动化运行创建、动作检查点、延迟恢复和结果隔离。
	runs automationRunCoordinator
	// actions 执行确认发货、卡密和消息动作，集中维护外部副作用边界。
	actions automationActionExecutor
	// notifications 负责把运行结果转换为可选的发货通知。
	notifications deliveryNotifier
	// taskRunner 协调账号任务执行、凭证阻断和账号状态门禁。
	taskRunner accountTaskCoordinator
	store      *db.Store
	senders    SenderProvider
	logger     *slog.Logger
	// dependencies 拥有生产构造时固定的外部依赖快照，运行期间不可替换。
	dependencies centerDependencies
}

// centerDependencies 集中拥有自动化中心构造后不可变的外部依赖。
type centerDependencies struct {
	// mtop 是确认发货使用的 MTOP 客户端。
	mtop mtop.Client
	// accountTaskClient 是账号任务使用的平台客户端。
	accountTaskClient AccountTaskClient
	// fetcher 是订单详情查询器。
	fetcher OrderDetailFetcher
	// recoverer 是凭证恢复器。
	recoverer CredentialRecoverer
	// notifier 是发货结果通知器。
	notifier Notifier
	// cookieSrc 是构造期注入的 Cookie 读取函数。
	cookieSrc func(context.Context, string) (string, error)
	// apiFetcher 提供普通 API 卡发货请求能力。
	apiFetcher APICardFetcher
}

// New 构造使用默认协议实现的自动化中心。
func New(store *db.Store, senders SenderProvider, logger *slog.Logger) *Center {
	return NewWithDependencies(store, senders, logger, CenterDependencies{})
}

// NewWithDependencies 构造依赖固定的自动化中心，生产装配应通过该入口注入外部能力。
func NewWithDependencies(store *db.Store, senders SenderProvider, logger *slog.Logger, dependencies CenterDependencies) *Center {
	if logger == nil {
		logger = slog.Default()
	}
	// client 保存确认发货使用的固定 MTOP 客户端。
	client := dependencies.MTop
	// defaultTaskClient 保存默认 MTOP 实现同时提供的账号任务能力。
	var defaultTaskClient AccountTaskClient
	if client == nil {
		// defaultClient 保存默认协议客户端的具体实现，供两个消费者共享。
		defaultClient := mtop.NewClient()
		client = defaultClient
		defaultTaskClient = defaultClient
	}
	// accountTaskClient 保存自动评价和商品擦亮使用的固定协议客户端。
	accountTaskClient := dependencies.AccountTaskClient
	if accountTaskClient == nil {
		accountTaskClient = defaultTaskClient
	}
	center := &Center{
		facts:   newEventFactRecorder(store),
		rules:   newRuleMatcher(store),
		planner: actionPlanner{},
		store:   store,
		senders: senders,
		dependencies: centerDependencies{
			mtop:              client,
			accountTaskClient: accountTaskClient,
			fetcher:           dependencies.OrderDetailFetcher,
			notifier:          dependencies.Notifier,
			cookieSrc:         dependencies.CookieSource,
			apiFetcher:        dependencies.APICardFetcher,
		},
		logger: logger.With("subsys", "automation"),
	}
	// recoverer、ok 保存订单详情查询器提供的凭证恢复能力及类型判断结果
	if recoverer, ok := center.dependencies.fetcher.(CredentialRecoverer); ok {
		center.dependencies.recoverer = recoverer
	}
	center.actions = automationActionExecutor{
		store:   store,
		senders: senders,
		mtop: func() mtop.Client {
			return center.dependencies.mtop
		},
		recoverer: func() CredentialRecoverer {
			return center.dependencies.recoverer
		},
		logger: center.logger,
		cookieSource: func(ctx context.Context, cookieID string) (string, error) {
			// source 是构造期固定的 Cookie 读取函数；为空时回退到仓储。
			if source := center.dependencies.cookieSrc; source != nil {
				return source(ctx, cookieID)
			}
			return center.store.Cookies.GetValue(ctx, cookieID)
		},
		apiFetcher:            func() APICardFetcher { return center.dependencies.apiFetcher },
		wakeCredentialBlocked: center.wakeCredentialBlockedAutomation,
	}
	center.notifications = deliveryNotifier{
		current: func() Notifier {
			return center.dependencies.notifier
		},
		logger: center.logger,
	}
	center.taskRunner = accountTaskCoordinator{
		repository: newStoreAccountTaskRepository(store),
		client:     func() AccountTaskClient { return center.dependencies.accountTaskClient },
		senders:    senders,
		recoverer: func() CredentialRecoverer {
			return center.dependencies.recoverer
		},
		logger: center.logger,
	}
	center.runs = automationRunCoordinator{
		store:                    store,
		planner:                  center.planner,
		logger:                   center.logger,
		prepareTask:              center.prepareTask,
		actionDelaySeconds:       center.actionDelaySeconds,
		accountAutomationAllowed: center.accountAutomationAllowed,
		accountSenderReady:       center.accountSenderReady,
		deferTask:                center.deferTask,
		executeAction:            center.executeActionWithProof,
		hasNotifier:              func() bool { return center.dependencies.notifier != nil },
		notifyResult:             center.notifyResult,
	}
	return center
}

// RunAccountTask 执行指定账号任务，并保持 Center 的公开兼容入口。
func (c *Center) RunAccountTask(ctx context.Context, accountID, taskType string) (AccountTaskSummary, error) {
	if c == nil || c.taskRunner.repository == nil {
		return AccountTaskSummary{TaskType: taskType}, errors.New("账号任务协调器未初始化")
	}
	// summary、err 保存账号任务执行结果及其错误，供日志和兼容调用方共同使用。
	summary, err := c.taskRunner.runAccountTask(ctx, accountID, taskType)
	if c.logger == nil {
		return summary, err
	}
	if err != nil {
		if errors.Is(err, errAccountTaskCredentialRenewed) {
			c.logger.Info("手动账号任务因凭证续期暂停，等待下一次执行", "account", accountID, "task", taskType, "err", err)
		} else {
			c.logger.Warn("手动账号任务执行失败", "account", accountID, "task", taskType, "err", err)
		}
		return summary, err
	}
	c.logger.Info("手动账号任务执行成功", "account", accountID, "task", taskType,
		"found", summary.Found, "success", summary.Success, "failed", summary.Failed, "skipped", summary.Skipped)
	return summary, nil
}

// scanAccountTasks 扫描启用的账号任务，并委托给账号任务协调器。
func (c *Center) scanAccountTasks(ctx context.Context) {
	c.taskRunner.scanAccountTasks(ctx)
}

// HandleTask 处理一条自动化任务。无匹配规则时安全忽略。
func (c *Center) HandleTask(ctx context.Context, task Task) error {
	_, err := c.handleTask(ctx, task)
	return err
}

func (c *Center) handleTask(ctx context.Context, task Task) (bool, error) {
	if c == nil || c.store == nil || c.store.Automation == nil {
		return false, nil
	}
	task.TriggerType = strings.TrimSpace(task.TriggerType)
	if task.TriggerType == "" || task.AccountID == "" {
		return false, nil
	}
	// 简化付款消息只有会话标识，先从本账号待发货订单回填订单事实，再记录事件和匹配规则。
	// resolveErr 保存简化消息订单事实回填错误
	if resolvedTask, resolveErr := c.resolvePaidTaskOrder(ctx, task); resolveErr != nil {
		return false, resolveErr
	} else {
		task = resolvedTask
	}
	// unknownRoleTask 标记本次 WebSocket 交易事件是否未携带明确角色；仅该类事件需要记录本地卖家事实核验的放行日志。
	unknownRoleTask := task.OrderRole == OrderRoleUnknown && isWebSocketSellerOrderTask(task)
	// verifiedTask、sellerVerified、rejectReason 保存未知角色事件的本地卖家核验结果；失败时在事实写入前停止，避免把买家订单落到卖家账号。
	verifiedTask, sellerVerified, rejectReason, roleErr := c.authorizeWebSocketSellerTask(ctx, task)
	if roleErr != nil {
		return false, roleErr
	}
	if !sellerVerified {
		if roleVerificationRetryable(rejectReason) {
			if isDeferredReplay(task) {
				// 未知角色延期任务再次没有本地证据时交给统一退避；达到上限后由调度器发送人工处理通知。
				return false, errSellerRoleEvidencePending
			}
			// deferErr 保存首条未知角色事件写入延期队列的错误；写入失败不能伪装成已安全处理。
			if deferErr := c.deferUnknownRoleTask(ctx, task, rejectReason); deferErr != nil {
				return false, deferErr
			}
			if c.logger != nil {
				c.logger.Info("未知角色发货事件已保存，等待本地卖家事实", "account", task.AccountID, "order_id", task.OrderID, "trigger", task.TriggerType, "reason", rejectReason)
			}
			return true, nil
		}
		if c.logger != nil {
			c.logger.Info("未知角色发货事件未通过本地卖家核验，未执行外部动作", "account", task.AccountID, "order_id", task.OrderID, "trigger", task.TriggerType, "reason", rejectReason)
		}
		return false, nil
	}
	if unknownRoleTask && c.logger != nil {
		c.logger.Info("未知角色交易事件已通过本地卖家核验，继续执行自动化", "account", verifiedTask.AccountID, "order_id", verifiedTask.OrderID, "item_id", verifiedTask.ItemID, "trigger", verifiedTask.TriggerType, "source", verifiedTask.Source)
	}
	task = verifiedTask
	if err := c.facts.record(ctx, task); err != nil {
		if errors.Is(err, db.ErrForbidden) {
			// 同一平台交易卡片可能同时送到卖家和买家登录账号；订单已归属其他账号时，绝不能覆盖或执行该副本。
			c.logger.Debug("跨账号重复订单事件，忽略", "account", task.AccountID, "trigger", task.TriggerType, "order_id", task.OrderID)
			return false, nil
		}
		return false, err
	}
	if task.OrderID != "" {
		if order, orderErr := c.store.Orders.Get(ctx, task.OrderID); orderErr == nil && order != nil {
			task = mergeOrderIntoTask(task, order)
		} else if orderErr != nil && !errors.Is(orderErr, db.ErrNotFound) {
			return false, fmt.Errorf("读取自动化事件订单事实: %w", orderErr)
		}
	}
	paused, until, err := c.store.Cookies.IsPaused(ctx, task.AccountID)
	if err != nil {
		return false, err
	}
	if paused {
		if err := c.deferTask(ctx, task, until); err != nil {
			return false, err
		}
		c.logger.Info("账号已暂停，自动化事件已持久化等待恢复", "account", task.AccountID, "trigger", task.TriggerType, "due_at", until)
		return true, nil
	}
	enabled, err := c.store.Cookies.Status(ctx, task.AccountID)
	if err != nil {
		return false, err
	}
	if !enabled {
		c.logger.Info("账号已停用，记录事件事实但不执行自动化", "account", task.AccountID, "trigger", task.TriggerType)
		return false, nil
	}
	if task.TriggerType == TriggerBargainPending {
		return c.handleBargainPending(ctx, task)
	}
	if task.TriggerType == TriggerOrderCompleted {
		// 买家确认收货事件只更新本地订单完成事实，自动评价由账号任务扫描该事实后独立执行。
		return true, nil
	}
	if task.TriggerType == TriggerOrderPaid && !task.ForceConfirmShipment {
		// autoDelivery、autoDeliveryErr 分别保存账号自动发货总开关（auto_confirm）和读取错误。
		autoDelivery, autoDeliveryErr := c.autoDeliveryEnabled(ctx, task.AccountID)
		if autoDeliveryErr != nil {
			return false, autoDeliveryErr
		}
		if !autoDelivery {
			c.logger.Info("账号未开启自动发货，跳过付款自动发货", "account", task.AccountID, "order_id", task.OrderID)
			return false, nil
		}
	}
	// aiPricingActive 表示订单创建事件已由互斥的 AI 议价模式接管；aiPricingErr 是报价执行或状态收口错误。
	if aiPricingActive, aiPricingErr := c.handleAIPricingMode(ctx, task); aiPricingActive || aiPricingErr != nil {
		return false, aiPricingErr
	}
	rules, err := c.rules.match(ctx, task)
	if err != nil {
		return false, err
	}
	if len(rules) == 0 {
		c.logger.Warn("未匹配商品规则或已确认的账号通用规则，未执行自动化", "trigger", task.TriggerType, "order_id", task.OrderID, "item_id", task.ItemID)
		return false, nil
	}
	var firstErr error
	for _, rule := range rules {
		if err := c.executeRule(ctx, task, rule); err != nil {
			var prepErr *preparationError
			if errors.As(err, &prepErr) && !isDeferredReplay(task) {
				dueAt := time.Now().UTC().Add(time.Minute).Unix()
				if deferErr := c.deferTaskWithError(ctx, task, dueAt, prepErr.Error()); deferErr != nil {
					return false, errors.Join(err, fmt.Errorf("持久化前置失败任务: %w", deferErr))
				}
				c.logger.Warn("自动化前置处理失败，任务已持久化等待恢复", "account", task.AccountID,
					"order_id", task.OrderID, "trigger", task.TriggerType, "retry_at", dueAt, "err", err)
				return true, nil
			}
			if errors.Is(err, errAutomationDeferred) {
				return true, nil
			}
			c.logger.Error("自动化规则执行失败", "rule_id", rule.ID, "trigger", task.TriggerType, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return false, firstErr
}

// handleBargainPending 处理砍价“待刀成”WS 阶段：仅按独立账号开关调用免拼，绝不匹配发卡或确认发货规则。
func (c *Center) handleBargainPending(ctx context.Context, task Task) (bool, error) {
	if task.Source != "ws" {
		c.logger.Warn("拒绝非 WebSocket 的免拼阶段任务", "source", task.Source, "account", task.AccountID, "order_id", task.OrderID)
		return false, nil
	}
	// autoBargain、settingsErr 保存独立自动免拼开关和读取错误。
	autoBargain, settingsErr := c.store.Cookies.GetAutoBargain(ctx, task.AccountID)
	if settingsErr != nil {
		return false, fmt.Errorf("读取自动免拼设置: %w", settingsErr)
	}
	if !autoBargain {
		c.logger.Info("账号未启用自动免拼，跳过待刀成阶段", "account", task.AccountID, "order_id", task.OrderID)
		return false, nil
	}
	if task.OrderID == "" {
		return false, fmt.Errorf("免拼阶段缺少订单ID")
	}
	// claimed、claimErr 保存本次 WS 是否取得免拼阶段唯一执行权。
	claimed, claimErr := c.store.Automation.ClaimBargainFreeShipping(ctx, task.OrderID, task.AccountID)
	if claimErr != nil {
		return false, fmt.Errorf("领取免拼阶段执行权: %w", claimErr)
	}
	if !claimed {
		c.logger.Info("免拼阶段已有其他任务处理，跳过重复事件", "account", task.AccountID, "order_id", task.OrderID)
		return false, nil
	}
	c.logger.Info("开始执行自动免拼", "account", task.AccountID, "order_id", task.OrderID, "trigger", task.TriggerType)
	// actionErr 保存独立免拼接口的执行结果。
	actionErr := c.actions.freeShipBargain(ctx, task)
	// status 保存可重试的明确失败、需要人工核对的未知结果或成功终态。
	status := "succeeded"
	if actionErr != nil {
		// uncertain 用于识别可能已被平台执行、因而不能自动再次提交的免拼结果。
		var uncertain *uncertainActionError
		if errors.As(actionErr, &uncertain) {
			status = "needs_review"
		} else {
			status = "failed"
		}
		c.logger.Warn("自动免拼失败，已保存阶段状态", "account", task.AccountID, "order_id", task.OrderID, "status", status, "err", actionErr)
	} else {
		c.logger.Info("自动免拼成功，等待成功小刀消息后发卡", "account", task.AccountID, "order_id", task.OrderID)
	}
	// finishErr 保存免拼阶段终态写入错误，避免远端成功后丢失兜底资格。
	if finishErr := c.store.Automation.FinishBargainFreeShipping(ctx, task.OrderID, task.AccountID, status); finishErr != nil {
		// reviewReason 说明阶段收口失败后为何不能再次自动免拼。
		reviewReason := "免拼请求已经执行，但本地阶段状态保存失败，禁止自动重试，请核对平台订单状态：" + finishErr.Error()
		if actionErr != nil {
			reviewReason = "免拼请求结果和本地阶段状态均无法确认，禁止自动重试，请核对平台订单状态：" + errors.Join(actionErr, finishErr).Error()
		}
		// notifyCtx 保证原始请求取消后，人工处理通知仍有独立的短时入队预算。
		notifyCtx, notifyCancel := newAutomationRunCompensationContext(ctx)
		c.notifications.notifyManualIntervention(notifyCtx, task, "二人小刀免拼", reviewReason, bargainManualInterventionKey(task))
		notifyCancel()
		if actionErr != nil {
			return false, errors.Join(actionErr, fmt.Errorf("收口免拼阶段: %w", finishErr))
		}
		return false, uncertainAction(fmt.Errorf("闲鱼已免拼，但本地阶段保存失败: %w", finishErr))
	}
	if status == "needs_review" {
		// notifyCtx 保证免拼结果不确定时的人工处理通知不受平台调用上下文取消影响。
		notifyCtx, notifyCancel := newAutomationRunCompensationContext(ctx)
		c.notifications.notifyManualIntervention(notifyCtx, task, "二人小刀免拼", actionErr.Error(), bargainManualInterventionKey(task))
		notifyCancel()
	}
	return false, actionErr
}

// bargainManualInterventionKey 返回同一账号、订单、免拼阶段共享的通知幂等键，重复 WS 不会制造重复告警。
func bargainManualInterventionKey(task Task) string {
	return fmt.Sprintf("manual-intervention:bargain-free-shipping:%s:%s", task.AccountID, task.OrderID)
}

// taskAutomationRunID 返回任务关联的持久化运行 ID；没有关联运行时返回 0。
func taskAutomationRunID(task Task) int64 {
	id, ok := rawInt64(task.Raw, "automation_run_id")
	if !ok || id < 0 {
		return 0
	}
	return id
}

// rawInt64 从 Task.Raw 读取整数字段。延期任务经 JSON 往返后数字会变成 float64，
// 不能用 fmt.Sprint 再解析：1000000 会被格式化为 "1e+06" 而解析失败。
func rawInt64(raw map[string]any, key string) (int64, bool) {
	if raw == nil {
		return 0, false
	}
	switch value := raw[key].(type) {
	case int:
		return int64(value), true
	case int64:
		return value, true
	case int32:
		return int64(value), true
	case float64:
		if value != math.Trunc(value) || math.Abs(value) > 1<<53 {
			return 0, false
		}
		return int64(value), true
	case json.Number:
		id, err := value.Int64()
		return id, err == nil
	case string:
		id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		return id, err == nil
	}
	return 0, false
}

// autoDeliveryEnabled 返回账号是否开启自动发货总开关（库字段 auto_confirm，付款后自动发卡密或发货模板）。
// 它不是发货后的“自动确认发货”开关；后者由 auto_consign 控制，在发卡成功后才读取。实时付款事件、
// 待发货兜底与运行续跑都必须先通过本开关；读取失败时返回错误，调用方不得失败开放。
// 兜底扫描必须在重开运行或领取冷却窗口之前检查它：开关关闭时 HandleTask 会直接返回而不收口运行，
// 被重开的运行会留在 running 并继续持有租约，租约到期后失败运行恢复链路直接执行 executeRule 而绕过本开关。
func (c *Center) autoDeliveryEnabled(ctx context.Context, accountID string) (bool, error) {
	// enabled、err 保存账号自动发货总开关与读取错误。
	enabled, err := c.store.Cookies.GetAutoConfirm(ctx, accountID)
	if err != nil {
		return false, fmt.Errorf("读取自动发货设置: %w", err)
	}
	return enabled, nil
}

// taskDelayCursor 返回已为其安排过延时的动作下标；没有记录时返回 -1。
func taskDelayCursor(task Task) int {
	cursor, ok := rawInt64(task.Raw, "automation_delay_cursor")
	if !ok || cursor < 0 || cursor > math.MaxInt32 {
		return -1
	}
	return int(cursor)
}

func isDeferredReplay(task Task) bool {
	return task.Raw != nil && task.Raw["automation_deferred_replay"] == true
}

func (c *Center) deferTask(ctx context.Context, task Task, dueAt int64) error {
	return c.deferTaskWithError(ctx, task, dueAt, "")
}

func (c *Center) deferTaskWithError(ctx context.Context, task Task, dueAt int64, errMsg string) error {
	key := buildTriggerKey(task)
	if key == "" {
		return fmt.Errorf("暂停期间的自动化事件缺少可持久化防重键")
	}
	task.CookieStr = ""
	raw, err := json.Marshal(task)
	if err != nil {
		return err
	}
	return c.store.Automation.DeferTask(ctx, db.DeferredAutomationTask{
		TaskKey: task.AccountID + ":" + key, CookieID: task.AccountID,
		TriggerType: task.TriggerType, TaskJSON: string(raw), DueAt: dueAt, ErrorMessage: errMsg,
	})
}

// executeRule 将规则执行委托给运行协调器，保持 Center 的兼容调用入口。
func (c *Center) executeRule(ctx context.Context, task Task, rule db.AutomationRule) error {
	return c.runs.executeRule(ctx, task, rule)
}

// executeRunActions 将动作执行委托给运行协调器，保持手动发货等调用方的兼容入口。
func (c *Center) executeRunActions(ctx context.Context, task Task, ruleID int64, run *db.AutomationRun, actions []db.AutomationAction, skipDelays bool) (int, bool, error) {
	return c.runs.executeRunActions(ctx, task, ruleID, run, actions, skipDelays)
}

// notifyResult 根据规则执行结果发送通知。成功且实际发出了内容才通知，
// 避免无匹配动作的空跑刷屏；每次通知都以运行 ID 和终态持久化去重。
func (c *Center) notifyResult(ctx context.Context, task Task, runID int64, status string, sent int, errMsg string) {
	c.notifications.notifyResult(ctx, task, runID, status, sent, errMsg)
}

func (c *Center) notifyRunNeedsReview(ctx context.Context, run db.AutomationRun, reason string) {
	if c == nil {
		return
	}
	c.notifications.notifyRunNeedsReview(ctx, run, reason)
}

// actionDelaySeconds 统一卡密默认延时和动作覆盖语义。旧动作没有
// delay_override 字段时自动使用卡密上的默认延时。
func (c *Center) actionDelaySeconds(ctx context.Context, action db.AutomationAction) (int, error) {
	if action.ActionType == ActionSendTemplate {
		return action.DelaySeconds, nil
	}
	if action.ActionType != ActionSendCard || action.CardID <= 0 {
		return action.DelaySeconds, nil
	}
	var cfg struct {
		DelayOverride bool `json:"delay_override"`
	}
	_ = json.Unmarshal([]byte(action.ConfigJSON), &cfg)
	card, err := c.store.Cards.GetSummary(ctx, action.CardID)
	if err != nil {
		return 0, err
	}
	if !card.Enabled {
		return 0, fmt.Errorf("卡密组 %d 已停用", card.ID)
	}
	if cfg.DelayOverride {
		return action.DelaySeconds, nil
	}
	return card.DelaySeconds, nil
}

func (c *Center) prepareTask(ctx context.Context, task Task) (Task, error) {
	// task、err 分别表示补全买家信息后的任务快照与准备阶段错误。
	task, err := c.prepareBuyerNickname(ctx, task)
	if err != nil {
		return task, err
	}
	if task.OrderID == "" {
		return task, nil
	}
	// upsertErr 保存自动化准备阶段订单事实写入结果；失败时禁止继续执行外部动作。
	if err := c.store.Orders.Upsert(ctx, task.OrderID, db.OrderUpsertOpts{
		CookieID: task.AccountID,
		ItemID:   task.ItemID,
		BuyerID:  task.BuyerID,
		ChatID:   task.ChatID,
	}); err != nil {
		return task, fmt.Errorf("保存自动化准备阶段订单事实: %w", err)
	}
	needsDetail := task.TriggerType == TriggerOrderPaid
	if existing, err := c.store.Orders.Get(ctx, task.OrderID); err == nil && existing != nil {
		task = mergeOrderIntoTask(task, existing)
		if needsDetail && (existing.Quantity == "" || existing.Amount == "") {
			needsDetail = true
		}
		// 规则是否多规格由 action.config_json 决定；这里无法提前知道命中的 action，
		// 因此交易类事件统一补齐规格，确保后续规格映射有事实依据。
		if needsDetail && (existing.SpecName == "" || existing.SpecValue == "") {
			needsDetail = true
		}
	}
	// fetcher 是构造期固定的订单详情查询器；执行过程中不允许替换依赖。
	fetcher := c.dependencies.fetcher
	if !needsDetail || fetcher == nil {
		return task, nil
	}
	cookieStr := task.CookieStr
	if strings.TrimSpace(cookieStr) == "" {
		var err error
		cookieStr, err = c.cookieValue(ctx, task.AccountID)
		if err != nil {
			return task, err
		}
	}
	detail, err := fetcher.FetchOrderDetail(ctx, task.AccountID, task.OrderID, task.ItemID, task.BuyerID, cookieStr)
	if err != nil {
		return task, err
	}
	if detail == nil {
		return task, nil
	}
	if detail.Quantity != "" {
		task.Quantity = detail.Quantity
	}
	if detail.SpecName != "" {
		task.SpecName = detail.SpecName
	}
	if detail.SpecValue != "" {
		task.SpecValue = detail.SpecValue
	}
	if detail.Amount != "" {
		task.Amount = detail.Amount
	}
	if detail.OrderStatus != "" {
		task.OrderStatus = detail.OrderStatus
	}
	// upsertErr 保存补齐订单详情后的事实写入结果，失败时不允许进入动作执行阶段。
	if err := c.store.Orders.Upsert(ctx, task.OrderID, db.OrderUpsertOpts{
		CookieID:    task.AccountID,
		ItemID:      task.ItemID,
		BuyerID:     task.BuyerID,
		ChatID:      task.ChatID,
		SpecName:    task.SpecName,
		SpecValue:   task.SpecValue,
		Quantity:    task.Quantity,
		Amount:      task.Amount,
		OrderStatus: task.OrderStatus,
	}); err != nil {
		return task, fmt.Errorf("保存订单详情事实: %w", err)
	}
	return task, nil
}

// executeActionWithProof 执行动作并把当前运行内已成功投递的凭证传给确认发货动作。
func (c *Center) executeActionWithProof(ctx context.Context, task Task, action db.AutomationAction, proof shipmentDeliveryProof) (actionExecutionResult, error) {
	return c.actions.executeActionWithProof(ctx, task, action, proof)
}

// wakeCredentialBlockedAutomation 在 Cookie 更新后唤醒凭证阻塞的自动化任务。
func (c *Center) wakeCredentialBlockedAutomation(ctx context.Context, accountID string) {
	if c == nil || c.store == nil || c.store.Automation == nil {
		return
	}
	if err := c.store.Automation.WakeCredentialBlocked(ctx, accountID); err != nil {
		c.logger.Warn("Cookie 更新后唤醒自动化任务失败", "account", accountID, "err", err)
	}
}

// accountAutomationAllowed 判断账号是否仍允许执行自动化动作。
func (c *Center) accountAutomationAllowed(ctx context.Context, accountID string) (bool, error) {
	return c.taskRunner.accountAutomationAllowed(ctx, accountID)
}

// accountSenderReady 判断账号是否具备可发送自动化消息的在线连接。
func (c *Center) accountSenderReady(accountID string) bool {
	if c == nil || c.senders == nil {
		return false
	}
	sender, ok := c.senders.Sender(accountID)
	if !ok {
		return false
	}
	if ready, ok := sender.(automationReadySender); ok {
		return ready.AutomationReady()
	}
	return true
}

// cookieValue 读取账号 Cookie 的兼容入口。
func (c *Center) cookieValue(ctx context.Context, cookieID string) (string, error) {
	return c.actions.cookieValue(ctx, cookieID)
}

func buildTriggerKey(task Task) string {
	if task.Raw != nil {
		// marker 是未知角色事件在订单号补齐前生成的稳定防重键；恢复时必须优先沿用它。
		if marker, ok := task.Raw[roleVerificationTaskKeyField].(string); ok && strings.TrimSpace(marker) != "" {
			return strings.TrimSpace(marker)
		}
	}
	if task.TriggerType == TriggerReviewMissingTimeout && task.OrderID != "" {
		if attempt, ok := task.Raw["attempt"]; ok {
			return fmt.Sprintf("%s:%s:%v", task.TriggerType, task.OrderID, attempt)
		}
	}
	if task.OrderID != "" {
		return task.TriggerType + ":" + task.OrderID
	}
	if task.UpdateKey != "" {
		return task.TriggerType + ":" + task.UpdateKey
	}
	return ""
}
