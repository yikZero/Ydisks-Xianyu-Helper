package reconciliation

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"xianyu-go/internal/db"
)

// countingHandler 是统计 Warn 日志条数并在达到阈值时取消 Context 的 slog 处理器，用于无需 sleep 地同步后台扫描轮次。
type countingHandler struct {
	// warns 记录已经输出的 Warn 日志条数。
	warns *atomic.Int32
	// threshold 是触发取消所需的 Warn 条数。
	threshold int32
	// cancel 取消后台扫描 Context。
	cancel context.CancelFunc
}

// Enabled 对所有级别返回 true。
func (h countingHandler) Enabled(context.Context, slog.Level) bool { return true }

// Handle 统计 Warn 日志，达到阈值后取消后台扫描。
func (h countingHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Level == slog.LevelWarn && h.warns.Add(1) >= h.threshold {
		h.cancel()
	}
	return nil
}

// WithAttrs 返回自身，测试不关心附加属性。
func (h countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

// WithGroup 返回自身，测试不关心分组。
func (h countingHandler) WithGroup(string) slog.Handler { return h }

// TestRunLogsTickerScanFailure 验证定时扫描轮次失败时记录告警并继续运行，直到 Context 取消后退出。
func TestRunLogsTickerScanFailure(t *testing.T) {
	// database、dialect、err 是迁移后随即关闭的数据库，使每轮扫描都失败。
	database, dialect, err := db.Open(context.Background(), t.TempDir()+"/ticker.db")
	if err != nil {
		t.Fatal(err)
	}
	// store 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	store := db.NewStore(database, dialect)
	database.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// warns 统计失败告警条数；第一条来自启动扫描，第二条来自定时轮次。
	var warns atomic.Int32
	// service 使用极短间隔和计数日志器。
	service := New(store, slog.New(countingHandler{warns: &warns, threshold: 2, cancel: cancel}))
	service.interval = time.Millisecond
	service.Run(ctx)
	if warns.Load() < 2 {
		t.Fatalf("warns=%d", warns.Load())
	}
}
