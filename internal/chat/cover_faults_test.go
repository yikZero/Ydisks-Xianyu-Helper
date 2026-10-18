package chat

import (
	"context"
	"errors"
	"testing"

	"xianyu-go/internal/db"
)

// faultRepository 包装生产仓储并按需注入错误或空结果，用于覆盖聊天服务的仓储失败分支。
type faultRepository struct {
	Repository
	// itemExistsErr 是商品存在性查询应返回的错误。
	itemExistsErr error
	// roleErr 是会话角色写入应返回的错误。
	roleErr error
	// saveErr 是消息保存应返回的错误。
	saveErr error
	// saveNil 为真时消息保存返回空结果，模拟命中清空截止线。
	saveNil bool
	// upsertErr 是会话写入应返回的错误。
	upsertErr error
}

// ItemExists 返回预置错误，否则委托真实仓储。
func (r faultRepository) ItemExists(ctx context.Context, accountID, itemID string) (bool, error) {
	if r.itemExistsErr != nil {
		return false, r.itemExistsErr
	}
	return r.Repository.(sessionRoleRepository).ItemExists(ctx, accountID, itemID)
}

// SessionRole 委托真实仓储，满足 sessionRoleRepository。
func (r faultRepository) SessionRole(ctx context.Context, accountID, chatID, itemID string) (db.ChatSession, error) {
	return r.Repository.(sessionRoleRepository).SessionRole(ctx, accountID, chatID, itemID)
}

// UpdateSessionRole 返回预置错误，否则委托真实仓储。
func (r faultRepository) UpdateSessionRole(ctx context.Context, accountID, chatID, itemID, role, buyer, seller, source string) error {
	if r.roleErr != nil {
		return r.roleErr
	}
	return r.Repository.(sessionRoleRepository).UpdateSessionRole(ctx, accountID, chatID, itemID, role, buyer, seller, source)
}

// SaveMessage 返回预置错误或空结果，否则委托真实仓储。
func (r faultRepository) SaveMessage(ctx context.Context, session db.ChatSession, message db.ChatMessage, unread bool) (*db.ChatMessage, bool, error) {
	if r.saveErr != nil {
		return nil, false, r.saveErr
	}
	if r.saveNil {
		return nil, false, nil
	}
	return r.Repository.SaveMessage(ctx, session, message, unread)
}

// UpsertSession 返回预置错误，否则委托真实仓储。
func (r faultRepository) UpsertSession(ctx context.Context, session db.ChatSession) error {
	if r.upsertErr != nil {
		return r.upsertErr
	}
	return r.Repository.UpsertSession(ctx, session)
}

// TestRecordIncomingRepositoryFaults 覆盖实时消息保存失败、角色写入失败与角色会话缺失被吞掉的分支。
func TestRecordIncomingRepositoryFaults(t *testing.T) {
	// store 和 cleanup 提供隔离数据库。
	store, cleanup := chatTestStore(t)
	defer cleanup()
	// ctx 是本地仓储调用上下文。
	ctx := context.Background()
	if err := store.Items.Upsert(ctx, &db.ItemInfoRow{CookieID: "account-1", ItemID: "item-1", ItemTitle: "商品"}); err != nil {
		t.Fatal(err)
	}
	// base 是真实仓储适配器。
	base := newStoreRepository(store)
	// incoming 是满足本地商品角色判定的实时消息。
	incoming := Incoming{AccountID: "account-1", AccountUserID: "me", ChatID: "c1", BuyerID: "peer", ItemID: "item-1", MessageID: "m1", Text: "hi", ObservedAt: 1}
	// saveFailure 是保存阶段注入的错误。
	saveFailure := errors.New("save failed")
	if _, _, err := NewWithRepository(faultRepository{Repository: base, saveErr: saveFailure}).RecordIncoming(ctx, incoming); !errors.Is(err, saveFailure) {
		t.Fatalf("err=%v", err)
	}
	// roleFailure 是角色写入阶段注入的错误。
	roleFailure := errors.New("role failed")
	if _, _, err := NewWithRepository(faultRepository{Repository: base, roleErr: roleFailure}).RecordIncoming(ctx, incoming); !errors.Is(err, roleFailure) {
		t.Fatalf("err=%v", err)
	}
	// second 使用新消息键，角色写入返回 ErrNotFound 时应被视为会话已切换而忽略。
	second := incoming
	second.MessageID = "m2"
	if _, _, err := NewWithRepository(faultRepository{Repository: base, roleErr: db.ErrNotFound}).RecordIncoming(ctx, second); err != nil {
		t.Fatalf("err=%v", err)
	}
}

// TestRecordConversationPageRepositoryFaults 覆盖联系人页商品查询、会话写入与角色写入失败。
func TestRecordConversationPageRepositoryFaults(t *testing.T) {
	// store 和 cleanup 提供隔离数据库。
	store, cleanup := chatTestStore(t)
	defer cleanup()
	// ctx 是本地仓储调用上下文。
	ctx := context.Background()
	if err := store.Items.Upsert(ctx, &db.ItemInfoRow{CookieID: "account-1", ItemID: "item-1", ItemTitle: "商品"}); err != nil {
		t.Fatal(err)
	}
	// base 是真实仓储适配器。
	base := newStoreRepository(store)
	// body 是带商品扩展的联系人页。
	body := map[string]any{"userConvs": []any{map[string]any{"singleChatUserConversation": map[string]any{
		"singleChatConversation": map[string]any{"cid": "c1@goofish", "pairFirst": "me@goofish", "pairSecond": "peer@goofish", "extension": `{"itemId":"item-1"}`},
		"modifyTime":             float64(2000),
		"lastMessage":            map[string]any{"message": map[string]any{"createAt": float64(2000), "content": map[string]any{"custom": map[string]any{"summary": "摘要"}}}},
	}}}}
	// failure 是注入的仓储错误。
	failure := errors.New("boom")
	// cases 是各注入点及其名称。
	cases := map[string]faultRepository{
		"item":   {Repository: base, itemExistsErr: failure},
		"upsert": {Repository: base, upsertErr: failure},
		"role":   {Repository: base, roleErr: failure},
	}
	// name 和 repo 是当前注入点及其仓储。
	for name, repo := range cases {
		if _, err := NewWithRepository(repo).RecordConversationPage(ctx, "account-1", "me", body); !errors.Is(err, failure) {
			t.Fatalf("%s err=%v", name, err)
		}
	}
}

// TestRecordHistoryPageStopsAtClearedBoundary 验证历史消息命中清空截止线时停止回灌并重置分页。
func TestRecordHistoryPageStopsAtClearedBoundary(t *testing.T) {
	// store 和 cleanup 提供隔离数据库。
	store, cleanup := chatTestStore(t)
	defer cleanup()
	// body 是含一条文本消息且声明有更多页的历史页。
	body := map[string]any{"hasMore": true, "nextCursor": float64(5), "userMessageModels": []any{
		map[string]any{"message": map[string]any{"messageId": "m1", "createAt": float64(1000), "extension": map[string]any{"senderUserId": "peer@goofish"}, "content": map[string]any{"custom": map[string]any{"data": "eyJjb250ZW50VHlwZSI6MSwidGV4dCI6eyJ0ZXh0IjoiaGkifX0="}}}},
	}}
	// service 使用保存时返回空结果的仓储。
	service := NewWithRepository(faultRepository{Repository: newStoreRepository(store), saveNil: true})
	// page 和 err 是回灌结果。
	page, err := service.RecordHistoryPage(context.Background(), "account-1", "cid", "me", db.ChatSession{CookieID: "account-1", ChatID: "cid"}, body)
	if err != nil || page.HasMore || page.NextCursor != 0 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}

// TestOutgoingObservedAtAndDefaultType 覆盖出站记录使用调用方观察时间与空类型默认文本。
func TestOutgoingObservedAtAndDefaultType(t *testing.T) {
	// store 和 cleanup 提供隔离数据库。
	store, cleanup := chatTestStore(t)
	defer cleanup()
	// stored 和 err 是官方回显写入结果。
	stored, err := New(store).RecordOutgoingMessageSent(context.Background(), db.ChatSession{CookieID: "account-1", ChatID: "c1", BuyerID: "peer"}, "pnm-1", " ", " 你好 ", 4242)
	if err != nil || stored == nil || stored.MessageType != "text" || stored.Content != "你好" {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
}

// TestExtractStringAndOfficialTypeEdges 覆盖候选键去重、空键，以及官方类型首个命中即停止。
func TestExtractStringAndOfficialTypeEdges(t *testing.T) {
	if got := extractString(map[string]any{"a": "x"}, " ", "A", "a"); got != "x" {
		t.Fatalf("got=%q", got)
	}
	if got := findOfficialContentType([]any{map[string]any{"contentType": float64(14)}, map[string]any{"contentType": float64(25)}}); got != "14" {
		t.Fatalf("got=%q", got)
	}
}
