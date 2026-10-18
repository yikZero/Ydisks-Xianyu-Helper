package server

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// 本文件保存只被测试使用的历史薄包装：生产代码已改走带凭证/租约的正式入口，测试仍通过它们覆盖同一实现。

// start 登记任务并返回任务 ID 与幂等收束回调。
// ctx 是任务的取消和超时来源；complete 必须在任务函数退出时调用，重复调用不会覆盖首个终态。
func (r *taskRegistry) start(name string, ctx context.Context) (string, func(error)) {
	if r == nil {
		r = newTaskRegistry()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// taskID 是进程内唯一标识，便于日志与管理端状态查询关联。
	taskID := fmt.Sprintf("srv-task-%d", atomic.AddUint64(&r.nextID, 1))
	// startedAt 记录任务实际进入 goroutine 前的登记时间。
	startedAt := time.Now().UTC()
	// snapshot 是新任务的初始状态快照。
	snapshot := taskStatusSnapshot{ID: taskID, Name: name, State: taskStateRunning, StartedAt: startedAt}
	// deadline、hasDeadline 分别表示 Context 的截止时间及其是否存在截止时间。
	if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
		// deadlineAt 保存 Context 的截止时间，帮助运维识别尚未收束的超时任务。
		deadlineAt := deadline.UTC()
		snapshot.DeadlineAt = &deadlineAt
	}
	r.mu.Lock()
	r.tasks[taskID] = &trackedTask{snapshot: snapshot, ctx: ctx}
	r.order = append(r.order, taskID)
	r.pruneLocked()
	r.mu.Unlock()
	// complete 负责把任务函数的退出结果转换为统一状态；它可被 defer 安全调用。
	complete := func(taskErr error) {
		r.finish(taskID, taskErr)
	}
	return taskID, complete
}

// finish 将任务写入首个终态；错误正文不会进入注册表，避免敏感信息通过管理端泄露。
func (r *taskRegistry) finish(taskID string, taskErr error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// task 保存待收束的任务记录。
	task, ok := r.tasks[taskID]
	if !ok || task.snapshot.State != taskStateRunning {
		return
	}
	// finishedAt 记录任务函数退出的 UTC 时间。
	finishedAt := time.Now().UTC()
	task.snapshot.FinishedAt = &finishedAt
	if taskErr != nil {
		task.snapshot.State = taskStateFailed
		return
	}
	task.snapshot.State = stateForContext(task.ctx)
}

// pruneLocked 删除最早的已完成历史，调用方必须持有 r.mu 写锁。
func (r *taskRegistry) pruneLocked() {
	for len(r.order) > r.maxHistory {
		// candidateIndex 优先指向最早已完成记录，避免正常任务被运行中的旧任务阻塞清理。
		candidateIndex := -1
		// index、taskID 分别表示历史顺序位置和对应任务标识。
		for index, taskID := range r.order {
			// task、exists 表示当前标识对应的任务及其是否仍在注册表中。
			task, exists := r.tasks[taskID]
			if !exists || task.snapshot.State != taskStateRunning {
				candidateIndex = index
				break
			}
		}
		// 全部记录仍在运行时暂不删除，避免管理端丢失活动任务；完成后下一次登记会继续清理。
		if candidateIndex < 0 {
			return
		}
		// oldestID 是本轮选中的最早可清理任务标识。
		oldestID := r.order[candidateIndex]
		r.order = append(r.order[:candidateIndex], r.order[candidateIndex+1:]...)
		delete(r.tasks, oldestID)
	}
}
