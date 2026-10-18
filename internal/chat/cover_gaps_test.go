package chat

import (
	"context"
	"errors"
	"testing"

	"xianyu-go/internal/db"
)

// ownerErrorRepository 在动态归属查询阶段返回基础设施错误，用于验证订阅被主动关闭。
type ownerErrorRepository struct {
	fakeRepository
	// ownerErr 是归属查询应返回的基础设施错误。
	ownerErr error
	// ownerID 是归属查询成功时返回的管理用户标识。
	ownerID int64
}

// GetOwnerID 返回预置的归属结果，不访问任何真实存储。
func (r *ownerErrorRepository) GetOwnerID(context.Context, string) (int64, error) {
	return r.ownerID, r.ownerErr
}

// TestSessionRoleFallbackWithoutRoleRepository 验证仓储缺少角色能力时读取返回 unknown、写入返回明确错误。
func TestSessionRoleFallbackWithoutRoleRepository(t *testing.T) {
	// service 使用不支持会话角色扩展的内存仓储。
	service := NewWithRepository(&fakeRepository{})
	// session 和 err 是降级读取得到的 unknown 角色结论。
	session, err := service.SessionRole(context.Background(), "acc", "chat", "item")
	if err != nil || session.AccountRole != "unknown" || session.CookieID != "acc" || session.ChatID != "chat" || session.ItemID != "item" {
		t.Fatalf("session=%+v err=%v", session, err)
	}
	// updateErr 是缺少角色仓储时写入应返回的错误。
	updateErr := service.UpdateSessionRole(context.Background(), "acc", "chat", "item", "seller", "b", "s", "src")
	if updateErr == nil {
		t.Fatal("缺少角色仓储时必须返回错误")
	}
}

// TestSessionRoleDelegatesToStore 验证生产仓储下角色读写、不存在会话与商品存在性查询均正确委托。
func TestSessionRoleDelegatesToStore(t *testing.T) {
	// store 和 cleanup 提供隔离 SQLite 数据库及关闭责任。
	store, cleanup := chatTestStore(t)
	// ctx 是本测试全部本地仓储调用的上下文。
	ctx := context.Background()
	// service 使用生产仓储适配器。
	service := New(store)
	if err := store.Chats.UpsertSession(ctx, db.ChatSession{CookieID: "account-1", ChatID: "chat-1", ItemID: "item-1", BuyerID: "buyer"}); err != nil {
		t.Fatal(err)
	}
	if err := service.UpdateSessionRole(ctx, "account-1", "chat-1", "item-1", "seller", "buyer", "me", "local_item"); err != nil {
		t.Fatal(err)
	}
	// session 和 err 是已保存的角色结论。
	session, err := service.SessionRole(ctx, "account-1", "chat-1", "item-1")
	if err != nil || session.AccountRole != "seller" || session.SellerUserID != "me" {
		t.Fatalf("session=%+v err=%v", session, err)
	}
	// missing 和 missingErr 是商品不一致时的 unknown 结论。
	missing, missingErr := service.SessionRole(ctx, "account-1", "chat-1", "other-item")
	if missingErr != nil || missing.AccountRole != "unknown" {
		t.Fatalf("missing=%+v err=%v", missing, missingErr)
	}
	// repository 是直接使用的存储仓储，用于覆盖商品存在性分支。
	repository := storeRepository{store: store}
	// exists 和 existsErr 是不存在商品的查询结果。
	exists, existsErr := repository.ItemExists(ctx, "account-1", "no-such-item")
	if existsErr != nil || exists {
		t.Fatalf("exists=%v err=%v", exists, existsErr)
	}
	// itemErr 保存商品夹具写入的错误结果，非空时测试立即终止。
	if itemErr := store.Items.Upsert(ctx, &db.ItemInfoRow{CookieID: "account-1", ItemID: "item-1", ItemTitle: "商品"}); itemErr != nil {
		t.Fatal(itemErr)
	}
	exists, existsErr = repository.ItemExists(ctx, "account-1", "item-1")
	if existsErr != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, existsErr)
	}
	cleanup()
	// closedExists 和 closedErr 是数据库关闭后的基础设施错误结果。
	closedExists, closedErr := repository.ItemExists(ctx, "account-1", "item-1")
	if closedErr == nil || closedExists {
		t.Fatalf("closedExists=%v err=%v", closedExists, closedErr)
	}
}

// TestPublishClosesSubscriptionsWhenOwnerLookupFails 验证归属查询失败会关闭全部订阅，零归属则静默忽略。
func TestPublishClosesSubscriptionsWhenOwnerLookupFails(t *testing.T) {
	// repository 模拟归属查询基础设施失败。
	repository := &ownerErrorRepository{ownerErr: errors.New("db down")}
	// service 使用可注入归属结果的仓储。
	service := NewWithRepository(repository)
	// events 和 cancel 是管理订阅的事件队列及释放函数。
	events, cancel, err := service.Subscribe(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	service.Publish("acc", Event{Type: "x"})
	// event 和 open 是被关闭队列的读取结果。
	event, open := <-events
	if open {
		t.Fatalf("订阅应被关闭: %+v", event)
	}
	// 归属为零时不应投递也不应关闭。
	repository.ownerErr = nil
	repository.ownerID = 0
	// quiet 和 quietCancel 是第二个订阅。
	quiet, quietCancel, quietErr := service.Subscribe(context.Background(), 7)
	if quietErr != nil {
		t.Fatal(quietErr)
	}
	defer quietCancel()
	service.Publish("acc", Event{Type: "x"})
	select {
	case /* got 是意外收到的事件 */ got := <-quiet:
		t.Fatalf("归属为零不应投递: %+v", got)
	default:
	}
}

// TestPublishDropsSlowSubscriber 验证队列已满的订阅者会被移除并关闭。
func TestPublishDropsSlowSubscriber(t *testing.T) {
	// repository 返回归属于用户 9 的账号。
	repository := &ownerErrorRepository{ownerID: 9}
	// service 使用该归属仓储。
	service := NewWithRepository(repository)
	// events 和 cancel 是订阅队列及释放函数。
	events, cancel, err := service.Subscribe(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	// i 是填满 128 容量队列的事件序号。
	for i := 0; i < 129; i++ {
		service.Publish("acc", Event{Type: "x"})
	}
	// drained 统计关闭前取出的事件数量。
	drained := 0
	for range events {
		drained++
	}
	if drained != 128 {
		t.Fatalf("drained=%d", drained)
	}
}

// TestNormalizedItemCardContentRejectsIncomplete 覆盖商品卡片缺字段、协议相对图片地址与完整输出。
func TestNormalizedItemCardContentRejectsIncomplete(t *testing.T) {
	// full 是完整商品卡片载荷。
	full := map[string]any{"itemCard": map[string]any{"item": map[string]any{"itemId": "1", "title": "t", "mainPic": "//img/x.png", "price": "¥9.9"}}}
	// got 是归一化后的 JSON。
	got := normalizedItemCardContent(full)
	if got != `{"item_id":"1","title":"t","image_url":"https://img/x.png","price":"9.9"}` {
		t.Fatalf("got=%s", got)
	}
	if normalizedItemCardContent(map[string]any{}) != "" {
		t.Fatal("缺少 item 应返回空")
	}
	// key 是依次被移除的必需字段名。
	for _, key := range []string{"itemId", "title", "mainPic", "price"} {
		// item 是缺少当前必需字段的商品载荷。
		item := map[string]any{"itemId": "1", "title": "t", "mainPic": "m", "price": "1"}
		delete(item, key)
		if normalizedItemCardContent(map[string]any{"itemCard": map[string]any{"item": item}}) != "" {
			t.Fatalf("缺少 %s 应返回空", key)
		}
	}
}
