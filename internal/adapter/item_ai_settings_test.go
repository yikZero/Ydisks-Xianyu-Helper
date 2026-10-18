package adapter

import (
	"context"
	"testing"

	itemapp "xianyu-go/internal/application/items"
	"xianyu-go/internal/db"
)

// TestItemAISettingsRepositoryRejectsMissingStore 验证商品 AI 配置适配器缺少依赖时每个方法都快速失败，不伪装成功。
func TestItemAISettingsRepositoryRejectsMissingStore(t *testing.T) {
	ctx := context.Background()
	// repositories 覆盖三种缺少依赖的构造方式：nil 工厂、空 Store 工厂以及 nil 接收者。
	repositories := map[string]*ItemAISettingsRepository{
		"nil工厂":  NewItemAISettingsRepository(nil),
		"空仓储":    {},
		"nil接收者": nil,
	}
	// name、repository 是当前验证的装配场景及其适配器。
	for name, repository := range repositories {
		t.Run(name, func(t *testing.T) {
			if _, err := repository.AccountOwned(ctx, 1, "cid"); err == nil {
				t.Fatal("缺少账号仓储时 AccountOwned 必须返回错误")
			}
			if _, err := repository.ItemExists(ctx, "cid", "item"); err == nil {
				t.Fatal("缺少商品仓储时 ItemExists 必须返回错误")
			}
			if _, err := repository.Get(ctx, "cid", "item"); err == nil {
				t.Fatal("缺少配置存储时 Get 必须返回错误")
			}
			if err := repository.Upsert(ctx, itemapp.ItemAISettings{CookieID: "cid", ItemID: "item", Override: itemapp.ItemAIOverrideEnabled}); err == nil {
				t.Fatal("缺少配置存储时 Upsert 必须返回错误")
			}
			if err := repository.Delete(ctx, "cid", "item"); err == nil {
				t.Fatal("缺少配置存储时 Delete 必须返回错误")
			}
		})
	}
}

// TestItemDependenciesCreatesItemAISettingsRepository 验证商品依赖工厂能创建绑定同一 Store 的 AI 配置适配器，且 nil 工厂返回 nil。
func TestItemDependenciesCreatesItemAISettingsRepository(t *testing.T) {
	// dependencies、err 是由真实 Store 构造的商品依赖及其错误。
	dependencies, err := NewItemDependencies(db.NewStore(nil, db.DialectSQLite))
	if err != nil {
		t.Fatalf("NewItemDependencies: %v", err)
	}
	if dependencies.NewItemAISettingsRepository() == nil {
		t.Fatal("有效依赖必须返回商品 AI 配置适配器")
	}
	// missing 是未初始化的依赖工厂，调用方应得到 nil 而不是 panic。
	var missing *ItemDependencies
	if missing.NewItemAISettingsRepository() != nil {
		t.Fatal("nil 依赖工厂不应创建适配器")
	}
}

// TestItemAISettingsRepositoryPersistsAndIsolatesAccounts 验证适配器在真实 SQLite 上完成归属判断、商品存在性、读写与删除，
// 并且账号归属只按所属用户成立、已删除商品视为不存在。
func TestItemAISettingsRepositoryPersistsAndIsolatesAccounts(t *testing.T) {
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	ctx := context.Background()
	// admin 是拥有测试账号 cid 的用户。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// dependencies、depErr 是绑定测试 Store 的商品依赖。
	dependencies, depErr := NewItemDependencies(store)
	if depErr != nil {
		t.Fatal(depErr)
	}
	// repository 是被测的商品 AI 配置适配器。
	repository := dependencies.NewItemAISettingsRepository()

	// owned、ownedErr 验证所属用户拥有账号 cid。
	if owned, ownedErr := repository.AccountOwned(ctx, admin.ID, "cid"); ownedErr != nil || !owned {
		t.Fatalf("所属用户必须拥有账号: owned=%v err=%v", owned, ownedErr)
	}
	// 其他用户或不存在的账号不得被判定为拥有者。
	if owned, ownedErr := repository.AccountOwned(ctx, admin.ID+999, "cid"); ownedErr != nil || owned {
		t.Fatalf("其他用户不得拥有账号: owned=%v err=%v", owned, ownedErr)
	}
	// owned、ownedErr 保存 AccountOwned 调用的返回值及其错误，随后断言是否符合该场景的预期。
	if owned, ownedErr := repository.AccountOwned(ctx, admin.ID, "no-such-account"); ownedErr != nil || owned {
		t.Fatalf("不存在的账号不得被拥有: owned=%v err=%v", owned, ownedErr)
	}

	// 商品尚未同步时 ItemExists 必须为 false 且不是错误。
	if exists, existsErr := repository.ItemExists(ctx, "cid", "item-1"); existsErr != nil || exists {
		t.Fatalf("未同步商品不得存在: exists=%v err=%v", exists, existsErr)
	}
	// upsertErr 保存 Upsert 调用返回的错误，随后断言它是否符合该场景的预期。
	if upsertErr := store.Items.Upsert(ctx, &db.ItemInfoRow{CookieID: "cid", ItemID: "item-1", ItemTitle: "相机"}); upsertErr != nil {
		t.Fatal(upsertErr)
	}
	// exists、existsErr 保存 ItemExists 调用的返回值及其错误，随后断言是否符合该场景的预期。
	if exists, existsErr := repository.ItemExists(ctx, "cid", "item-1"); existsErr != nil || !exists {
		t.Fatalf("已同步商品必须存在: exists=%v err=%v", exists, existsErr)
	}
	// 同一商品标识挂在其他账号下不得被误判为存在。
	if exists, existsErr := repository.ItemExists(ctx, "other-account", "item-1"); existsErr != nil || exists {
		t.Fatalf("商品存在性必须按账号隔离: exists=%v err=%v", exists, existsErr)
	}

	// 无独立记录时读取应得到继承账号的默认配置。
	defaults, defaultsErr := repository.Get(ctx, "cid", "item-1")
	if defaultsErr != nil || defaults.Override != itemapp.ItemAIOverrideInherit || defaults.Context != "" || defaults.CookieID != "cid" || defaults.ItemID != "item-1" {
		t.Fatalf("无记录必须返回继承默认值: %+v err=%v", defaults, defaultsErr)
	}

	// 保存强制启用与专属资料后，读取必须原样返回。
	if saveErr := repository.Upsert(ctx, itemapp.ItemAISettings{CookieID: "cid", ItemID: "item-1", Override: itemapp.ItemAIOverrideEnabled, Context: "含原装电池"}); saveErr != nil {
		t.Fatal(saveErr)
	}
	// saved、savedErr 保存 Get 调用的返回值及其错误，随后断言是否符合该场景的预期。
	saved, savedErr := repository.Get(ctx, "cid", "item-1")
	if savedErr != nil || saved.Override != itemapp.ItemAIOverrideEnabled || saved.Context != "含原装电池" {
		t.Fatalf("保存后读取不一致: %+v err=%v", saved, savedErr)
	}
	// 再次保存同一商品必须覆盖而不是新增第二行。
	if saveErr := repository.Upsert(ctx, itemapp.ItemAISettings{CookieID: "cid", ItemID: "item-1", Override: itemapp.ItemAIOverrideDisabled, Context: ""}); saveErr != nil {
		t.Fatal(saveErr)
	}
	// overwritten、overwrittenErr 保存 Get 调用的返回值及其错误，随后断言是否符合该场景的预期。
	overwritten, overwrittenErr := repository.Get(ctx, "cid", "item-1")
	if overwrittenErr != nil || overwritten.Override != itemapp.ItemAIOverrideDisabled || overwritten.Context != "" {
		t.Fatalf("覆盖保存不一致: %+v err=%v", overwritten, overwrittenErr)
	}

	// 删除后读取恢复为继承默认值。
	if deleteErr := repository.Delete(ctx, "cid", "item-1"); deleteErr != nil {
		t.Fatal(deleteErr)
	}
	// reset、resetErr 保存 Get 调用的返回值及其错误，随后断言是否符合该场景的预期。
	reset, resetErr := repository.Get(ctx, "cid", "item-1")
	if resetErr != nil || reset.Override != itemapp.ItemAIOverrideInherit || reset.Context != "" {
		t.Fatalf("删除后必须回到继承默认值: %+v err=%v", reset, resetErr)
	}

	// 商品被逻辑删除后不再存在，即使仍有历史配置也不能被管理。
	if deleteItemErr := store.Items.Delete(ctx, "cid", "item-1"); deleteItemErr != nil {
		t.Fatal(deleteItemErr)
	}
	// exists、existsErr 保存 ItemExists 调用的返回值及其错误，随后断言是否符合该场景的预期。
	if exists, existsErr := repository.ItemExists(ctx, "cid", "item-1"); existsErr != nil || exists {
		t.Fatalf("已删除商品必须视为不存在: exists=%v err=%v", exists, existsErr)
	}
}
