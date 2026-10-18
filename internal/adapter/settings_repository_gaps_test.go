package adapter

import (
	"context"
	"errors"
	"testing"

	settingsapp "xianyu-go/internal/application/settings"
	"xianyu-go/internal/db"
)

// settingsRepositoryCalls 调用设置仓储的全部带校验入口并按名称返回错误。
func settingsRepositoryCalls(ctx context.Context, repository *SettingsRepository) map[string]error {
	// errs 按方法名保存调用错误。
	errs := map[string]error{}
	_, errs["PublicSystem"] = repository.PublicSystem(ctx)
	_, errs["RedactedSystem"] = repository.RedactedSystem(ctx)
	_, errs["GetSystem"] = repository.GetSystem(ctx, "k")
	_, errs["ReadSensitiveSystem"] = repository.ReadSensitiveSystem(ctx, 1, "k", "a", "r")
	errs["ApplySystemChanges"] = repository.ApplySystemChanges(ctx, map[string]string{"a": "b"}, map[string]settingsapp.SecretChange{"s": {Action: "clear"}})
	errs["SetSystem"] = repository.SetSystem(ctx, "k", "v")
	errs["AddAudit"] = repository.AddAudit(ctx, settingsapp.AuditRecord{UserID: 1, Action: "a", Resource: "r"})
	_, errs["ListUser"] = repository.ListUser(ctx, 1)
	_, errs["GetUser"] = repository.GetUser(ctx, 1, "k")
	errs["SetUser"] = repository.SetUser(ctx, 1, "k", "v")
	_, errs["CheckOwnership"] = repository.CheckOwnership(ctx, 1, "cid")
	_, errs["ListAIReply"] = repository.ListAIReply(ctx, 1)
	_, errs["GetAIReply"] = repository.GetAIReply(ctx, 1, "cid")
	errs["UpsertAIReply"] = repository.UpsertAIReply(ctx, "cid", settingsapp.AIReplySettings{})
	_, errs["HasEnabledAdjustPriceRule"] = repository.HasEnabledAdjustPriceRule(ctx, "cid")
	return errs
}

// TestSettingsRepositoryRejectsUnconfiguredAdapter 验证未装配的设置仓储在每个入口返回错误，空指针的纯函数入口返回安全默认值。
func TestSettingsRepositoryRejectsUnconfiguredAdapter(t *testing.T) {
	// nilRepository 是空指针适配器。
	var nilRepository *SettingsRepository
	if nilRepository.IsSensitiveSettingKey("captcha.remote_secret_key") || nilRepository.SensitiveSettingKeys() != nil {
		t.Fatal("空适配器的敏感键查询应返回安全默认值")
	}
	// repository 遍历空指针与缺少全部子仓储的适配器。
	for _, repository := range []*SettingsRepository{nilRepository, NewSettingsRepository(&db.Store{})} {
		// err、name 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；当前被检查的调用或用例名称，用于失败时定位。
		for name, err := range settingsRepositoryCalls(context.Background(), repository) {
			if err == nil {
				t.Fatalf("%s 未装配时应返回错误", name)
			}
		}
	}
}

// TestSettingsRepositoryPartialStoreAndDatabaseFailures 覆盖缺审计/自动化存储、数据库故障与跨用户所有者查询。
func TestSettingsRepositoryPartialStoreAndDatabaseFailures(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// partial 只装配普通设置相关仓储，缺少审计与自动化规则存储。
	partial := NewSettingsRepository(&db.Store{Settings: store.Settings, UserSettings: store.UserSettings, AIReply: store.AIReply, Cookies: store.Cookies})
	if err := partial.AddAudit(ctx, settingsapp.AuditRecord{UserID: 1, Action: "a", Resource: "r"}); err == nil {
		t.Fatal("缺审计存储应失败")
	}
	if _, err := partial.HasEnabledAdjustPriceRule(ctx, "cid"); err == nil {
		t.Fatal("缺自动化存储应失败")
	}
	// admin 是账号 cid 的所有者；other 是第二个用户。
	admin, err := store.Users.GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	// repository 是完整装配的设置适配器。
	repository := NewSettingsRepository(store)
	// err、owner 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；测试中的局部对象，承载被测流程的输入、依赖或输出。
	if owner, err := repository.CheckOwnership(ctx, admin.ID, "cid"); err != nil || owner != admin.ID {
		t.Fatalf("本人账号: %d %v", owner, err)
	}
	// 其他用户查询他人账号时返回真实所有者。
	if owner, err := repository.CheckOwnership(ctx, admin.ID+100, "cid"); err != nil || owner != admin.ID {
		t.Fatalf("他人账号所有者: %d %v", owner, err)
	}
	if _, err := repository.CheckOwnership(ctx, admin.ID, "no-such"); !errors.Is(err, settingsapp.ErrAccountNotFound) {
		t.Fatalf("不存在账号: %v", err)
	}
	// 数据库关闭后冲突检查失败向上传播，所有权查询也失败。
	cleanup()
	if err := repository.UpsertAIReply(ctx, "cid", settingsapp.AIReplySettings{AIEnabled: true}); err == nil {
		t.Fatal("冲突检查失败应传播")
	}
	if _, err := repository.CheckOwnership(ctx, admin.ID, "cid"); err == nil {
		t.Fatal("数据库故障应传播")
	}
	if _, err := repository.CheckOwnership(ctx, admin.ID+100, "cid"); err == nil {
		t.Fatal("所有者查询故障应传播")
	}
}
