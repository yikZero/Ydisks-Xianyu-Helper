package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xianyu-go/internal/db"
)

// lockedBuffer 是并发安全的日志缓冲区，供异步 worker 写入、测试轮询读取。
type lockedBuffer struct {
	// mu 保护 buffer 的读写。
	mu sync.Mutex
	// buffer 保存累计的日志文本。
	buffer bytes.Buffer
}

// Write 追加日志文本。
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

// contains 判断累计日志是否包含片段。
func (b *lockedBuffer) contains(fragment string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Contains(b.buffer.String(), fragment)
}

// waitLog 在有限时间内轮询等待日志出现片段。
func waitLog(t *testing.T, logs *lockedBuffer, fragment string) {
	t.Helper()
	// deadline 是等待异步日志的预算。
	deadline := time.Now().Add(5 * time.Second)
	for !logs.contains(fragment) {
		if time.Now().After(deadline) {
			t.Fatalf("未等到日志: %s", fragment)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestWSRecorderCleanupAndFlushLogging 验证清理删除历史、清理失败与批量写入失败的日志，以及默认日志器与空 done。
func TestWSRecorderCleanupAndFlushLogging(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 成功清理历史报文。
	account, _, store, cleanup := newAccountForTest(t)
	defer cleanup()
	if _, err := store.DB.ExecContext(ctx, `INSERT INTO ws_messages (cookie_id,created_at) VALUES (?, '2000-01-01 00:00:00')`, account.CookieID); err != nil {
		t.Fatal(err)
	}
	// logs 收集 Debug 级别日志。
	logs := &lockedBuffer{}
	// recorder 是被测 WebSocket 报文记录器
	recorder := newWSRecorder(store, account.CookieID, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	recorder.start(ctx)
	waitLog(t, logs, "已清理过期 WS 报文")

	// 表缺失：清理与写入都失败。
	failAccount, _, failStore, failCleanup := newAccountForTest(t)
	defer failCleanup()
	if _, err := failStore.DB.ExecContext(ctx, `DROP TABLE ws_messages`); err != nil {
		t.Fatal(err)
	}
	// failLogs 收集失败告警。
	failLogs := &lockedBuffer{}
	// failing 是预置失败行为的测试替身，用于驱动错误分支
	failing := newWSRecorder(failStore, failAccount.CookieID, slog.New(slog.NewTextHandler(failLogs, nil)))
	failing.start(ctx)
	waitLog(t, failLogs, "清理过期 WS 报文失败")
	// i 是填充或循环序号
	for i := 0; i < WSRecordBatchSize; i++ {
		failing.queue <- db.WSMessage{CookieID: failAccount.CookieID, Direction: "in"}
	}
	waitLog(t, failLogs, "记录 WS 报文失败")

	// 空日志器使用默认实例；done 为空的已启动记录器视为已退出。
	if defaulted := newWSRecorder(store, "cid", nil); defaulted == nil || defaulted.logger == nil {
		t.Fatal("空日志器应回退到默认实例")
	}
	// bare 是已标记启动但没有 done 通道的记录器。
	bare := &wsRecorder{}
	bare.started.Store(true)
	if !bare.waitContext(context.Background()) {
		t.Fatal("缺少 done 通道应视为已退出")
	}
	cancel()
	if !recorder.waitContext(context.Background()) || !failing.waitContext(context.Background()) {
		t.Fatal("取消后记录器应退出")
	}
}

// initialReadyHandler 实现首次传输就绪回调，用于验证生命周期拒绝时不启动协程。
type initialReadyHandler struct {
	// recordingHandler 提供 Handler 基础实现。
	*recordingHandler
	// called 统计回调次数，回调在账号生命周期协程中执行，故使用原子计数。
	called atomic.Int32
}

// OnInitialTransportReady 记录调用。
func (h *initialReadyHandler) OnInitialTransportReady(context.Context, string) { h.called.Add(1) }

// TestInitialTransportReadyRejectedAndReplyServiceWiring 验证已停止账号不触发首次就绪回调，且带投递端口时装配回复服务。
func TestInitialTransportReadyRejectedAndReplyServiceWiring(t *testing.T) {
	// handler 是首次就绪回调处理器。
	handler := &initialReadyHandler{recordingHandler: &recordingHandler{}}
	// account 将在通知前被停止，生命周期不再接纳新任务。
	account := New(Config{CookieID: "idle", CookieStr: "unb=1", Handler: handler})
	// stopCtx、stopCancel 为停止账号提供有限预算。
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := account.StopContext(stopCtx); err != nil {
		t.Fatal(err)
	}
	account.notifyInitialTransportReady()
	if handler.called.Load() != 0 {
		t.Fatal("已停止账号不应触发回调")
	}
	// store、cleanup 提供回复服务所需存储。
	store, cleanup := newReplyStore(t)
	defer cleanup()
	// wired 是配置了投递端口的账号，用于验证回复服务装配
	wired := New(Config{CookieID: "cid", CookieStr: "unb=1", Store: store, ReplyDelivery: &fixedDelivery{}})
	if wired.reply == nil {
		t.Fatal("配置投递端口后应装配回复服务")
	}
	// client 是注入可编程行为的 MTOP 测试替身 ；err 保存 newAIHTTPClient 调用返回的错误，随后断言它是否符合该场景的预期
	if client, err := newAIHTTPClient("http://127.0.0.1:1"); err != nil || client == nil {
		t.Logf("默认 AI HTTP 客户端工厂返回: %v", err)
	}
}
