package db

import (
	"context"
	"testing"
)

// TestShouldUpdateOrderStatusPlatformSnapshotRules 验证“退款中→待发货”和“已取消→退款中/已完成”只接受平台快照，其余倒退对两类来源一致拦截。
func TestShouldUpdateOrderStatusPlatformSnapshotRules(t *testing.T) {
	// cases 覆盖平台快照专属放行、事件来源拦截以及两类来源都必须拦截的阶段倒退。
	cases := []struct {
		// current、incoming 是本地状态与待写入状态；snapshot 表示来源是否为平台快照；want 是期望结果。
		current, incoming string
		snapshot, want    bool
	}{
		{current: "refunding", incoming: "pending_ship", snapshot: true, want: true},
		{current: "refunding", incoming: "pending_ship", snapshot: false, want: false},
		{current: "refunding", incoming: "processing", snapshot: true, want: false},
		{current: "refunding", incoming: "shipped", snapshot: false, want: true},
		{current: "cancelled", incoming: "refunding", snapshot: false, want: false},
		{current: "cancelled", incoming: "completed", snapshot: false, want: false},
		{current: "cancelled", incoming: "refunding", snapshot: true, want: true},
		{current: "cancelled", incoming: "completed", snapshot: true, want: true},
		{current: "cancelled", incoming: "pending_ship", snapshot: true, want: false},
		{current: "cancelled", incoming: "shipped", snapshot: true, want: false},
		{current: "shipped", incoming: "pending_ship", snapshot: true, want: false},
		{current: "completed", incoming: "shipped", snapshot: true, want: false},
		{current: "completed", incoming: "refunding", snapshot: false, want: true},
		{current: "pending_ship", incoming: "refunding", snapshot: false, want: true},
	}
	// testCase 是当前验证的一组状态迁移。
	for _, testCase := range cases {
		if got := /* got 是当前迁移的实际判断结果。 */ shouldUpdateOrderStatus(testCase.current, testCase.incoming, testCase.snapshot); got != testCase.want {
			t.Errorf("%s -> %s snapshot=%v: got %v want %v", testCase.current, testCase.incoming, testCase.snapshot, got, testCase.want)
		}
	}
}

// TestOrderUpsertRefundWithdrawnRestoresPendingShipOnlyFromSnapshot 用真实 SQLite 验证买家撤回退款后，
// 只有平台快照能把订单恢复为待发货，迟到事件不能；已取消订单也不会被迟到的退款中事件改写。
func TestOrderUpsertRefundWithdrawnRestoresPendingShipOnlyFromSnapshot(t *testing.T) {
	// store、cleanup 是独立 SQLite 测试库及其释放函数。
	store, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	// err 是创建订单所属用户的结果。
	if _, err := store.Users.Create(ctx, "refund-owner", "refund-owner@example.com", "pw"); err != nil {
		t.Fatal(err)
	}
	// owner 是订单所属账号的用户，外键要求订单引用已存在的账号。
	owner, _ := store.Users.GetByUsername(ctx, "refund-owner")
	// err 是创建订单所属账号的结果。
	if err := store.Cookies.CreateOwned(ctx, "refund-account", "cookie", owner.ID); err != nil {
		t.Fatal(err)
	}
	// write 按指定来源写入订单状态，失败立即终止测试。
	write := func(orderID, status string, snapshot bool) {
		// err 是本次订单写入结果。
		if err := store.Orders.Upsert(ctx, orderID, OrderUpsertOpts{CookieID: "refund-account", OrderStatus: status, PlatformSnapshot: snapshot}); err != nil {
			t.Fatal(err)
		}
	}
	// statusOf 读取订单当前规范化状态。
	statusOf := func(orderID string) string {
		// order、err 是当前订单读取结果。
		order, err := store.Orders.Get(ctx, orderID)
		if err != nil {
			t.Fatal(err)
		}
		return NormalizeOrderStatus(order.OrderStatus)
	}
	write("refund-order", "pending_ship", true)
	write("refund-order", "refunding", true)
	write("refund-order", "pending_ship", false)
	if got := /* got 是迟到付款事件写入后的状态。 */ statusOf("refund-order"); got != "refunding" {
		t.Fatalf("迟到事件不应把退款中改回待发货，got %q", got)
	}
	write("refund-order", "pending_ship", true)
	if got := /* got 是平台快照写入后的状态。 */ statusOf("refund-order"); got != "pending_ship" {
		t.Fatalf("平台快照应恢复为待发货，got %q", got)
	}
	write("closed-order", "cancelled", true)
	write("closed-order", "refunding", false)
	if got := /* got 是迟到退款事件写入后的状态。 */ statusOf("closed-order"); got != "cancelled" {
		t.Fatalf("迟到事件不应改写已取消订单，got %q", got)
	}
}
