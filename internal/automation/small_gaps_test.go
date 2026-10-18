package automation

import (
	"context"
	"testing"

	"xianyu-go/internal/db"
)

// TestAccountTaskRepositoryAdapterGaps 覆盖账号任务仓储适配器的 Cookie 委托与空存储锁保护。
func TestAccountTaskRepositoryAdapterGaps(t *testing.T) {
	// release 是空存储时返回的空操作解锁函数，调用不得崩溃。
	release := storeAccountTaskRepository{}.LockAccountCredentials("cid")
	release()
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// repo 是委托真实仓储的适配器。
	repo := storeAccountTaskRepository{store: store}
	// value、err 保存夹具账号的 Cookie 读取结果。
	value, err := repo.GetValue(context.Background(), "cid")
	if err != nil || value == "" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	repo.LockAccountCredentials("cid")()
}

// TestDeliveryNotifierWithoutNotifier 验证未注入通知器时终态通知安全跳过。
func TestDeliveryNotifierWithoutNotifier(t *testing.T) {
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// center 未注入通知器，终态通知应只记录日志。
	center := New(store, nil, nil)
	center.notifyResult(context.Background(), Task{AccountID: "cid"}, 1, "success", 1, "")
}

// TestSendCardGuards 覆盖发卡动作缺少卡组、卡组读取失败与 API 客户端缺失的前置保护。
func TestSendCardGuards(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// center 未注入 API 卡取回器。
	center := New(store, nil, nil)
	if _, err := center.actions.sendCardWithProof(ctx, Task{}, db.AutomationAction{ActionType: ActionSendCard, Enabled: true, ConfigJSON: `{}`}); err == nil {
		t.Fatal("缺少卡组ID应报错")
	}
	if _, err := center.actions.sendCardWithProof(ctx, Task{}, db.AutomationAction{ActionType: ActionSendCard, CardID: 424242, Enabled: true, ConfigJSON: `{}`}); err == nil {
		t.Fatal("卡组不存在应报错")
	}
	if _, err := center.actions.sendAPICardWithProof(ctx, Task{}, db.AutomationAction{}, &db.CardFull{}, 1); err == nil {
		t.Fatal("API 客户端缺失应报错")
	}
}
