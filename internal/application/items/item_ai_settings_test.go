package items

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// itemAISettingsRepositoryFake 是商品级 AI 应用服务测试使用的仓储替身。
type itemAISettingsRepositoryFake struct {
	owned    bool
	exists   bool
	settings ItemAISettings
	saved    ItemAISettings
	deleted  bool
	err      error
}

// AccountOwned 返回预设账号归属结果。
func (repository *itemAISettingsRepositoryFake) AccountOwned(context.Context, int64, string) (bool, error) {
	return repository.owned, repository.err
}

// ItemExists 返回预设商品存在性结果。
func (repository *itemAISettingsRepositoryFake) ItemExists(context.Context, string, string) (bool, error) {
	return repository.exists, repository.err
}

// Get 返回预设商品配置。
func (repository *itemAISettingsRepositoryFake) Get(context.Context, string, string) (ItemAISettings, error) {
	return repository.settings, repository.err
}

// Upsert 记录最后一次保存配置。
func (repository *itemAISettingsRepositoryFake) Upsert(_ context.Context, settings ItemAISettings) error {
	repository.saved = settings
	return repository.err
}

// Delete 记录默认配置删除动作。
func (repository *itemAISettingsRepositoryFake) Delete(context.Context, string, string) error {
	repository.deleted = true
	return repository.err
}

// TestItemAISettingsServiceValidation 验证归属、商品存在性、三态和资料长度边界。
func TestItemAISettingsServiceValidation(t *testing.T) {
	// repository 和 service 是默认允许访问的应用服务测试依赖。
	repository := &itemAISettingsRepositoryFake{owned: true, exists: true, settings: ItemAISettings{CookieID: "account", ItemID: "item", Override: ItemAIOverrideInherit}}
	// service 和 serviceErr 是待测应用服务及其构造错误。
	service, serviceErr := NewItemAISettingsService(repository)
	if serviceErr != nil {
		t.Fatal(serviceErr)
	}
	// saveErr 是规范化保存配置时的错误。
	if saveErr := service.Save(context.Background(), 1, ItemAISettings{CookieID: " account ", ItemID: " item ", Override: ItemAIOverrideEnabled, Context: "资料"}); saveErr != nil {
		t.Fatal(saveErr)
	}
	if repository.saved.CookieID != "account" || repository.saved.ItemID != "item" || repository.saved.Override != ItemAIOverrideEnabled {
		t.Fatalf("规范化保存异常: %+v", repository.saved)
	}
	// resetErr 是默认配置重置操作的错误。
	if resetErr := service.Save(context.Background(), 1, ItemAISettings{CookieID: "account", ItemID: "item", Override: ItemAIOverrideInherit}); resetErr != nil || !repository.deleted {
		t.Fatalf("默认配置重置异常: deleted=%v err=%v", repository.deleted, resetErr)
	}
	// invalidErr 是非法覆盖值保存错误。
	if invalidErr := service.Save(context.Background(), 1, ItemAISettings{CookieID: "account", ItemID: "item", Override: "invalid"}); !errors.Is(invalidErr, ErrItemAISettingsInvalidOverride) {
		t.Fatalf("非法覆盖值错误异常: %v", invalidErr)
	}
	// longErr 是超长商品资料保存错误。
	if longErr := service.Save(context.Background(), 1, ItemAISettings{CookieID: "account", ItemID: "item", Override: ItemAIOverrideInherit, Context: strings.Repeat("中", ItemAIContextMaxRunes+1)}); !errors.Is(longErr, ErrItemAISettingsContextTooLong) {
		t.Fatalf("超长资料错误异常: %v", longErr)
	}
	repository.owned = false
	// forbiddenErr 是跨用户读取配置错误。
	if _, forbiddenErr := service.Get(context.Background(), 1, "account", "item"); !errors.Is(forbiddenErr, ErrItemAISettingsForbidden) {
		t.Fatalf("跨用户访问错误异常: %v", forbiddenErr)
	}
	repository.owned, repository.exists = true, false
	// missingErr 是商品不存在时的读取错误。
	if _, missingErr := service.Get(context.Background(), 1, "account", "item"); !errors.Is(missingErr, ErrItemAISettingsItemNotFound) {
		t.Fatalf("商品缺失错误异常: %v", missingErr)
	}
}

// TestNewItemAISettingsServiceRequiresRepository 验证服务构造必须提供仓储端口，未初始化服务的每个入口都拒绝执行。
func TestNewItemAISettingsServiceRequiresRepository(t *testing.T) {
	// err、service 保存 NewItemAISettingsService 调用的返回值及其错误，随后断言是否符合该场景的预期。
	if service, err := NewItemAISettingsService(nil); err == nil || service != nil {
		t.Fatalf("缺少仓储时构造必须失败: service=%v err=%v", service, err)
	}
	// uninitialized 是没有仓储的零值服务，模拟装配遗漏。
	uninitialized := &ItemAISettingsService{}
	if _, err := uninitialized.Get(context.Background(), 1, "account", "item"); err == nil {
		t.Fatal("未初始化服务的 Get 必须返回错误")
	}
	if err := uninitialized.Save(context.Background(), 1, ItemAISettings{CookieID: "account", ItemID: "item", Override: ItemAIOverrideInherit}); err == nil {
		t.Fatal("未初始化服务的 Save 必须返回错误")
	}
	// nilService 是 nil 接收者，同样不得 panic。
	var nilService *ItemAISettingsService
	if _, err := nilService.Get(context.Background(), 1, "account", "item"); err == nil {
		t.Fatal("nil 服务的 Get 必须返回错误")
	}
}

// TestItemAISettingsServiceRejectsIncompleteIdentity 验证缺少用户、账号或商品标识时，Get 和 Save 在访问仓储前就拒绝，
// 且账号缺失按越权处理、商品缺失按不存在处理。
func TestItemAISettingsServiceRejectsIncompleteIdentity(t *testing.T) {
	// repository 默认允许访问；若校验被绕过，测试会通过 saved 或 deleted 发现副作用。
	repository := &itemAISettingsRepositoryFake{owned: true, exists: true}
	// service 是待测应用服务。
	service, serviceErr := NewItemAISettingsService(repository)
	if serviceErr != nil {
		t.Fatal(serviceErr)
	}
	ctx := context.Background()
	// getCases 覆盖 Get 的三类身份缺失。
	getCases := []struct {
		// name 是子用例说明。
		name string
		// userID、cookieID、itemID 是传入的身份。
		userID           int64
		cookieID, itemID string
		// want 是期望的稳定错误。
		want error
	}{
		{"用户为零", 0, "account", "item", ErrItemAISettingsForbidden},
		{"账号为空白", 1, "   ", "item", ErrItemAISettingsForbidden},
		{"商品为空白", 1, "account", "  ", ErrItemAISettingsItemNotFound},
	}
	// tc 是当前 Get 子用例。
	for _, tc := range getCases {
		if _, err := service.Get(ctx, tc.userID, tc.cookieID, tc.itemID); !errors.Is(err, tc.want) {
			t.Fatalf("Get %s: err=%v want %v", tc.name, err, tc.want)
		}
	}
	// saveCases 覆盖 Save 的身份缺失与资料编码非法。
	saveCases := []struct {
		// name 是子用例说明。
		name string
		// userID、settings 是传入的用户与配置。
		userID   int64
		settings ItemAISettings
		// want 是期望的稳定错误。
		want error
	}{
		{"用户为负", -1, ItemAISettings{CookieID: "account", ItemID: "item", Override: ItemAIOverrideEnabled}, ErrItemAISettingsForbidden},
		{"账号为空白", 1, ItemAISettings{CookieID: " ", ItemID: "item", Override: ItemAIOverrideEnabled}, ErrItemAISettingsForbidden},
		{"商品为空白", 1, ItemAISettings{CookieID: "account", ItemID: " ", Override: ItemAIOverrideEnabled}, ErrItemAISettingsItemNotFound},
		{"资料含非法 UTF-8", 1, ItemAISettings{CookieID: "account", ItemID: "item", Override: ItemAIOverrideEnabled, Context: "坏\xff"}, ErrItemAISettingsInvalidContext},
	}
	// tc 是当前 Save 子用例。
	for _, tc := range saveCases {
		if err := service.Save(ctx, tc.userID, tc.settings); !errors.Is(err, tc.want) {
			t.Fatalf("Save %s: err=%v want %v", tc.name, err, tc.want)
		}
	}
	if repository.saved.CookieID != "" || repository.deleted {
		t.Fatalf("校验失败不得触达仓储写入: saved=%+v deleted=%v", repository.saved, repository.deleted)
	}
}

// TestItemAISettingsServiceKeepsInfrastructureErrorsAndSavesContextOnlyInherit 验证仓储基础设施错误原样返回（不被误映射为越权或不存在），
// 并且“继承账号但保留商品资料”不会被当作默认配置删除，而是保存资料。
func TestItemAISettingsServiceKeepsInfrastructureErrorsAndSavesContextOnlyInherit(t *testing.T) {
	// infraErr 是仓储抛出的基础设施错误。
	infraErr := errors.New("数据库不可用")
	// failing 的每个端口都返回基础设施错误。
	failing := &itemAISettingsRepositoryFake{owned: true, exists: true, err: infraErr}
	// service 是绑定失败仓储的应用服务。
	service, serviceErr := NewItemAISettingsService(failing)
	if serviceErr != nil {
		t.Fatal(serviceErr)
	}
	ctx := context.Background()
	if _, err := service.Get(ctx, 1, "account", "item"); !errors.Is(err, infraErr) {
		t.Fatalf("Get 应保留基础设施错误，got %v", err)
	}
	if err := service.Save(ctx, 1, ItemAISettings{CookieID: "account", ItemID: "item", Override: ItemAIOverrideEnabled}); !errors.Is(err, infraErr) {
		t.Fatalf("Save 应保留基础设施错误，got %v", err)
	}

	// splitFake 让账号归属成功但商品存在性检查失败，覆盖第二步的基础设施错误。
	splitFake := &splitErrorRepository{itemAISettingsRepositoryFake: itemAISettingsRepositoryFake{owned: true, exists: true}, itemErr: infraErr}
	// splitService 是绑定该仓储的应用服务。
	splitService, splitErr := NewItemAISettingsService(splitFake)
	if splitErr != nil {
		t.Fatal(splitErr)
	}
	if err := splitService.Save(ctx, 1, ItemAISettings{CookieID: "account", ItemID: "item", Override: ItemAIOverrideEnabled}); !errors.Is(err, infraErr) {
		t.Fatalf("商品存在性检查失败应原样返回，got %v", err)
	}

	// healthy 是正常仓储；继承但带资料的配置必须走保存而不是删除。
	healthy := &itemAISettingsRepositoryFake{owned: true, exists: true}
	// healthyService 是绑定正常仓储的应用服务。
	healthyService, healthyErr := NewItemAISettingsService(healthy)
	if healthyErr != nil {
		t.Fatal(healthyErr)
	}
	if err := healthyService.Save(ctx, 1, ItemAISettings{CookieID: "account", ItemID: "item", Override: ItemAIOverrideInherit, Context: "仅资料"}); err != nil {
		t.Fatal(err)
	}
	if healthy.deleted || healthy.saved.Context != "仅资料" || healthy.saved.Override != ItemAIOverrideInherit {
		t.Fatalf("继承但有资料必须保存: deleted=%v saved=%+v", healthy.deleted, healthy.saved)
	}
	// 资料恰为上限字符数时必须允许保存（边界值）。
	if err := healthyService.Save(ctx, 1, ItemAISettings{CookieID: "account", ItemID: "item", Override: ItemAIOverrideEnabled, Context: strings.Repeat("中", ItemAIContextMaxRunes)}); err != nil {
		t.Fatalf("恰好上限字符数必须允许: %v", err)
	}
}

// splitErrorRepository 在账号归属通过后让商品存在性检查返回指定错误，用于覆盖授权的第二步失败。
type splitErrorRepository struct {
	itemAISettingsRepositoryFake
	// itemErr 是 ItemExists 返回的错误。
	itemErr error
}

// ItemExists 返回预设错误，模拟商品仓储故障。
func (repository *splitErrorRepository) ItemExists(context.Context, string, string) (bool, error) {
	return false, repository.itemErr
}
