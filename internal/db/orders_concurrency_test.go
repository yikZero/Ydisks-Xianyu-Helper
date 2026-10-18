package db

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestOrderUpsertRetryDelay 验证订单乐观锁冲突的退避曲线有界，避免高并发下立即重试形成活锁。
func TestOrderUpsertRetryDelay(t *testing.T) {
	// cases 保存覆盖首次冲突、指数增长、负数输入和上限截断的退避时间测试数据。
	cases := []struct {
		// name 标识当前退避场景；attempt 表示已连续失败次数；want 表示期望等待时间。
		name    string
		attempt int
		want    time.Duration
	}{
		{name: "首次冲突", attempt: 0, want: time.Millisecond},
		{name: "第三次冲突", attempt: 2, want: 4 * time.Millisecond},
		{name: "负数按首次处理", attempt: -1, want: time.Millisecond},
		{name: "超过上限", attempt: 20, want: maxOrderUpsertRetryDelay},
	}
	// testCase 表示当前要验证的一组退避输入与期望输出。
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// got 保存实际计算得到的重试等待时间。
			got := orderUpsertRetryDelay(testCase.attempt)
			if got != testCase.want {
				t.Fatalf("attempt=%d delay=%s want %s", testCase.attempt, got, testCase.want)
			}
		})
	}
}

// TestWaitOrderUpsertRetryHonorsCancellation 验证请求取消后不会继续等待订单重试窗口。
func TestWaitOrderUpsertRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// err 保存取消后的重试等待结果。
	err := waitOrderUpsertRetry(ctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled retry error=%v want %v", err, context.Canceled)
	}
}

func TestOrderUpsertConcurrentStatusNeverRegresses(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := store.Users.Create(ctx, "order-owner", "order-owner@example.com", "pw"); err != nil {
		t.Fatal(err)
	}
	owner, _ := store.Users.GetByUsername(ctx, "order-owner")
	if err := store.Cookies.CreateOwned(ctx, "order-account", "cookie", owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Orders.Upsert(ctx, "concurrent-order", OrderUpsertOpts{CookieID: "order-account", OrderStatus: "paid"}); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errCh := make(chan error, 200)
	var wg sync.WaitGroup
	for _, status := range []string{"paid", "shipped"} {
		status := status
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 100; i++ {
				errCh <- upsertOrderRetryingConflicts(ctx, store, "concurrent-order", OrderUpsertOpts{
					CookieID: "order-account", OrderStatus: status,
				})
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent upsert: %v", err)
		}
	}
	order, err := store.Orders.Get(ctx, "concurrent-order")
	if err != nil {
		t.Fatal(err)
	}
	if got := NormalizeOrderStatus(order.OrderStatus); got != "shipped" {
		t.Fatalf("final status=%q want shipped", got)
	}
	if order.Version <= 1 {
		t.Fatalf("version=%d was not advanced", order.Version)
	}

	if err := store.Orders.Upsert(ctx, "concurrent-order", OrderUpsertOpts{CookieID: "order-account", OrderStatus: "completed"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Orders.Upsert(ctx, "concurrent-order", OrderUpsertOpts{CookieID: "order-account", OrderStatus: "shipped"}); err != nil {
		t.Fatal(err)
	}
	order, _ = store.Orders.Get(ctx, "concurrent-order")
	if got := NormalizeOrderStatus(order.OrderStatus); got != "completed" {
		t.Fatalf("completed order regressed to %q", got)
	}
}

// maxTestOrderConflictRetries 是并发测试中调用方对 ErrOrderConflict 的最大重试轮数；
// 每轮内部已有 maxOrderUpsertRetries 次带退避的 CAS 尝试，上限只用于防止回归时测试无限循环。
const maxTestOrderConflictRetries = 50

// upsertOrderRetryingConflicts 按 ErrOrderConflict “请重试”的公开约定在调用方重试同一写入。
// ctx 控制数据库操作；store、orderID、opts 与 Orders.Upsert 相同。返回最后一次非冲突结果，
// 冲突轮数耗尽时返回 ErrOrderConflict；-race 等慢速环境下单轮 CAS 可能耗尽，但状态推进规则不得因此被绕过。
func upsertOrderRetryingConflicts(ctx context.Context, store *Store, orderID string, opts OrderUpsertOpts) error {
	// err 保存最近一轮写入结果，非冲突错误或成功时立即返回给测试断言。
	var err error
	// round 是调用方重试轮次，每轮都会重新读取订单快照后再做版本比较。
	for round := 0; round < maxTestOrderConflictRetries; round++ {
		err = store.Orders.Upsert(ctx, orderID, opts)
		if !errors.Is(err, ErrOrderConflict) {
			return err
		}
	}
	return err
}

// TestUpsertOrderRetryingConflictsReturnsNonConflictResult 验证测试辅助函数在无竞争时直接返回写入结果，且不吞掉非冲突错误。
func TestUpsertOrderRetryingConflictsReturnsNonConflictResult(t *testing.T) {
	// store、cleanup 是独立 SQLite 测试库及其释放函数。
	store, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	// err 是创建订单所属用户的结果。
	if _, err := store.Users.Create(ctx, "helper-owner", "helper-owner@example.com", "pw"); err != nil {
		t.Fatal(err)
	}
	// owner 是订单所属账号的用户，外键要求订单引用已存在的账号。
	owner, _ := store.Users.GetByUsername(ctx, "helper-owner")
	// err 是创建订单所属账号的结果。
	if err := store.Cookies.CreateOwned(ctx, "helper-account", "cookie", owner.ID); err != nil {
		t.Fatal(err)
	}
	// err 是无竞争时首次写入订单的结果，应直接成功。
	if err := upsertOrderRetryingConflicts(ctx, store, "helper-order", OrderUpsertOpts{CookieID: "helper-account", OrderStatus: "paid"}); err != nil {
		t.Fatalf("无竞争写入应成功: %v", err)
	}
	// err 是空订单号这一非冲突校验错误，辅助函数必须原样返回而不是重试。
	err := upsertOrderRetryingConflicts(ctx, store, "", OrderUpsertOpts{CookieID: "helper-account", OrderStatus: "paid"})
	if err == nil || errors.Is(err, ErrOrderConflict) {
		t.Fatalf("空订单号应返回非冲突错误，got %v", err)
	}
}
