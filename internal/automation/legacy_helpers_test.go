package automation

import (
	"context"
	"xianyu-go/internal/db"
)

// 本文件保存只被测试使用的历史薄包装：生产代码已改走带凭证/租约的正式入口，测试仍通过它们覆盖同一实现。

// executeAction 执行一个动作，并把消息发送错误分类为可安全重试或结果未知。
func (e *automationActionExecutor) executeAction(ctx context.Context, task Task, action db.AutomationAction) (int, error) {
	// result 保存不带发货凭证的兼容动作结果；直接调用该入口的旧调用方不附加确认发货内容。
	result, err := e.executeActionWithProof(ctx, task, action, shipmentDeliveryProof{})
	return result.sent, err
}

// confirmShipment 根据账号设置和任务强制标记决定是否确认订单发货。
func (e *automationActionExecutor) confirmShipment(ctx context.Context, task Task) error {
	return e.confirmShipmentWithProof(ctx, task, shipmentDeliveryProof{})
}

// sendCard 按规格和购买数量分配文本、图片或数据卡密。
func (e *automationActionExecutor) sendCard(ctx context.Context, task Task, action db.AutomationAction) (int, error) {
	// result 保存卡密动作的发送数量和短暂凭证；旧入口只返回数量以保持测试及兼容调用方稳定。
	result, err := e.sendCardWithProof(ctx, task, action)
	return result.sent, err
}

// scanPendingShipDeliveries 兜底扫描没有付款运行的待发货订单并补触发自动发货。
func (s *Scheduler) scanPendingShipDeliveries(ctx context.Context) {
	if ctx == nil {
		return
	}
	// scanCtx、cancel 为直接调用入口提供独立预算及释放函数。
	scanCtx, cancel := context.WithTimeout(ctx, s.pendingShipScanBudgetValue())
	defer cancel()
	_ = s.scanPendingShipDeliveriesWithContextAndLimit(scanCtx, s.pendingShipScanMaxTasksValue())
}

// scanPendingShipResumes 兜底续跑「有运行但剩余动作全部为幂等状态动作」的待发货订单。
//
// 存在意义：运行可能在没有完成全部动作的情况下收口（例如消息动作结果不确定被隔离，
// 或收口过程中进程退出）。这类订单不会被 PendingShipOrdersWithoutPaidRunAfter 选中
// （它已经有一条运行），于是永久停在待发货，且没有任何重试入口。
//
// 安全边界：续跑集合由 PendingShipResumableRunsAfter 限定为「游标已越过全部发卡/模板动作」，
// 因此这里实际执行的只可能是 confirm_shipment 这类不会再次联系买家、且平台侧幂等的动作。
func (s *Scheduler) scanPendingShipResumes(ctx context.Context) {
	if ctx == nil {
		return
	}
	// scanCtx 为直接调用该扫描入口时提供的独立单轮预算；正式调度由 scan 传入共享预算上下文。
	scanCtx, cancel := context.WithTimeout(ctx, s.pendingShipScanBudgetValue())
	defer cancel()
	s.scanPendingShipResumesWithContext(scanCtx)
}

// scanPendingShipResumesWithContext 在给定预算内扫描并续跑尚未完成的待发货运行。
func (s *Scheduler) scanPendingShipResumesWithContext(ctx context.Context) {
	// remainingTasks 保存本次直接调用可使用的待发货任务额度。
	remainingTasks := s.pendingShipScanMaxTasksValue()
	_ = s.scanPendingShipResumesWithContextAndLimit(ctx, remainingTasks)
}

// executeAction 将具体动作委托给发货动作执行器。
func (c *Center) executeAction(ctx context.Context, task Task, action db.AutomationAction) (int, error) {
	return c.actions.executeAction(ctx, task, action)
}

// confirmShipment 将确认发货委托给发货动作执行器。
func (c *Center) confirmShipment(ctx context.Context, task Task) error {
	return c.actions.confirmShipment(ctx, task)
}

// sendCard 将卡密发送委托给发货动作执行器。
func (c *Center) sendCard(ctx context.Context, task Task, action db.AutomationAction) (int, error) {
	return c.actions.sendCard(ctx, task, action)
}

// cardContent 获取卡密组内容的兼容入口。
func (c *Center) cardContent(ctx context.Context, card *db.CardFull) (text, imageURL string, err error) {
	return c.actions.cardContent(ctx, card)
}

// sendImage 将图片消息发送委托给发货动作执行器。
func (c *Center) sendImage(ctx context.Context, task Task, imageURL string, cardID int64) error {
	return c.actions.sendImage(ctx, task, imageURL, cardID)
}
