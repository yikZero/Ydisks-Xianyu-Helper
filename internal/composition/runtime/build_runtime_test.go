package runtime

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"xianyu-go/internal/db"
)

// TestBuildRuntimeWiresCompleteRuntimeAndLifecycle 使用真实临时 SQLite 验证组合根能装配完整运行时，并完成启动与逆序关闭。
func TestBuildRuntimeWiresCompleteRuntimeAndLifecycle(t *testing.T) {
	ctx := context.Background()
	// sqlDB、dialect、openErr 分别是临时库连接、方言及打开错误。
	sqlDB, dialect, openErr := db.Open(ctx, filepath.Join(t.TempDir(), "runtime.db"))
	if openErr != nil {
		t.Fatal(openErr)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	// built、buildErr 是禁用浏览器后的完整运行时及其构造错误。
	built, buildErr := BuildRuntime(
		RuntimeOptions{NoBrowser: true, SecureCookie: false, WebDir: t.TempDir(), Addr: "127.0.0.1:0"},
		RuntimeInfrastructure{Store: db.NewStore(sqlDB, dialect), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))},
	)
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	if built.HTTPServer == nil || built.Lifecycle == nil {
		t.Fatalf("runtime incomplete: %+v", built)
	}
	// startCtx 限制启动与关闭的最长等待，防止后台组件异常时测试挂起。
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// startErr 保存生命周期启动错误，非空表示后台组件启动失败。
	if startErr := built.Lifecycle.Start(startCtx); startErr != nil {
		t.Fatal(startErr)
	}
	if closeErr := built.Lifecycle.Close(startCtx); closeErr != nil {
		t.Fatal(closeErr)
	}
}
