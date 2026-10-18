package adapter

import (
	"context"
	"errors"
	"testing"

	settingsapp "xianyu-go/internal/application/settings"
	"xianyu-go/internal/db"
)

// TestSettingsRepositoryDeterministicPaths 覆盖设置适配器的系统、用户、AI 和归属查询路径。
func TestSettingsRepositoryDeterministicPaths(t *testing.T) {
	t.Setenv("XIANYU_DATA_KEY", "settings-adapter-test-key")
	// nilRepository 验证空适配器的敏感键查询安全返回。
	var nilRepository *SettingsRepository
	if nilRepository.IsSensitiveSettingKey("smtp_password") || nilRepository.SensitiveSettingKeys() != nil {
		t.Fatal("nil settings repository should be safe")
	}
	// store、cleanup 提供真实 SQLite 设置存储。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// repository 保存绑定当前数据库的设置适配器。
	repository := NewSettingsRepository(store)
	ctx := context.Background()
	// admin、adminErr 保存测试用户身份。
	admin, adminErr := store.Users.GetByUsername(ctx, "admin")
	if adminErr != nil {
		t.Fatal(adminErr)
	}
	// secretSetErr 保存测试敏感系统设置写入结果，供受控读取端口验证审计闭环。
	if secretSetErr := repository.SetSystem(ctx, "ai_api_key", "adapter-secret"); secretSetErr != nil {
		t.Fatal(secretSetErr)
	}
	// secretValue、secretReadErr 保存敏感系统设置的受控读取结果。
	secretValue, secretReadErr := repository.ReadSensitiveSystem(ctx, admin.ID, "ai_api_key", "settings.use", "adapter")
	if secretReadErr != nil || secretValue != "adapter-secret" {
		t.Fatalf("敏感设置读取异常 value=%q err=%v", secretValue, secretReadErr)
	}
	if !repository.IsSensitiveSettingKey("SMTP_PASSWORD") || len(repository.SensitiveSettingKeys()) == 0 {
		t.Fatal("sensitive key metadata missing")
	}
	// err 表示普通系统设置写入错误。
	if err := repository.SetSystem(ctx, "ui.theme", "dark"); err != nil {
		t.Fatal(err)
	}
	// value、valueErr 保存普通系统设置读取结果。
	value, valueErr := repository.GetSystem(ctx, "ui.theme")
	if valueErr != nil || value != "dark" {
		t.Fatalf("value=%q err=%v", value, valueErr)
	}
	// public、publicErr 保存公开系统设置。
	public, publicErr := repository.PublicSystem(ctx)
	if publicErr != nil || public == nil {
		t.Fatalf("public=%v err=%v", public, publicErr)
	}
	// redacted、redactedErr 保存脱敏系统设置。
	redacted, redactedErr := repository.RedactedSystem(ctx)
	if redactedErr != nil || redacted == nil {
		t.Fatalf("redacted=%v err=%v", redacted, redactedErr)
	}
	// err 表示普通设置和清除敏感设置的原子变更结果。
	if err := repository.ApplySystemChanges(ctx, map[string]string{"ui.locale": "zh-CN"}, map[string]settingsapp.SecretChange{"smtp_password": {Action: "clear"}}); err != nil {
		t.Fatal(err)
	}
	// err 表示非敏感审计记录写入错误。
	if err := repository.AddAudit(ctx, settingsapp.AuditRecord{UserID: admin.ID, Action: "read", Resource: "settings", Keys: []string{"ui.theme"}, Outcome: "accepted"}); err != nil {
		t.Fatal(err)
	}
	// err 表示用户设置写入错误。
	if err := repository.SetUser(ctx, admin.ID, "density", "compact"); err != nil {
		t.Fatal(err)
	}
	// userSettings、userSettingsErr 保存用户设置列表。
	userSettings, userSettingsErr := repository.ListUser(ctx, admin.ID)
	if userSettingsErr != nil || userSettings["density"] != "compact" {
		t.Fatalf("user settings=%v err=%v", userSettings, userSettingsErr)
	}
	// userValue、userValueErr 保存用户单项设置。
	userValue, userValueErr := repository.GetUser(ctx, admin.ID, "density")
	if userValueErr != nil || userValue != "compact" {
		t.Fatalf("user value=%q err=%v", userValue, userValueErr)
	}
	// ownerID、ownerErr 保存账号归属查询结果。
	ownerID, ownerErr := repository.CheckOwnership(ctx, admin.ID, " cid ")
	if ownerErr != nil || ownerID != admin.ID {
		t.Fatalf("owner=%d err=%v", ownerID, ownerErr)
	}
	// missingOwner、missingOwnerErr 验证不存在账号的统一错误。
	missingOwner, missingOwnerErr := repository.CheckOwnership(ctx, admin.ID, "missing")
	if missingOwner != 0 || !errors.Is(missingOwnerErr, settingsapp.ErrAccountNotFound) {
		t.Fatalf("missing owner=%d err=%v", missingOwner, missingOwnerErr)
	}
	// aiSettings、aiSettingsErr 保存用户范围内的 AI 配置摘要。
	if err := repository.UpsertAIReply(ctx, "cid", settingsapp.AIReplySettings{AIEnabled: true, MaxBargainRounds: 2, AIMode: "bargain_only"}); err != nil {
		t.Fatal(err)
	}
	// aiSettings、aiSettingsErr 保存用户范围内的 AI 配置摘要及读取错误。
	aiSettings, aiSettingsErr := repository.ListAIReply(ctx, admin.ID)
	if aiSettingsErr != nil || len(aiSettings) != 1 || aiSettings[0].CookieID != "cid" {
		t.Fatalf("ai settings=%+v err=%v", aiSettings, aiSettingsErr)
	}
	// aiSetting、aiSettingErr 保存单账号 AI 配置摘要。
	aiSetting, aiSettingErr := repository.GetAIReply(ctx, admin.ID, "cid")
	if aiSettingErr != nil || !aiSetting.AIEnabled {
		t.Fatalf("ai setting=%+v err=%v", aiSetting, aiSettingErr)
	}
	// missingAI、missingAIErr 验证用户范围内不存在 AI 配置的错误。
	missingAI, missingAIErr := repository.GetAIReply(ctx, admin.ID, "other")
	if missingAI.CookieID != "" || !errors.Is(missingAIErr, settingsapp.ErrConfigNotFound) {
		t.Fatalf("missing AI=%+v err=%v", missingAI, missingAIErr)
	}
	// hasRule、hasRuleErr 保存自动化改价规则存在性查询结果。
	hasRule, hasRuleErr := repository.HasEnabledAdjustPriceRule(ctx, "cid")
	if hasRuleErr != nil || hasRule {
		t.Fatalf("has rule=%v err=%v", hasRule, hasRuleErr)
	}
}

// TestSettingsRepositoryAIReplyModeAndPricingConflict 验证账号 AI 设置在真实 SQLite 上的需求：
// 工作模式随设置保存并能从列表与单项读取中原样取回；账号已有启用的固定改价规则时，启用 AI 议价必须以冲突拒绝且不写入；
// 未启用 AI 议价时不受固定规则影响。
func TestSettingsRepositoryAIReplyModeAndPricingConflict(t *testing.T) {
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// repository 是被测的设置适配器。
	repository := NewSettingsRepository(store)
	ctx := context.Background()
	// owner 是账号 cid 的所有者。
	owner, ownerErr := store.Users.GetByUsername(ctx, "admin")
	if ownerErr != nil {
		t.Fatal(ownerErr)
	}
	// 保存全场景模式的设置并读回。
	saved := settingsapp.AIReplySettings{AIEnabled: true, MaxDiscountPercent: 10, MaxDiscountAmount: 50, MaxBargainRounds: 3, CustomPrompts: "当天发货", AIMode: "full_service"}
	if err := repository.UpsertAIReply(ctx, "cid", saved); err != nil {
		t.Fatal(err)
	}
	// single、singleErr 保存 GetAIReply 调用的返回值及其错误，随后断言是否符合该场景的预期。
	single, singleErr := repository.GetAIReply(ctx, owner.ID, "cid")
	if singleErr != nil || single.AIMode != "full_service" || single.CustomPrompts != "当天发货" || !single.AIEnabled {
		t.Fatalf("单项读取不一致: %+v err=%v", single, singleErr)
	}
	// list、listErr 保存 ListAIReply 调用的返回值及其错误，随后断言是否符合该场景的预期。
	list, listErr := repository.ListAIReply(ctx, owner.ID)
	if listErr != nil || len(list) != 1 || list[0].AIMode != "full_service" {
		t.Fatalf("列表读取不一致: %+v err=%v", list, listErr)
	}
	// 未配置的账号返回“未配置”，而不是零值设置。
	if _, err := repository.GetAIReply(ctx, owner.ID, "no-such-account"); !errors.Is(err, settingsapp.ErrConfigNotFound) {
		t.Fatalf("未配置账号应返回 ErrConfigNotFound，got %v", err)
	}

	// 账号存在启用的固定改价规则时，再次启用 AI 议价必须冲突，且已保存的设置不被覆盖。
	if _, err := store.Automation.Create(ctx, db.AutomationRuleInput{
		UserID: owner.ID, CookieID: "cid", ItemID: "item-adjust", Name: "固定改价", TriggerType: "price_change", Enabled: true, Priority: 1,
		Actions: []db.AutomationActionInput{{ActionType: "adjust_price", Enabled: true, SortOrder: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	// err、has 保存 HasEnabledAdjustPriceRule 调用的返回值及其错误，随后断言是否符合该场景的预期。
	if has, err := repository.HasEnabledAdjustPriceRule(ctx, " cid "); err != nil || !has {
		t.Fatalf("应检测到固定改价规则（含空白账号标识）: has=%v err=%v", has, err)
	}
	// conflicting 是在已保存设置基础上改为仅砍价模式的草稿，用于触发固定改价规则冲突。
	conflicting := saved
	conflicting.AIMode = "bargain_only"
	if err := repository.UpsertAIReply(ctx, "cid", conflicting); !errors.Is(err, settingsapp.ErrPricingModeConflict) {
		t.Fatalf("启用 AI 议价与固定改价规则应冲突，got %v", err)
	}
	// after、err 保存 GetAIReply 调用的返回值及其错误，随后断言是否符合该场景的预期。
	if after, err := repository.GetAIReply(ctx, owner.ID, "cid"); err != nil || after.AIMode != "full_service" {
		t.Fatalf("冲突失败不得覆盖已保存设置: %+v err=%v", after, err)
	}
	// 关闭 AI 议价时不受固定规则影响，可保存其他模式。
	disabled := conflicting
	disabled.AIEnabled = false
	if err := repository.UpsertAIReply(ctx, "cid", disabled); err != nil {
		t.Fatalf("关闭 AI 议价应不受固定改价规则影响: %v", err)
	}
}

// TestSettingsRepositoryAIReplyRejectsUninitializedStore 验证设置适配器缺少依赖时，AI 设置相关入口都返回错误而不是 panic 或伪装成功。
func TestSettingsRepositoryAIReplyRejectsUninitializedStore(t *testing.T) {
	// repository 是没有任何存储的设置适配器。
	repository := &SettingsRepository{}
	ctx := context.Background()
	if _, err := repository.ListAIReply(ctx, 1); err == nil {
		t.Error("ListAIReply 未初始化必须返回错误")
	}
	if _, err := repository.GetAIReply(ctx, 1, "cid"); err == nil {
		t.Error("GetAIReply 未初始化必须返回错误")
	}
	if err := repository.UpsertAIReply(ctx, "cid", settingsapp.AIReplySettings{}); err == nil {
		t.Error("UpsertAIReply 未初始化必须返回错误")
	}
	if _, err := repository.HasEnabledAdjustPriceRule(ctx, "cid"); err == nil {
		t.Error("HasEnabledAdjustPriceRule 未初始化必须返回错误")
	}
}
