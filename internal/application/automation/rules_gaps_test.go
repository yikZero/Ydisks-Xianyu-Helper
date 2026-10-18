package automation

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// failingProfileOwnership 在读取商品规格事实时返回固定错误，用于覆盖规格读取失败分支。
type failingProfileOwnership struct {
	// ruleOwnershipFake 提供通用归属行为。
	*ruleOwnershipFake
	err error
}

// GetItemDeliveryProfile 总是返回预置错误。
func (f *failingProfileOwnership) GetItemDeliveryProfile(context.Context, int64, string, string) (ItemDeliveryProfile, error) {
	return ItemDeliveryProfile{}, f.err
}

// TestNormalizeForUpdateGuardsAndRepositoryError 验证更新规范化的参数守卫与原规则读取失败透传。
func TestNormalizeForUpdateGuardsAndRepositoryError(t *testing.T) {
	// nilService 是未初始化的规则服务。
	var nilService *RuleService
	if _, err := nilService.NormalizeForUpdate(context.Background(), 1, 1, RuleDraft{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err=%v", err)
	}
	// boom 是原规则读取失败。
	boom := errors.New("repository down")
	// service 的仓储读取原规则会失败。
	service := NewRuleService(&ruleRepositoryFake{existingErr: boom}, &ruleOwnershipFake{})
	if _, err := service.NormalizeForUpdate(context.Background(), 1, 2, RuleDraft{}); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if _, err := service.NormalizeForUpdate(context.Background(), 0, 2, RuleDraft{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err=%v", err)
	}
}

// TestNormalizeSurfacesSKUSpecErrors 验证多规格发货动作缺少完整规格时，规则规范化直接返回规格错误。
func TestNormalizeSurfacesSKUSpecErrors(t *testing.T) {
	// service 使用无规格事实提供者的归属替身，因此靠动作文本里的分隔符推断多规格。
	service := NewRuleService(&ruleRepositoryFake{}, &ruleOwnershipFake{cardType: "text", cardEnabled: true})
	// _, err 是规格不完整的草稿被拒绝的原因。
	_, err := service.Normalize(context.Background(), 1, RuleDraft{CookieID: "a", TriggerType: TriggerOrderPaid, Actions: []ActionDraft{{
		ActionType: ActionSendCard, CardID: 1, ConfigJSON: `{"spec_name":"颜色;版本","spec_value":""}`,
	}}})
	if err == nil || !strings.Contains(err.Error(), "完整规格") {
		t.Fatalf("err=%v", err)
	}
}

// TestNormalizeRuleSKUConfigsBranches 覆盖规格事实读取失败、停用动作跳过、非法配置、规格不完整与规格名称顺序不一致。
func TestNormalizeRuleSKUConfigsBranches(t *testing.T) {
	ctx := context.Background()
	// boom 是商品规格事实读取失败。
	boom := errors.New("profile unavailable")
	// failing 的归属替身读取规格事实失败。
	failing := &failingProfileOwnership{ruleOwnershipFake: &ruleOwnershipFake{}, err: boom}
	if err := normalizeRuleSKUConfigs(ctx, failing, 1, "a", "item", nil); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	// plain 的归属替身不提供规格事实，仅按动作文本推断。
	plain := &ruleOwnershipFake{}
	// disabled 的动作被停用，应原样跳过而不解析配置。
	disabled := []ActionInput{{ActionType: ActionSendCard, Enabled: false, ConfigJSON: "not json"}, {ActionType: "other", Enabled: true, ConfigJSON: "not json"}}
	if err := normalizeRuleSKUConfigs(ctx, plain, 1, "a", "", disabled); err != nil {
		t.Fatalf("err=%v", err)
	}
	if err := normalizeRuleSKUConfigs(ctx, plain, 1, "a", "", []ActionInput{{ActionType: ActionSendCard, Enabled: true, ConfigJSON: "null"}}); err == nil {
		t.Fatal("null 配置应被拒绝")
	}
	if err := normalizeRuleSKUConfigs(ctx, plain, 1, "a", "", []ActionInput{{ActionType: ActionSendTemplate, Enabled: true, ConfigJSON: "[1]"}}); err == nil {
		t.Fatal("非对象配置应被拒绝")
	}
	// mismatch 的两个动作规格名称顺序不一致。
	mismatch := []ActionInput{
		{ActionType: ActionSendCard, Enabled: true, ConfigJSON: `{"spec_name":"颜色;版本","spec_value":"红;专业"}`},
		{ActionType: ActionSendCard, Enabled: true, ConfigJSON: `{"spec_name":"版本;颜色","spec_value":"专业;红"}`},
	}
	if err := normalizeRuleSKUConfigs(ctx, plain, 1, "a", "", mismatch); err == nil || !strings.Contains(err.Error(), "一致") {
		t.Fatalf("err=%v", err)
	}
	// consistent 的两个动作规格名称一致，应规范化通过。
	consistent := []ActionInput{
		{ActionType: ActionSendCard, Enabled: true, ConfigJSON: `{"spec_name":"颜色;版本","spec_value":"红;专业"}`},
		{ActionType: ActionSendTemplate, Enabled: true, ConfigJSON: `{"spec_name":"颜色;版本","spec_value":"蓝;基础"}`},
	}
	if err := normalizeRuleSKUConfigs(ctx, plain, 1, "a", "", consistent); err != nil {
		t.Fatalf("err=%v", err)
	}
}
