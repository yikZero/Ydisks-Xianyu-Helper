package adapter

import (
	"context"
	"errors"
	"testing"
	"time"

	"xianyu-go/internal/xianyu/mtop"
)

// blockingDetailFetcher 在收到调用后阻塞到测试放行，用于构造在途请求与限流等待场景。
type blockingDetailFetcher struct {
	// started 在首次调用进入后关闭，通知测试平台请求已在途。
	started chan struct{}
	// release 关闭后放行阻塞中的调用。
	release chan struct{}
}

// FetchOrderDetail 通知开始并阻塞到放行，然后返回固定详情。
func (f *blockingDetailFetcher) FetchOrderDetail(context.Context, string, string) (*mtop.OrderDetailResult, error) {
	close(f.started)
	<-f.release
	return &mtop.OrderDetailResult{Quantity: "1"}, nil
}

// TestOrderDetailCoordinatorGuards 覆盖协调器空接收者、空抓取器与空标识的保护。
func TestOrderDetailCoordinatorGuards(t *testing.T) {
	ctx := context.Background()
	// nilCoordinator 是空指针协调器。
	var nilCoordinator *OrderDetailCoordinator
	if _, err := nilCoordinator.Fetch(ctx, "a", "o", "c", &fakeOrderDetailClient{}); err == nil {
		t.Fatal("空协调器应失败")
	}
	// coordinator 是零间隔协调器。
	coordinator := newOrderDetailCoordinator(0, 0, nil)
	if _, err := coordinator.Fetch(ctx, "a", "o", "c", nil); err == nil {
		t.Fatal("空抓取器应失败")
	}
	if _, err := coordinator.Fetch(ctx, " ", "o", "c", &fakeOrderDetailClient{}); err == nil {
		t.Fatal("空账号应失败")
	}
	if _, err := coordinator.Fetch(ctx, "a", " ", "c", &fakeOrderDetailClient{}); err == nil {
		t.Fatal("空订单应失败")
	}
	if cloneOrderDetailResult(nil) != nil {
		t.Fatal("nil 克隆应为 nil")
	}
}

// TestOrderDetailCoordinatorWaiterCancelAndGapCancel 覆盖等待者在途请求时取消，以及限流等待期间取消。
func TestOrderDetailCoordinatorWaiterCancelAndGapCancel(t *testing.T) {
	// coordinator 使用零间隔，便于首个请求立即在途。
	coordinator := newOrderDetailCoordinator(0, 0, nil)
	// fetcher 阻塞首个请求。
	fetcher := &blockingDetailFetcher{started: make(chan struct{}), release: make(chan struct{})}
	// firstDone 接收首个调用的结果。
	firstDone := make(chan error, 1)
	go func() {
		_, err := coordinator.Fetch(context.Background(), "acct", "order", "c", fetcher)
		firstDone <- err
	}()
	<-fetcher.started
	// waitCtx 是等待者的可取消上下文，取消后应立即返回而不影响首个请求。
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := coordinator.Fetch(waitCtx, "acct", "order", "c", fetcher); !errors.Is(err, context.Canceled) {
		t.Fatalf("等待者取消: %v", err)
	}
	close(fetcher.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("首个请求: %v", err)
	}
	// slow 使用较长限流间隔：首个请求设定时隙后，第二个订单在等待期间取消。
	slow := newOrderDetailCoordinator(time.Hour, time.Hour, nil)
	if _, err := slow.Fetch(context.Background(), "acct", "o1", "c", &fakeOrderDetailClient{detail: &mtop.OrderDetailResult{}}); err != nil {
		t.Fatal(err)
	}
	// gapCtx 在等待限流前已取消。
	gapCtx, gapCancel := context.WithCancel(context.Background())
	gapCancel()
	if _, err := slow.Fetch(gapCtx, "acct", "o2", "c", &fakeOrderDetailClient{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("限流等待取消: %v", err)
	}
}
