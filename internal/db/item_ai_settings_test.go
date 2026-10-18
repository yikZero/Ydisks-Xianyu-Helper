package db

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestItemAISettingsLifecycle 验证商品级 AI 配置的默认继承、保存、更新、删除和账号级联清理。
func TestItemAISettingsLifecycle(t *testing.T) {
	// database、store 和 cleanup 是隔离的 SQLite 测试数据库及其资源。
	database, _, openErr := Open(context.Background(), filepath.Join(t.TempDir(), "item-ai.db"))
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer database.Close()
	// store 是商品 AI 配置持久层测试使用的仓储聚合。
	store := NewStore(database, DialectSQLite)
	ctx := context.Background()
	// createErr 是创建测试用户时的错误。
	if _, createErr := store.Users.Create(ctx, "item-ai-owner", "item-ai@example.com", "password"); createErr != nil {
		t.Fatal(createErr)
	}
	// owner 和 ownerErr 是测试账号所有者及其查询错误。
	owner, ownerErr := store.Users.GetByUsername(ctx, "item-ai-owner")
	if ownerErr != nil {
		t.Fatal(ownerErr)
	}
	// saveErr 是保存测试账号时的错误。
	if saveErr := store.Cookies.Save(ctx, "item-ai-account", "unb=1", owner.ID); saveErr != nil {
		t.Fatal(saveErr)
	}
	// initial 是没有独立记录时的默认继承配置。
	initial, initialErr := store.ItemAISettings.Get(ctx, "item-ai-account", "item-1")
	if initialErr != nil || initial.Override != ItemAIOverrideInherit || initial.Context != "" {
		t.Fatalf("默认商品 AI 配置异常: settings=%+v err=%v", initial, initialErr)
	}
	// upsertErr 是首次保存商品配置时的错误。
	if upsertErr := store.ItemAISettings.Upsert(ctx, ItemAISettingsRow{CookieID: "item-ai-account", ItemID: "item-1", Override: ItemAIOverrideEnabled, Context: "24 小时内发货"}); upsertErr != nil {
		t.Fatal(upsertErr)
	}
	// saved 是写入后读取到的完整配置。
	saved, savedErr := store.ItemAISettings.Get(ctx, "item-ai-account", "item-1")
	if savedErr != nil || saved.Override != ItemAIOverrideEnabled || saved.Context != "24 小时内发货" {
		t.Fatalf("保存商品 AI 配置异常: settings=%+v err=%v", saved, savedErr)
	}
	// invalidErr 是非法覆盖值写入错误。
	if invalidErr := store.ItemAISettings.Upsert(ctx, ItemAISettingsRow{CookieID: "item-ai-account", ItemID: "item-1", Override: "invalid"}); invalidErr == nil {
		t.Fatal("非法覆盖值应被拒绝")
	}
	// longErr 是超长商品资料写入错误。
	if longErr := store.ItemAISettings.Upsert(ctx, ItemAISettingsRow{CookieID: "item-ai-account", ItemID: "item-1", Override: ItemAIOverrideInherit, Context: strings.Repeat("中", ItemAIContextMaxRunes+1)}); longErr == nil {
		t.Fatal("超长商品资料应被拒绝")
	}
	// deleteErr 是重置独立商品配置时的错误。
	if deleteErr := store.ItemAISettings.Delete(ctx, "item-ai-account", "item-1"); deleteErr != nil {
		t.Fatal(deleteErr)
	}
	// reset 是删除独立记录后恢复的默认继承配置。
	reset, resetErr := store.ItemAISettings.Get(ctx, "item-ai-account", "item-1")
	if resetErr != nil || reset.Override != ItemAIOverrideInherit || reset.Context != "" {
		t.Fatalf("重置商品 AI 配置异常: settings=%+v err=%v", reset, resetErr)
	}
}

// TestItemAISettingsRejectsInvalidUTF8AndReportsDatabaseErrors 验证数据库层拒绝非法 UTF-8 资料，
// 并且数据库不可用时 Get、Upsert、Delete 都返回错误而不是伪装成默认继承或保存成功。
func TestItemAISettingsRejectsInvalidUTF8AndReportsDatabaseErrors(t *testing.T) {
	// database 是隔离的 SQLite 测试数据库，稍后主动关闭以模拟不可用。
	database, _, openErr := Open(context.Background(), filepath.Join(t.TempDir(), "item-ai-errors.db"))
	if openErr != nil {
		t.Fatal(openErr)
	}
	// store 是商品 AI 配置测试使用的仓储聚合。
	store := NewStore(database, DialectSQLite)
	ctx := context.Background()
	// invalidErr 是非法 UTF-8 资料写入错误，必须在触达数据库前被拒绝。
	if invalidErr := store.ItemAISettings.Upsert(ctx, ItemAISettingsRow{CookieID: "a", ItemID: "i", Override: ItemAIOverrideEnabled, Context: "坏\xff字节"}); invalidErr == nil {
		t.Fatal("非法 UTF-8 商品资料必须被拒绝")
	}
	if closeErr := database.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	// getErr 保存 Get 调用返回的错误，随后断言它是否符合该场景的预期。
	if _, getErr := store.ItemAISettings.Get(ctx, "a", "i"); getErr == nil {
		t.Fatal("数据库不可用时 Get 不得伪装成默认继承配置")
	}
	// upsertErr 保存 Upsert 调用返回的错误，随后断言它是否符合该场景的预期。
	if upsertErr := store.ItemAISettings.Upsert(ctx, ItemAISettingsRow{CookieID: "a", ItemID: "i", Override: ItemAIOverrideEnabled}); upsertErr == nil {
		t.Fatal("数据库不可用时 Upsert 必须返回错误")
	}
	// deleteErr 保存 Delete 调用返回的错误，随后断言它是否符合该场景的预期。
	if deleteErr := store.ItemAISettings.Delete(ctx, "a", "i"); deleteErr == nil {
		t.Fatal("数据库不可用时 Delete 必须返回错误")
	}
}

// TestItemQueriesExposeAIOverridePerAccount 验证商品查询把 ai_override 与商品关联返回：
// 无独立配置的商品为 inherit，有配置的商品为配置值；同一商品标识在不同账号下互不影响；
// 用户范围查询只返回所属账号的商品；数据库不可用时三个查询都返回错误。
func TestItemQueriesExposeAIOverridePerAccount(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()
	// ownerID、cookieID 是测试账号 A 及其所有者。
	ownerID, cookieID := seedAccount(t, store)
	ctx := context.Background()
	// otherCookie 是同一用户名下的另一个账号，用于验证同商品标识的隔离。
	const otherCookie = "item-ai-other-account"
	if err := store.Cookies.Save(ctx, otherCookie, "unb=2", ownerID); err != nil {
		t.Fatal(err)
	}
	// 账号 A 有两件商品，账号 B 有同一标识的商品。
	for _, row := range []ItemInfoRow{{CookieID: cookieID, ItemID: "same-item", ItemTitle: "A 的商品"}, {CookieID: cookieID, ItemID: "plain-item", ItemTitle: "A 的普通商品"}, {CookieID: otherCookie, ItemID: "same-item", ItemTitle: "B 的商品"}} {
		// row 是循环变量的独立副本，避免子用例闭包共享同一个迭代变量。
		row := row
		if err := store.Items.Upsert(ctx, &row); err != nil {
			t.Fatal(err)
		}
	}
	// 只给账号 A 的 same-item 配置强制停用。
	if err := store.ItemAISettings.Upsert(ctx, ItemAISettingsRow{CookieID: cookieID, ItemID: "same-item", Override: ItemAIOverrideDisabled}); err != nil {
		t.Fatal(err)
	}
	// overrides 把商品行按“账号/商品”映射到其 AIOverride，便于逐项断言。
	overrides := func(rows []ItemInfoRow) map[string]string {
		// result 保存 make 调用的返回结果，供随后的断言使用。
		result := make(map[string]string, len(rows))
		// row 是当前遍历的元素。
		for _, row := range rows {
			result[row.CookieID+"/"+row.ItemID] = row.AIOverride
		}
		return result
	}
	// accountRows、accountErr 是账号 A 的商品列表。
	accountRows, accountErr := store.Items.AllForCookie(ctx, cookieID)
	if accountErr != nil {
		t.Fatal(accountErr)
	}
	// got 保存 overrides 调用的返回结果，供随后的断言使用。
	if got := overrides(accountRows); len(got) != 2 || got[cookieID+"/same-item"] != ItemAIOverrideDisabled || got[cookieID+"/plain-item"] != ItemAIOverrideInherit {
		t.Fatalf("账号商品列表的 ai_override 不一致: %v", got)
	}
	// 账号 B 的同标识商品不受账号 A 配置影响。
	otherRows, otherErr := store.Items.AllForCookie(ctx, otherCookie)
	if otherErr != nil || len(otherRows) != 1 || otherRows[0].AIOverride != ItemAIOverrideInherit {
		t.Fatalf("其他账号同标识商品必须保持 inherit: %+v err=%v", otherRows, otherErr)
	}
	// 单件详情同样返回覆盖值。
	if detail, err := store.Items.GetByCookieItem(ctx, cookieID, "same-item"); err != nil || detail.AIOverride != ItemAIOverrideDisabled || detail.ItemTitle != "A 的商品" {
		t.Fatalf("商品详情 ai_override 不一致: %+v err=%v", detail, err)
	}
	if _, err := store.Items.GetByCookieItem(ctx, cookieID, "no-such-item"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的商品应返回 ErrNotFound，got %v", err)
	}
	// 用户范围查询：不带账号过滤时返回该用户全部商品，带账号过滤时只返回该账号；其他用户看不到任何商品。
	allRows, allErr := store.Items.ListForUser(ctx, ownerID, "")
	if allErr != nil || len(allRows) != 3 {
		t.Fatalf("用户范围应返回全部 3 件商品: %d err=%v", len(allRows), allErr)
	}
	// got 保存 overrides 调用的返回结果，供随后的断言使用。
	if got := overrides(allRows); got[cookieID+"/same-item"] != ItemAIOverrideDisabled || got[otherCookie+"/same-item"] != ItemAIOverrideInherit {
		t.Fatalf("用户范围的 ai_override 不一致: %v", got)
	}
	// err、filtered 保存 ListForUser 调用的返回值及其错误，随后断言是否符合该场景的预期。
	if filtered, err := store.Items.ListForUser(ctx, ownerID, otherCookie); err != nil || len(filtered) != 1 || filtered[0].CookieID != otherCookie {
		t.Fatalf("账号过滤结果不一致: %+v err=%v", filtered, err)
	}
	// err、foreign 保存 ListForUser 调用的返回值及其错误，随后断言是否符合该场景的预期。
	if foreign, err := store.Items.ListForUser(ctx, ownerID+999, ""); err != nil || len(foreign) != 0 {
		t.Fatalf("其他用户不得看到商品: %+v err=%v", foreign, err)
	}
	// 逻辑删除后的商品不再出现在任何查询中。
	if err := store.Items.Delete(ctx, cookieID, "same-item"); err != nil {
		t.Fatal(err)
	}
	// err、rows 保存 AllForCookie 调用的返回值及其错误，随后断言是否符合该场景的预期。
	if rows, err := store.Items.AllForCookie(ctx, cookieID); err != nil || len(rows) != 1 {
		t.Fatalf("删除后列表应只剩 1 件: %+v err=%v", rows, err)
	}
	// 数据库不可用时全部查询返回错误。
	if err := store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Items.AllForCookie(ctx, cookieID); err == nil {
		t.Error("AllForCookie 数据库故障必须返回错误")
	}
	if _, err := store.Items.GetByCookieItem(ctx, cookieID, "plain-item"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("GetByCookieItem 数据库故障必须返回基础设施错误，got %v", err)
	}
	if _, err := store.Items.ListForUser(ctx, ownerID, ""); err == nil {
		t.Error("ListForUser 数据库故障必须返回错误")
	}
}
