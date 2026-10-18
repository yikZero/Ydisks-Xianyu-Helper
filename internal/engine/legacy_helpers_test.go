package engine

import "time"

// 本文件保存只供测试调用的同步消息分发入口；生产路径经 messageDispatcher.dispatch 异步进入同一实现。

// handleMessage 分类并投递消息。
func (a *Account) handleMessage(decrypted map[string]any) {
	a.messageDispatcher.handleMessage(decrypted)
}

// markAndCheckDedup 提取消息 ID，检查 1 小时内是否已处理；未处理则标记。
// 返回 true 表示应继续处理。移植自 _schedule_debounced_reply 的去重段。
func (a *Account) markAndCheckDedup(decrypted map[string]any, chat *ChatMessage) bool {
	return a.messageDispatcher.markAndCheckDedup(decrypted, chat)
}

func (a *Account) cleanupDedupLocked(now time.Time) {
	a.messageDispatcher.cleanupDedupLocked(now)
}

// scheduleDebouncedReply 为 chat_id 调度防抖回复：
// 同一 chat_id 连续来消息时取消旧定时器、用最新消息重新计时，1s 后投递最后一条。
func (a *Account) scheduleDebouncedReply(chat ChatMessage) {
	a.messageDispatcher.scheduleDebouncedReply(chat)
}

// handleMessage 分类并投递消息，供 Account facade 和测试调用。
func (d *messageDispatcher) handleMessage(decrypted map[string]any) {
	// ctx、finish、accepted 分别是同步分发任务的上下文、释放函数及生命周期接纳结果。
	ctx, finish, accepted := d.beginTask()
	if !accepted {
		return
	}
	defer finish()
	d.handleMessageContext(ctx, decrypted)
}
