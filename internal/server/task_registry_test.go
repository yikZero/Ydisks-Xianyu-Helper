package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestTaskRegistryTracksCompletion 证明正常返回的后台任务会留下成功终态。
func TestTaskRegistryTracksCompletion(t *testing.T) {
	// registry 是本测试使用的内存任务注册表。
	registry := newTaskRegistry()
	// taskID、complete 是任务标识及退出时的收束回调。
	taskID, complete := registry.start("测试任务", context.Background())
	complete(nil)
	// snapshots 是任务状态快照列表。
	snapshots := registry.list()
	if len(snapshots) != 1 || snapshots[0].ID != taskID || snapshots[0].State != taskStateSucceeded {
		t.Fatalf("任务正常完成后的状态不正确: %+v", snapshots)
	}
}

// TestTaskRegistryPrunesCompletedHistoryAroundRunningTask 证明运行中的旧任务不会阻塞已完成历史清理。
func TestTaskRegistryPrunesCompletedHistoryAroundRunningTask(t *testing.T) {
	// registry 是容量缩小后的任务注册表，便于稳定触发清理。
	registry := newTaskRegistry()
	registry.maxHistory = 2
	// runningID 保留一个长期运行任务，验证它前面的完成记录仍可被清理。
	runningID, _ := registry.start("长期任务", context.Background())
	// completeFirst 是第一条已完成任务的终态回调。
	_, completeFirst := registry.start("已完成任务一", context.Background())
	completeFirst(nil)
	// completeSecond 是第二条已完成任务的终态回调。
	_, completeSecond := registry.start("已完成任务二", context.Background())
	completeSecond(nil)
	// snapshots 是清理后的有限任务历史。
	snapshots := registry.list()
	if len(snapshots) != 2 {
		t.Fatalf("任务历史未按容量清理: len=%d snapshots=%+v", len(snapshots), snapshots)
	}
	// snapshot 是当前检查的任务状态快照。
	for _, snapshot := range snapshots {
		if snapshot.ID == runningID {
			return
		}
	}
	t.Fatalf("运行中任务不应被已完成历史清理: snapshots=%+v", snapshots)
}

// TestTaskRegistryTracksCancellationAndTimeout 证明取消与超时不会被误报为成功。
func TestTaskRegistryTracksCancellationAndTimeout(t *testing.T) {
	// canceledContext、cancelCanceled 是主动取消任务的上下文及其取消函数。
	canceledContext, cancelCanceled := context.WithCancel(context.Background())
	// canceledRegistry 是主动取消场景使用的注册表。
	canceledRegistry := newTaskRegistry()
	// canceledComplete 是主动取消任务的终态收束回调。
	_, canceledComplete := canceledRegistry.start("取消任务", canceledContext)
	cancelCanceled()
	canceledComplete(nil)
	// snapshots 是主动取消任务的状态快照列表。
	if snapshots := canceledRegistry.list(); len(snapshots) != 1 || snapshots[0].State != taskStateCanceled {
		t.Fatalf("任务取消后的状态不正确: %+v", snapshots)
	}

	// timeoutContext、cancelTimeout 是超时任务的上下文及其释放函数。
	timeoutContext, cancelTimeout := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancelTimeout()
	// timeoutRegistry 是超时场景使用的注册表。
	timeoutRegistry := newTaskRegistry()
	// timeoutComplete 是超时任务的终态收束回调。
	_, timeoutComplete := timeoutRegistry.start("超时任务", timeoutContext)
	select {
	case <-timeoutContext.Done():
	case <-time.After(time.Second):
		t.Fatal("超时上下文未按预期结束")
	}
	// pendingSnapshots 是任务函数尚未退出时的即时状态，用于验证超时可观测性。
	pendingSnapshots := timeoutRegistry.list()
	if len(pendingSnapshots) != 1 || pendingSnapshots[0].State != taskStateTimedOut {
		t.Fatalf("任务超时未被即时观测: %+v", pendingSnapshots)
	}
	timeoutComplete(nil)
	// finalSnapshots 是超时任务退出后的最终状态快照列表。
	if finalSnapshots := timeoutRegistry.list(); len(finalSnapshots) != 1 || finalSnapshots[0].State != taskStateTimedOut {
		t.Fatalf("任务退出后的超时状态不正确: %+v", finalSnapshots)
	}
}

// TestListAdminTasksReturnsNamedStatusDTO 证明管理端任务查询只返回具名非敏感状态字段。
func TestListAdminTasksReturnsNamedStatusDTO(t *testing.T) {
	// srv 是只装配任务注册表的最小 HTTP 服务实例。
	srv := &Server{taskRegistry: newTaskRegistry()}
	// _, complete 是已登记任务的标识和完成回调。
	_, complete := srv.taskRegistry.start("HTTP 测试任务", context.Background())
	complete(nil)
	// request、recorder 是管理端查询请求及其响应捕获器。
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/tasks?limit=1", nil)
	// recorder 捕获管理端任务查询响应。
	recorder := httptest.NewRecorder()
	srv.listAdminTasks(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("任务查询状态码=%d, body=%s", recorder.Code, recorder.Body.String())
	}
	// response 是管理端具名状态 DTO 列表。
	var response []taskStatusResponse
	// decodeErr 是管理端状态 DTO 的 JSON 解析错误。
	if decodeErr := json.Unmarshal(recorder.Body.Bytes(), &response); decodeErr != nil {
		t.Fatalf("任务状态响应解析失败: %v", decodeErr)
	}
	if len(response) != 1 || response[0].Name != "HTTP 测试任务" || response[0].State != string(taskStateSucceeded) {
		t.Fatalf("任务状态响应不正确: %+v", response)
	}
}
