package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"

	"xianyu-go/internal/auth"
)

// failingListener 是 Close 总是返回自定义错误的监听器，用于覆盖绑定回滚时的关闭失败分支。
type failingListener struct{}

// Accept 不会被调用，仅满足 net.Listener 接口。
func (failingListener) Accept() (net.Conn, error) { return nil, errors.New("unused") }

// Close 返回非 ErrClosed 的错误，模拟底层关闭失败。
func (failingListener) Close() error { return errors.New("close failed") }

// Addr 返回固定占位地址。
func (failingListener) Addr() net.Addr { return &net.TCPAddr{} }

// stubDatabaseHealth 是仅用于 New 参数校验的健康检查桩。
type stubDatabaseHealth struct{}

// Ping 总是成功。
func (stubDatabaseHealth) Ping(context.Context) error { return nil }

// TestNewRejectsMissingDependencies 覆盖构造期缺失依赖的逐项拒绝。
func TestNewRejectsMissingDependencies(t *testing.T) {
	if _, err := New(Dependencies{}); err == nil {
		t.Error("missing auth should fail")
	}
	// authService 是非空但未接数据库的认证服务，仅用于通过第一项检查。
	authService := &auth.Service{}
	if _, err := New(Dependencies{Auth: authService}); err == nil {
		t.Error("missing health should fail")
	}
	if _, err := New(Dependencies{Auth: authService, DatabaseHealth: stubDatabaseHealth{}}); err == nil {
		t.Error("missing applications should fail")
	}
	if _, err := New(Dependencies{Auth: authService, DatabaseHealth: stubDatabaseHealth{}, Applications: &ApplicationPorts{}}); err == nil {
		t.Error("incomplete applications should fail")
	}
}

// TestRecoverExpiredSessionGuards 覆盖空 Server、空 applications 和空恢复器返回 false。
func TestRecoverExpiredSessionGuards(t *testing.T) {
	// nilServer 是空指针 Server。
	var nilServer *Server
	if nilServer.recoverExpiredSession(context.Background(), "c", errors.New("x")) {
		t.Error("nil server should not recover")
	}
	if (&Server{}).recoverExpiredSession(context.Background(), "c", nil) {
		t.Error("nil applications should not recover")
	}
	if (&Server{applications: &ApplicationPorts{}}).recoverExpiredSession(context.Background(), "c", nil) {
		t.Error("nil recovery should not recover")
	}
}

// TestRequestLoggerDefaultsStatusToOK 覆盖处理器未写响应时状态码回落为 200 的日志分支。
func TestRequestLoggerDefaultsStatusToOK(t *testing.T) {
	// srv 带默认日志器的最小 Server。
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	// handler 是什么都不写的下游处理器。
	handler := srv.requestLogger(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	// rec 记录响应。
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// TestMountSPAVariants 覆盖目录与嵌入式 SPA 的 no-store、API 404 与前端未构建分支。
func TestMountSPAVariants(t *testing.T) {
	// serve 对给定路由发起 GET 并返回响应记录。
	serve := func(router http.Handler, path string) *httptest.ResponseRecorder {
		// rec 记录响应。
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	// webDir 是仅含 index.html 的临时前端目录。
	webDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte("<html>ok</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	// dirRouter 挂载了目录 SPA。
	dirRouter := chi.NewRouter()
	(&Server{WebDir: webDir}).mountDirSPA(dirRouter)
	if rec := serve(dirRouter, "/static/index.html"); rec.Header().Get("Cache-Control") == "" {
		t.Error("static index should be no-store")
	}
	if rec := serve(dirRouter, "/cookies-api-missing"); rec.Code != http.StatusNotFound {
		t.Errorf("api path status = %d", rec.Code)
	}
	if rec := serve(dirRouter, "/dashboard"); rec.Code != http.StatusOK {
		t.Errorf("spa status = %d", rec.Code)
	}
	// emptyRouter 挂载了不含 index.html 的目录 SPA。
	emptyRouter := chi.NewRouter()
	(&Server{WebDir: t.TempDir()}).mountDirSPA(emptyRouter)
	if rec := serve(emptyRouter, "/dashboard"); rec.Code != http.StatusNotFound {
		t.Errorf("unbuilt status = %d", rec.Code)
	}

	// fsRouter 挂载了含 index.html 的内存文件系统 SPA。
	fsRouter := chi.NewRouter()
	(&Server{}).mountFSSPA(fsRouter, fstest.MapFS{"index.html": {Data: []byte("<html>fs</html>")}})
	if rec := serve(fsRouter, "/static/"); rec.Header().Get("Cache-Control") == "" {
		t.Error("static root should be no-store")
	}
	if rec := serve(fsRouter, "/static/index.html"); rec.Header().Get("Cache-Control") == "" {
		t.Error("static index should be no-store")
	}
	if rec := serve(fsRouter, "/orders-x"); rec.Code != http.StatusNotFound {
		t.Errorf("fs api path status = %d", rec.Code)
	}
	// noIndexRouter 挂载了缺少 index.html 的文件系统。
	noIndexRouter := chi.NewRouter()
	(&Server{}).mountFSSPA(noIndexRouter, fstest.MapFS{})
	if rec := serve(noIndexRouter, "/page"); rec.Code != http.StatusNotFound {
		t.Errorf("fs unbuilt status = %d", rec.Code)
	}
}

// TestServerLifecycleGuards 覆盖 nil 接收者、未启动、已停止与监听器异常的生命周期分支。
func TestServerLifecycleGuards(t *testing.T) {
	// nilServer 是空指针 Server，所有生命周期方法必须安全。
	var nilServer *Server
	if nilServer.Bind() == nil || nilServer.Start(context.Background()) == nil {
		t.Error("nil Bind/Start should fail")
	}
	if nilServer.Wait() != nil || nilServer.Stop(context.Background()) != nil {
		t.Error("nil Wait/Stop should be no-op")
	}
	nilServer.WaitForBackground()
	nilServer.beginBackgroundTask()
	nilServer.finishBackgroundTask()
	nilServer.beginWorker()()
	if !nilServer.waitForBackgroundContext(nilContext) {
		t.Error("nil server has nothing to wait for")
	}
	if nilServer.taskRegistryForServer() == nil {
		t.Error("nil server still gets a registry")
	}

	// fresh 是从未启动的 Server。
	fresh := &Server{}
	if fresh.Wait() == nil {
		t.Error("Wait before Start should fail")
	}
	if fresh.Stop(nilContext) != nil {
		t.Error("Stop before Start should be no-op")
	}

	// stoppedServer 已停止，不能重新绑定。
	stoppedServer := &Server{stopped: true}
	if stoppedServer.Bind() == nil {
		t.Error("Bind after Stop should fail")
	}
	// inconsistent 持有监听器但已停止，Start 必须拒绝。
	inconsistent := &Server{listener: failingListener{}, stopped: true}
	if inconsistent.Start(nilContext) == nil {
		t.Error("Start in stopped state should fail")
	}
	// alreadyStarted 持有监听器且已启动，Start 幂等返回 nil。
	alreadyStarted := &Server{listener: failingListener{}, started: true}
	if err := alreadyStarted.Start(context.Background()); err != nil {
		t.Errorf("Start twice: %v", err)
	}

	// canceled 是已取消的上下文。
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	// pendingDone 是永不关闭的 HTTP 完成信号。
	pendingDone := make(chan struct{})
	// stoppingServer 已进入停止流程但 HTTP 尚未退出。
	stoppingServer := &Server{started: true, stopped: true, httpDone: pendingDone}
	if !errors.Is(stoppingServer.Stop(canceled), context.Canceled) {
		t.Error("stop waiting for http should honor ctx")
	}
	// backgroundPending 是 HTTP 已退出但后台任务未完成的 Server。
	backgroundPending := &Server{started: true, stopped: true, backgroundDone: make(chan struct{})}
	if !errors.Is(backgroundPending.Stop(canceled), context.Canceled) {
		t.Error("stop waiting for background should honor ctx")
	}
	// closing 持有关闭会失败的监听器，Stop 应返回该错误。
	closing := &Server{listener: failingListener{}, httpDone: make(chan struct{})}
	if err := closing.Stop(context.Background()); err == nil || err.Error() != "close failed" {
		t.Errorf("listener close err = %v", err)
	}
	// shutdownTimeout 已启动但 HTTP goroutine 未退出，等待完成信号时被取消。
	shutdownTimeout := &Server{started: true, httpServer: &http.Server{}, httpDone: make(chan struct{})}
	if !errors.Is(shutdownTimeout.Stop(canceled), context.Canceled) {
		t.Error("stop should report ctx error when http never exits")
	}
	// backgroundAfterShutdown 的 HTTP 已完成但后台任务不退出。
	done := make(chan struct{})
	close(done)
	// backgroundAfterShutdown 表示 HTTP 已正常退出但后台任务始终未结束的 Server，用于覆盖等待后台任务被取消的分支
	backgroundAfterShutdown := &Server{started: true, httpServer: &http.Server{}, httpDone: done, backgroundDone: make(chan struct{})}
	if !errors.Is(backgroundAfterShutdown.Stop(canceled), context.Canceled) {
		t.Error("stop should report ctx error when background never exits")
	}
}

// TestServerBindStartStopRealListener 覆盖真实监听的 Bind/Start/Stop/Wait/Run 路径与错误传播。
func TestServerBindStartStopRealListener(t *testing.T) {
	// srv 是使用随机本地端口的测试 Server。
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	srv.Addr = "127.0.0.1:0"
	srv.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := srv.Bind(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Bind(); err != nil {
		t.Fatalf("second Bind should be idempotent: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(nilContext); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if err := srv.Wait(); err != nil {
		t.Fatal(err)
	}

	// rollback 只 Bind 不 Start，Stop 必须关闭监听器完成回滚。
	rollback, _, rollbackCleanup := newTestServer(t)
	defer rollbackCleanup()
	rollback.Addr = "127.0.0.1:0"
	if err := rollback.Bind(); err != nil {
		t.Fatal(err)
	}
	if err := rollback.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	// occupied 占用一个端口，使第二个 Server 的 Bind 失败。
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	// conflict 试图绑定已被占用的地址。
	conflict, _, conflictCleanup := newTestServer(t)
	defer conflictCleanup()
	conflict.Addr = occupied.Addr().String()
	if conflict.Bind() == nil || conflict.Start(context.Background()) == nil || conflict.Run(context.Background()) == nil {
		t.Error("bind conflicts should fail")
	}
}

// TestBackgroundTaskBookkeeping 覆盖空任务、计数归零保护与后台完成信号惰性创建。
func TestBackgroundTaskBookkeeping(t *testing.T) {
	// srv 是零值 Server，所有后台状态均惰性创建。
	srv := &Server{}
	// finish 在计数为 0 时调用必须直接返回。
	srv.finishBackgroundTask()
	if !srv.waitForBackgroundContext(nilContext) {
		t.Error("idle wait should succeed")
	}
	// taskID 是空任务的登记 ID。
	taskID := srv.startBackgroundTaskResult("empty", context.Background(), nil)
	if taskID == "" {
		t.Error("task id empty")
	}
	srv.WaitForBackground()
	// logged 带日志器的 Server 启动空任务。
	logged, _, cleanup := newTestServer(t)
	defer cleanup()
	logged.startBackgroundTaskContext("nil", context.Background(), nil)
	logged.WaitForBackground()
}

// nilContext 是刻意保持为 nil 的 Context，用于验证被测函数对空上下文的容错。
var nilContext context.Context
