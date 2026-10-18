package adapter

import (
	"context"
	"errors"
	"testing"

	keywordsapp "xianyu-go/internal/application/keywords"
)

// TestKeywordRepositoryCRUDMapping 验证关键词和指定商品回复的 SQLite 映射及成功路径。
func TestKeywordRepositoryCRUDMapping(t *testing.T) {
	// store 是使用临时 SQLite 数据库的测试存储。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// repository 是待验证的关键词数据库适配器。
	repository := NewKeywordRepository(store)
	ctx := context.Background()
	// owner 是测试模板中的关键词所有者。
	owner, ownerErr := store.Users.GetByUsername(ctx, "admin")
	if ownerErr != nil {
		t.Fatal(ownerErr)
	}
	// keywordID、createErr 保存关键词创建结果。
	keywordID, createErr := repository.Add(ctx, owner.ID, "cid", keywordsapp.Draft{Keyword: "价格", Reply: "50元", Type: "text", ItemID: "item-1"})
	if createErr != nil || keywordID <= 0 {
		t.Fatalf("创建关键词失败 id=%d err=%v", keywordID, createErr)
	}
	// rows、listErr 保存关键词列表结果。
	rows, listErr := repository.List(ctx, owner.ID, "cid")
	if listErr != nil || len(rows) != 1 || rows[0].Keyword != "价格" {
		t.Fatalf("关键词列表异常 rows=%+v err=%v", rows, listErr)
	}
	// updateErr 保存关键词更新结果。
	updateErr := repository.Update(ctx, owner.ID, "cid", keywordID, keywordsapp.Draft{Keyword: "新价格", Reply: "60元", Type: "text"})
	if updateErr != nil {
		t.Fatal(updateErr)
	}
	// replaceErr 保存批量替换结果。
	replaceErr := repository.Replace(ctx, owner.ID, "cid", []keywordsapp.Draft{{Keyword: "图片", Type: "image", ImageURL: "https://example.invalid/a.png"}})
	if replaceErr != nil {
		t.Fatal(replaceErr)
	}
	// replacedRows、replacedErr 保存批量替换后的列表结果。
	replacedRows, replacedErr := repository.List(ctx, owner.ID, "cid")
	if replacedErr != nil || len(replacedRows) != 1 || replacedRows[0].Type != "image" {
		t.Fatalf("替换结果异常 rows=%+v err=%v", replacedRows, replacedErr)
	}
	// deleteIndexErr 保存按稳定索引删除替换后关键词的结果。
	deleteIndexErr := repository.DeleteByIndex(ctx, owner.ID, "cid", 0)
	if deleteIndexErr != nil {
		t.Fatal(deleteIndexErr)
	}
	// setErr 保存指定商品回复写入结果。
	if setErr := repository.SetItemReply(ctx, owner.ID, "cid", "item-1", "专属回复"); setErr != nil {
		t.Fatal(setErr)
	}
	// itemReply、getErr 保存指定商品回复读取结果。
	itemReply, getErr := repository.GetItemReply(ctx, owner.ID, "cid", "item-1")
	if getErr != nil || itemReply.ReplyContent != "专属回复" {
		t.Fatalf("商品回复读取异常 row=%+v err=%v", itemReply, getErr)
	}
	// itemRows、itemListErr 保存指定商品回复列表结果。
	itemRows, itemListErr := repository.ListItemReplies(ctx, owner.ID)
	if itemListErr != nil || len(itemRows) != 1 || itemRows[0].CookieID != "cid" {
		t.Fatalf("商品回复列表异常 rows=%+v err=%v", itemRows, itemListErr)
	}
	// deleteReplyErr 保存指定商品回复删除结果。
	if deleteReplyErr := repository.DeleteItemReply(ctx, owner.ID, "cid", "item-1"); deleteReplyErr != nil {
		t.Fatal(deleteReplyErr)
	}
	// deleteErr 保存关键词 ID 删除结果。
	if deleteErr := repository.DeleteByID(ctx, owner.ID, "cid", keywordID); deleteErr != nil && !errors.Is(deleteErr, keywordsapp.ErrNotFound) {
		t.Fatal(deleteErr)
	}
}

// TestKeywordRepositoryRejectsCrossUserAccess 验证跨用户账号无法读写关键词或商品回复。
func TestKeywordRepositoryRejectsCrossUserAccess(t *testing.T) {
	// store 是使用临时 SQLite 数据库的测试存储。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	ctx := context.Background()
	// created、createErr 保存第二个用户的创建结果。
	created, createErr := store.Users.Create(ctx, "other", "other@example.invalid", "pw")
	if createErr != nil || !created {
		t.Fatal(createErr)
	}
	// other 是第二个用户的非敏感身份摘要。
	other, otherErr := store.Users.GetByUsername(ctx, "other")
	if otherErr != nil {
		t.Fatal(otherErr)
	}
	// saveErr 保存第二个用户账号的创建结果。
	if saveErr := store.Cookies.Save(ctx, "other-cid", "cookie", other.ID); saveErr != nil {
		t.Fatal(saveErr)
	}
	// repository 是待验证的关键词数据库适配器。
	repository := NewKeywordRepository(store)
	// err 保存跨用户读取结果。
	_, err := repository.List(ctx, 1, "other-cid")
	if !errors.Is(err, keywordsapp.ErrForbidden) {
		t.Fatalf("跨用户读取应返回 ErrForbidden，err=%v", err)
	}
	// writeErr 保存跨用户写入结果。
	writeErr := repository.SetItemReply(ctx, 1, "other-cid", "item", "reply")
	if !errors.Is(writeErr, keywordsapp.ErrForbidden) {
		t.Fatalf("跨用户写入应返回 ErrForbidden，err=%v", writeErr)
	}
}

// TestKeywordRepositoryPropagatesInfrastructureErrors 验证数据库故障不伪装为资源不存在。
func TestKeywordRepositoryPropagatesInfrastructureErrors(t *testing.T) {
	// store 是随后主动关闭数据库连接的测试存储。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// repository 是绑定测试存储的关键词适配器。
	repository := NewKeywordRepository(store)
	if closeErr := store.DB.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	// err 保存数据库关闭后的列表结果。
	_, err := repository.List(context.Background(), 1, "cid")
	if err == nil || errors.Is(err, keywordsapp.ErrNotFound) || errors.Is(err, keywordsapp.ErrForbidden) {
		t.Fatalf("数据库故障应原样返回，err=%v", err)
	}
	// missingErr 保存缺少数据库适配器依赖时的装配错误。
	_, missingErr := NewKeywordRepository(nil).List(context.Background(), 1, "cid")
	if missingErr == nil {
		t.Fatal("缺少 Store 时应返回装配错误")
	}
}

// TestKeywordRepositoryCoversClosedDatabaseOperations 验证关键词和商品回复端点传播数据库故障。
func TestKeywordRepositoryCoversClosedDatabaseOperations(t *testing.T) {
	// store 是随后主动关闭数据库连接的测试存储。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// repository 是绑定已关闭数据库的关键词适配器。
	repository := NewKeywordRepository(store)
	if closeErr := store.DB.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	// ctx 是本测试全部数据库操作使用的非取消上下文。
	ctx := context.Background()
	// operations 保存需要统一验证底层错误传播的关键词操作结果。
	operations := []struct {
		name string
		err  error
	}{
		{name: "列表", err: func() error {
			// err 表示关键词列表在关闭数据库后的底层错误。
			_, err := repository.List(ctx, 1, "cid")
			return err
		}()},
		{name: "添加", err: func() error {
			// err 表示关键词创建在关闭数据库后的底层错误。
			_, err := repository.Add(ctx, 1, "cid", keywordsapp.Draft{})
			return err
		}()},
		{name: "替换", err: repository.Replace(ctx, 1, "cid", nil)},
		{name: "更新", err: repository.Update(ctx, 1, "cid", 1, keywordsapp.Draft{})},
		{name: "按 ID 删除", err: repository.DeleteByID(ctx, 1, "cid", 1)},
		{name: "按索引删除", err: repository.DeleteByIndex(ctx, 1, "cid", 0)},
		{name: "商品回复列表", err: func() error {
			// err 表示商品回复列表在关闭数据库后的底层错误。
			_, err := repository.ListItemReplies(ctx, 1)
			return err
		}()},
		{name: "商品回复读取", err: func() error {
			// err 表示商品回复读取在关闭数据库后的底层错误。
			_, err := repository.GetItemReply(ctx, 1, "cid", "item")
			return err
		}()},
		{name: "商品回复写入", err: repository.SetItemReply(ctx, 1, "cid", "item", "reply")},
		{name: "商品回复删除", err: repository.DeleteItemReply(ctx, 1, "cid", "item")},
	}
	// operation 表示当前待验证的关键词操作及其底层结果。
	for _, operation := range operations {
		if operation.err == nil {
			t.Errorf("%s 未传播数据库故障", operation.name)
		}
	}
}

// TestKeywordRepositoryMapsMissingOwnedAccount 验证账号不存在时归属授权返回资源缺失错误。
func TestKeywordRepositoryMapsMissingOwnedAccount(t *testing.T) {
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// repository 是待验证的关键词适配器。
	repository := NewKeywordRepository(store)
	// err 保存不存在账号的归属授权结果。
	_, err := repository.List(context.Background(), 1, "missing-cookie")
	if !errors.Is(err, keywordsapp.ErrNotFound) {
		t.Fatalf("不存在账号应映射为 ErrNotFound，err=%v", err)
	}
}

var _ keywordsapp.Repository = (*KeywordRepository)(nil)

// TestKeywordRepositoryMapsMissingRulesAndInvalidIdentity 验证目标规则或商品回复不存在时统一映射为应用层 ErrNotFound，
// 并且用户标识非法、账号标识为空时在访问数据库之前就被拒绝。
func TestKeywordRepositoryMapsMissingRulesAndInvalidIdentity(t *testing.T) {
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// repository 是被测的关键词适配器。
	repository := NewKeywordRepository(store)
	ctx := context.Background()
	// owner 是账号 cid 的所有者。
	owner, ownerErr := store.Users.GetByUsername(ctx, "admin")
	if ownerErr != nil {
		t.Fatal(ownerErr)
	}
	// draft 是用于更新的有效草稿。
	draft := keywordsapp.Draft{Keyword: "价格", Reply: "50元", Type: "text"}
	if err := repository.Update(ctx, owner.ID, "cid", 9999, draft); !errors.Is(err, keywordsapp.ErrNotFound) {
		t.Fatalf("更新不存在的规则应映射为 ErrNotFound，got %v", err)
	}
	if err := repository.DeleteByID(ctx, owner.ID, "cid", 9999); !errors.Is(err, keywordsapp.ErrNotFound) {
		t.Fatalf("删除不存在的规则应映射为 ErrNotFound，got %v", err)
	}
	if err := repository.DeleteByIndex(ctx, owner.ID, "cid", 3); !errors.Is(err, keywordsapp.ErrNotFound) {
		t.Fatalf("越界序号应映射为 ErrNotFound，got %v", err)
	}
	if _, err := repository.GetItemReply(ctx, owner.ID, "cid", "no-item"); !errors.Is(err, keywordsapp.ErrNotFound) {
		t.Fatalf("不存在的商品回复应映射为 ErrNotFound，got %v", err)
	}
	// 用户标识非法时，所有入口都应返回 ErrInvalidUser，包括不经账号归属的列表入口。
	if _, err := repository.ListItemReplies(ctx, 0); !errors.Is(err, keywordsapp.ErrInvalidUser) {
		t.Fatalf("ListItemReplies 非法用户应 ErrInvalidUser，got %v", err)
	}
	if _, err := repository.List(ctx, -1, "cid"); !errors.Is(err, keywordsapp.ErrInvalidUser) {
		t.Fatalf("List 非法用户应 ErrInvalidUser，got %v", err)
	}
	// 账号标识为空属于无效输入。
	if _, err := repository.List(ctx, owner.ID, ""); !errors.Is(err, keywordsapp.ErrInvalidInput) {
		t.Fatalf("空账号应 ErrInvalidInput，got %v", err)
	}
	// 关闭数据库后，通过归属校验之前的查询失败应原样返回基础设施错误。
	if closeErr := store.DB.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if _, err := repository.List(ctx, owner.ID, "cid"); err == nil || errors.Is(err, keywordsapp.ErrNotFound) {
		t.Fatalf("数据库故障应返回基础设施错误，got %v", err)
	}
	if err := repository.DeleteByID(ctx, owner.ID, "cid", 1); err == nil || errors.Is(err, keywordsapp.ErrNotFound) {
		t.Fatalf("数据库故障不得被映射为未找到，got %v", err)
	}
}
