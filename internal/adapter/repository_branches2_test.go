package adapter

import (
	"context"
	"errors"
	"testing"

	automationapp "xianyu-go/internal/application/automation"
	chatapp "xianyu-go/internal/application/chat"
	"xianyu-go/internal/db"
)

// TestChatRepositoryFindListAndHideSession 覆盖按归属精确读取会话、键集分页（含游标）与隐藏清空会话。
func TestChatRepositoryFindListAndHideSession(t *testing.T) {
	// store、cleanup 是夹具数据库及清理函数。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	ctx := context.Background()
	// owner 是夹具账号 cid 的所有者。
	owner, ownerErr := store.Users.GetByUsername(ctx, "admin")
	if ownerErr != nil {
		t.Fatal(ownerErr)
	}
	// chatID 是依次写入的会话标识；时间递减以固定分页顺序。
	for index, chatID := range []string{"chat-a", "chat-b", "chat-c"} {
		// saveErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
		if saveErr := store.Chats.UpsertSession(ctx, db.ChatSession{CookieID: "cid", ChatID: chatID, BuyerID: "buyer-" + chatID, BuyerName: "买家", LastMessage: "hi", LastMessageAt: int64(30 - index)}); saveErr != nil {
			t.Fatal(saveErr)
		}
	}
	// repository 是聊天会话端口的数据库实现。
	repository := NewChatRepository(store).(chatapp.SessionRepository)
	// finder 是会话读取与分页端口。
	finder := repository.(interface {
		FindSession(context.Context, int64, string, string) (chatapp.Session, error)
		ListSessionPage(context.Context, int64, string, *chatapp.SessionCursor, int) (chatapp.SessionPage, error)
		HideAndClearSession(context.Context, int64, string, string, int64) (bool, error)
	})
	// found、findErr 是存在会话的精确读取结果。
	found, findErr := finder.FindSession(ctx, owner.ID, "cid", "chat-a")
	if findErr != nil || found.ChatID != "chat-a" || found.PeerUserID != "buyer-chat-a" {
		t.Fatalf("found=%+v err=%v", found, findErr)
	}
	// missingErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if _, missingErr := finder.FindSession(ctx, owner.ID, "cid", "nope"); !errors.Is(missingErr, chatapp.ErrChatSessionNotFound) {
		t.Fatalf("missing err=%v", missingErr)
	}
	// first、firstErr 是第一页（每页 2 条）。
	first, firstErr := finder.ListSessionPage(ctx, owner.ID, "cid", nil, 2)
	if firstErr != nil || len(first.Sessions) != 2 || !first.HasMore || first.NextCursor == nil {
		t.Fatalf("first=%+v err=%v", first, firstErr)
	}
	// second、secondErr 是使用游标读取的末页。
	second, secondErr := finder.ListSessionPage(ctx, owner.ID, "cid", first.NextCursor, 2)
	if secondErr != nil || len(second.Sessions) != 1 || second.HasMore || second.NextCursor != nil {
		t.Fatalf("second=%+v err=%v", second, secondErr)
	}
	// hidden、hideErr 是隐藏已有会话与重复隐藏的结果。
	hidden, hideErr := finder.HideAndClearSession(ctx, owner.ID, "cid", "chat-a", 100)
	if hideErr != nil || !hidden {
		t.Fatalf("hidden=%v err=%v", hidden, hideErr)
	}
	// 关闭数据库后分页必须透传底层错误。
	_ = store.DB.Close()
	// pageErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if _, pageErr := finder.ListSessionPage(ctx, owner.ID, "cid", nil, 2); pageErr == nil {
		t.Fatal("closed db should fail")
	}
}

// TestAutomationRepositoryGetUpdateAndItemProfile 覆盖单规则读取的归属隔离、AI 议价冲突保护、规则更新及商品规格摘要。
func TestAutomationRepositoryGetUpdateAndItemProfile(t *testing.T) {
	// store、cleanup 是夹具数据库及清理函数。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// repository 是自动化规则适配器。
	repository := NewAutomationRepository(store)
	ctx := context.Background()
	// owner 是夹具账号所有者。
	owner, ownerErr := store.Users.GetByUsername(ctx, "admin")
	if ownerErr != nil {
		t.Fatal(ownerErr)
	}
	// itemErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if itemErr := store.Items.Upsert(ctx, &db.ItemInfoRow{CookieID: "cid", ItemID: "item-1", ItemTitle: "商品"}); itemErr != nil {
		t.Fatal(itemErr)
	}
	// input 是普通的付款后确认发货规则。
	input := automationapp.RuleInput{UserID: owner.ID, CookieID: "cid", ItemID: "item-1", Name: "规则", TriggerType: automationapp.TriggerOrderPaid, Enabled: true, Priority: 100, ConfigJSON: "{}", Actions: []automationapp.ActionInput{{ActionType: automationapp.ActionConfirmShipment, Enabled: true, SortOrder: 1}}}
	// ensureErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if ensureErr := repository.EnsurePublishRule(ctx, input); ensureErr != nil {
		t.Fatal(ensureErr)
	}
	// rules、listErr 用于取得刚创建规则的标识。
	rules, listErr := repository.ListForUser(ctx, owner.ID)
	if listErr != nil || len(rules) != 1 {
		t.Fatalf("rules=%+v err=%v", rules, listErr)
	}
	// ruleID 是被测规则标识。
	ruleID := rules[0].ID
	// rule、getErr 是归属正确时的单规则读取。
	rule, getErr := repository.GetForUser(ctx, owner.ID, ruleID)
	if getErr != nil || rule.ID != ruleID {
		t.Fatalf("rule=%+v err=%v", rule, getErr)
	}
	// foreignErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if _, foreignErr := repository.GetForUser(ctx, owner.ID+1, ruleID); !errors.Is(foreignErr, automationapp.ErrRuleNotFound) {
		t.Fatalf("foreign err=%v", foreignErr)
	}
	// missingErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if _, missingErr := repository.GetForUser(ctx, owner.ID, ruleID+999); !errors.Is(missingErr, automationapp.ErrRuleNotFound) {
		t.Fatalf("missing err=%v", missingErr)
	}
	input.Name = "改名"
	// updateErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if updateErr := repository.Update(ctx, owner.ID, ruleID, input); updateErr != nil {
		t.Fatalf("update: %v", updateErr)
	}
	// profile、profileErr 是商品规格摘要。
	profile, profileErr := repository.GetItemDeliveryProfile(ctx, owner.ID, "cid", "item-1")
	if profileErr != nil || profile.IsMultiSpec {
		t.Fatalf("profile=%+v err=%v", profile, profileErr)
	}
	// missingItemErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if _, missingItemErr := repository.GetItemDeliveryProfile(ctx, owner.ID, "cid", "nope"); !errors.Is(missingItemErr, automationapp.ErrRuleNotFound) {
		t.Fatalf("missing item err=%v", missingItemErr)
	}
	// foreignOwnerErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if _, foreignOwnerErr := repository.GetItemDeliveryProfile(ctx, owner.ID+1, "cid", "item-1"); !errors.Is(foreignOwnerErr, automationapp.ErrRuleNotFound) {
		t.Fatalf("foreign owner err=%v", foreignOwnerErr)
	}
	// uninitializedErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if _, uninitializedErr := (&AutomationRepository{}).GetItemDeliveryProfile(ctx, 1, "cid", "item-1"); uninitializedErr == nil {
		t.Fatal("uninitialized should fail")
	}
}
