package orders

import (
	"context"
	"errors"
	"testing"
)

// cancelOnLockRepository 在凭证锁被获取时取消请求 Context，模拟等待账号凭证锁期间调用方放弃请求。
type cancelOnLockRepository struct {
	// refreshRepositoryFake 提供订单刷新仓储基础行为。
	*refreshRepositoryFake
	// cancel 取消当前请求 Context。
	cancel context.CancelFunc
}

// LockCredentials 获取测试凭证锁后立刻取消请求 Context。
func (r *cancelOnLockRepository) LockCredentials(string) func() {
	r.cancel()
	return func() {}
}

// TestRefreshRuntimeAccountGuardsAndOwnerFailure 验证运行账号同步的未初始化守卫与归属查询失败透传。
func TestRefreshRuntimeAccountGuardsAndOwnerFailure(t *testing.T) {
	// nilService 是未初始化的订单刷新服务。
	var nilService *RefreshService
	if _, err := nilService.RefreshRuntimeAccount(context.Background(), "cookie-1"); err == nil {
		t.Fatal("nil 服务应拒绝")
	}
	// boom 是归属查询失败。
	boom := errors.New("owner lookup failed")
	// service 的仓储归属查询会失败。
	service := NewRefreshService(&refreshRepositoryFake{ownerIDErr: boom}, &refreshRuntimeFake{}, 100)
	if _, err := service.RefreshRuntimeAccount(context.Background(), "cookie-1"); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
}

// TestDiscoverAccountStopsWhenContextCanceledAfterLock 验证等待凭证锁期间请求被取消时不会访问平台，并返回取消原因。
func TestDiscoverAccountStopsWhenContextCanceledAfterLock(t *testing.T) {
	// ctx、cancel 是可取消的请求 Context。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// repository 在加锁时取消 Context。
	repository := &cancelOnLockRepository{refreshRepositoryFake: &refreshRepositoryFake{owned: map[string]bool{"cookie-1": true}, detail: &PlatformRuntimeData{ID: "cookie-1", UserID: 7, Value: "cookie"}}, cancel: cancel}
	// runtime 不应被调用，开启列表接口以暴露越权访问。
	runtime := &refreshRuntimeFake{soldAvailable: true}
	// service 是待测订单刷新服务。
	service := NewRefreshService(repository, runtime, 100)
	if _, _, _, err := service.discoverAccount(ctx, 7, "cookie-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

// TestRefreshCountsPerOrderFailuresOnDiscoveryError 验证订单归属冲突被隔离时，逐单失败数量计入汇总而不是仅计一次账号级错误。
func TestRefreshCountsPerOrderFailuresOnDiscoveryError(t *testing.T) {
	// repository 中订单 o1 归属其他账号且未软删除，无法自动迁移；o2 为普通新单。
	repository := &soldRecoveryRepository{
		refreshRepositoryFake: refreshRepositoryFake{
			owned: map[string]bool{"cookie-1": true}, orders: map[string]*Order{},
			detail: &PlatformRuntimeData{ID: "cookie-1", UserID: 7, Value: "cookie"},
		},
		ownership: map[string]RefreshOwnership{"o1": {OrderID: "o1", CookieID: "other", Owned: true}},
	}
	// runtime 返回两个列表订单。
	runtime := &refreshRuntimeFake{soldAvailable: true, soldResult: RefreshSoldFetchResult{Orders: []RefreshSoldOrder{{OrderID: "o1", OrderStatus: "paid"}, {OrderID: "o2", OrderStatus: "paid"}}}}
	// service 是待测订单刷新服务。
	service := NewRefreshService(repository, runtime, 100)
	// result、err 是刷新结果。
	result, err := service.Refresh(context.Background(), 7, "cookie-1", "")
	if err != nil || !result.PartialFailure || result.Summary.Failed < 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
