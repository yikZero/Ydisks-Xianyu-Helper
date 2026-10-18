package adapter

import (
	"context"
	"errors"
	"testing"

	accountapp "xianyu-go/internal/application/account"
	"xianyu-go/internal/db"
)

// loginRepositoryCalls 调用账号登录仓储的全部入口并按名称返回错误，供未装配断言复用。
func loginRepositoryCalls(ctx context.Context, repository *AccountLoginRepository) map[string]error {
	// errs 按方法名保存调用错误。
	errs := map[string]error{}
	errs["CreateCookieOwned"] = repository.CreateCookieOwned(ctx, "cid", "v", 1)
	_, errs["LoadPlatformDetail"] = repository.LoadPlatformDetail(ctx, "cid")
	errs["UpdateFlatCookieOwned"] = repository.UpdateFlatCookieOwned(ctx, &accountapp.CredentialDetail{ID: "cid"}, "v")
	errs["UpdateRenewalCookie"] = repository.UpdateRenewalCookie(ctx, "cid", "v", "{}", 1)
	errs["ClearTokens"] = repository.ClearTokens(ctx, "cid")
	errs["UpdateProfile"] = repository.UpdateProfile(ctx, "cid", "n", "a")
	_, errs["FindAccount"] = repository.FindAccount(ctx, "cid")
	errs["UpdateFlatCookieOwnedForQR"] = repository.UpdateFlatCookieOwnedForQR(ctx, "cid", "v")
	errs["UpdateCookieSnapshotOwned"] = repository.UpdateCookieSnapshotOwned(ctx, "cid", "v", nil)
	_, errs["GetOwnedSummary"] = repository.GetOwnedSummary(ctx, 1, "cid")
	errs["DeleteOwned"] = repository.DeleteOwned(ctx, 1, "cid")
	return errs
}

// TestAccountLoginRepositoryRejectsUnconfiguredStores 验证缺少 Store 或 Cookie 仓储时每个入口都安全失败。
func TestAccountLoginRepositoryRejectsUnconfiguredStores(t *testing.T) {
	ctx := context.Background()
	// nilRepository 是空指针适配器。
	var nilRepository *AccountLoginRepository
	// repository 遍历空指针、缺 Store 与缺 Cookie 仓储三种形态。
	for _, repository := range []*AccountLoginRepository{nilRepository, NewAccountLoginRepository(nil), NewAccountLoginRepository(&db.Store{})} {
		// err、name 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；当前被检查的调用或用例名称，用于失败时定位。
		for name, err := range loginRepositoryCalls(ctx, repository) {
			// ClearTokens 在 Store 存在但 Token 仓储缺失时是兼容空操作，其余入口必须失败。
			if name == "ClearTokens" && repository != nil && repository.store != nil {
				if err != nil {
					t.Fatalf("缺 Token 仓储应为空操作: %v", err)
				}
				continue
			}
			if err == nil {
				t.Fatalf("%s 未装配时应返回错误", name)
			}
		}
		if repository.GetStatus(ctx, "cid") {
			t.Fatal("未装配时账号应视为停用")
		}
	}
}

// TestAccountLoginRepositoryGapOwnershipMapping 验证创建与摘要查询把归属冲突映射为应用层稳定错误。
func TestAccountLoginRepositoryGapOwnershipMapping(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// repository 是被测适配器。
	repository := NewAccountLoginRepository(store)
	if err := repository.CreateCookieOwned(ctx, "cid", "v", 99999); !errors.Is(err, accountapp.ErrForbidden) {
		t.Fatalf("他人账号创建应为 Forbidden: %v", err)
	}
	// admin 是账号现有所有者。
	admin, err := store.Users.GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.CreateCookieOwned(ctx, "cid", "v", admin.ID); !errors.Is(err, accountapp.ErrAlreadyExists) {
		t.Fatalf("重复创建应为 AlreadyExists: %v", err)
	}
	if _, err := repository.GetOwnedSummary(ctx, 99999, "cid"); !errors.Is(err, accountapp.ErrForbidden) {
		t.Fatalf("他人账号摘要应为 Forbidden: %v", err)
	}
	if _, err := repository.GetOwnedSummary(ctx, admin.ID, "no-such"); !errors.Is(err, accountapp.ErrNotFound) {
		t.Fatalf("不存在账号摘要应为 NotFound: %v", err)
	}
	if _, err := repository.LoadPlatformDetail(ctx, "no-such"); !errors.Is(err, accountapp.ErrCredentialNotFound) {
		t.Fatalf("不存在凭证应为 CredentialNotFound: %v", err)
	}
	// QR 适配器透传同一仓储，覆盖其薄包装。
	qr := NewQRLoginRepository(store)
	qr.LockCredentials("cid")()
	if _, err := qr.FindAccount(ctx, "cid"); err != nil {
		t.Fatal(err)
	}
	if err := qr.ClearTokens(ctx, "cid"); err != nil {
		t.Fatal(err)
	}
	if err := qr.UpdateFlatCookieOwned(ctx, "cid", "unb=1; a=b"); err != nil {
		t.Fatal(err)
	}
	if err := qr.UpdateCookieSnapshotOwned(ctx, "cid", "unb=1; a=b", nil); err != nil {
		t.Fatal(err)
	}
	if err := qr.CreateCookieOwned(ctx, "cid", "v", admin.ID); !errors.Is(err, accountapp.ErrAlreadyExists) {
		t.Fatalf("QR 重复创建: %v", err)
	}
}
