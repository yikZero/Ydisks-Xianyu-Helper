package automation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"xianyu-go/internal/db"
)

// offlineSenders 模拟账号发送器不在线的提供方：任何账号都查不到发送器。
type offlineSenders struct{}

// Sender 总是报告账号离线。
func (offlineSenders) Sender(string) (MessageSender, bool) { return nil, false }

// newConsignGapCenter 创建注入指定平台替身与可选恢复器的中心，返回中心、数据库与恢复替身。
func newConsignGapCenter(t *testing.T, client *fakeMTop, withRecoverer bool, recoverFail bool) (*Center, *db.Store, *fakeCredentialRecoverer) {
	t.Helper()
	store, cleanup := newAutomationTestStore(t)
	t.Cleanup(cleanup)
	// deps 是待注入的依赖。
	deps := CenterDependencies{MTop: client}
	// recoverer 是可选的凭证恢复替身。
	var recoverer *fakeCredentialRecoverer
	if withRecoverer {
		recoverer = &fakeCredentialRecoverer{store: store, fail: recoverFail}
		deps.OrderDetailFetcher = recoverer
	}
	return NewWithDependencies(store, testSenderProvider{sender: &testSender{}}, nil, deps), store, recoverer
}

// TestConfirmShipmentAttemptGapBranches 覆盖确认发货（普通接口）的会话、凭证恢复与能力降级分支；全程不得调用免拼接口。
func TestConfirmShipmentAttemptGapBranches(t *testing.T) {
	ctx := context.Background()
	// task 是账号 cid 的确认发货任务。
	task := Task{AccountID: "cid", OrderID: "consign-gap", ItemID: "i", BuyerID: "b", ChatID: "c"}
	// expired 是平台明确的会话失效结果。
	expired := fakeConsignResult{ret: []string{"FAIL_SYS_SESSION_EXPIRED::x"}}

	t.Run("账号没有凭证", func(t *testing.T) {
		// center 保存中心。
		center, _, _ := newConsignGapCenter(t, &fakeMTop{consignOk: true}, false, false)
		// ghost 是不存在的账号。
		ghost := task
		ghost.AccountID = "ghost"
		// attemptErr 是单次平台调用尝试（含凭证恢复与响应凭证保存）的最终错误，用于断言分类与是否禁止自动重试。
		if attemptErr := center.actions.confirmShipmentAttempt(ctx, ghost, shipmentDeliveryProof{}, true); attemptErr == nil {
			t.Fatal("无凭证应报错")
		}
	})
	t.Run("账号Cookie为空", func(t *testing.T) {
		// center、store 保存中心与数据库。
		center, store, _ := newConsignGapCenter(t, &fakeMTop{consignOk: true}, false, false)
		execGapSQL(t, store, `UPDATE cookies SET value='' WHERE id='cid'`)
		// attemptErr 是单次平台调用尝试（含凭证恢复与响应凭证保存）的最终错误，用于断言分类与是否禁止自动重试。
		if attemptErr := center.actions.confirmShipmentAttempt(ctx, task, shipmentDeliveryProof{}, true); attemptErr == nil || !strings.Contains(attemptErr.Error(), "Cookie 为空") {
			t.Fatalf("err=%v", attemptErr)
		}
	})
	t.Run("旧客户端回退普通确认接口且不调用免拼", func(t *testing.T) {
		// inner 记录调用次数。
		inner := &fakeMTop{consignOk: true, consignRet: []string{"SUCCESS::ok"}}
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		// center 的客户端只暴露接口方法，不带凭证能力。
		center := NewWithDependencies(store, nil, nil, CenterDependencies{MTop: plainMTop{Client: inner}})
		// attemptErr 是单次平台调用尝试（含凭证恢复与响应凭证保存）的最终错误，用于断言分类与是否禁止自动重试。
		if attemptErr := center.actions.confirmShipmentAttempt(ctx, task, shipmentDeliveryProof{}, true); attemptErr != nil {
			t.Fatal(attemptErr)
		}
		if inner.freeShippingCalls != 0 {
			t.Fatal("确认发货不得调用免拼接口")
		}
	})
	t.Run("会话失效且响应凭证保存失败", func(t *testing.T) {
		// center、store 保存中心与数据库。
		center, store, _ := newConsignGapCenter(t, &fakeMTop{consignResults: []fakeConsignResult{{ret: expired.ret, updated: "unb=123; _m_h5_tk=zz;"}}}, false, false)
		execGapSQL(t, store, `CREATE TRIGGER gap_abort_cookie BEFORE UPDATE ON cookies BEGIN SELECT RAISE(ABORT, 'forced cookie failure'); END`)
		// attemptErr 是单次平台调用尝试（含凭证恢复与响应凭证保存）的最终错误，用于断言分类与是否禁止自动重试。
		if attemptErr := center.actions.confirmShipmentAttempt(ctx, task, shipmentDeliveryProof{}, true); attemptErr == nil || !strings.Contains(attemptErr.Error(), "forced cookie failure") {
			t.Fatalf("err=%v", attemptErr)
		}
	})
	t.Run("恢复后仍失效不再重试", func(t *testing.T) {
		// client 两次都返回会话失效。
		client := &fakeMTop{consignResults: []fakeConsignResult{expired, expired}}
		// center、recoverer 保存中心与恢复替身。
		center, _, recoverer := newConsignGapCenter(t, client, true, false)
		// attemptErr 是单次平台调用尝试（含凭证恢复与响应凭证保存）的最终错误，用于断言分类与是否禁止自动重试。
		if attemptErr := center.actions.confirmShipmentAttempt(ctx, task, shipmentDeliveryProof{}, true); !errors.Is(attemptErr, errActionNotPerformed) || !strings.Contains(attemptErr.Error(), "恢复后仍返回") {
			t.Fatalf("err=%v", attemptErr)
		}
		if recoverer.calls != 1 || client.freeShippingCalls != 0 {
			t.Fatalf("recover=%d free=%d", recoverer.calls, client.freeShippingCalls)
		}
	})
}

// TestPersistConfirmedShipmentGapBranches 覆盖远端已确认发货后本地事实与补偿记录的写入失败分支。
func TestPersistConfirmedShipmentGapBranches(t *testing.T) {
	ctx := context.Background()
	// task 是已被平台确认发货的订单任务。
	task := Task{AccountID: "cid", OrderID: "persist-gap", ItemID: "i", BuyerID: "b", ChatID: "c"}
	// shippedAbort 让发货时间写入失败。
	const shippedAbort = `CREATE TRIGGER gap_abort_shipped BEFORE UPDATE OF shipped_at ON orders BEGIN SELECT RAISE(ABORT, 'forced shipped_at failure'); END`

	t.Run("发货时间写入失败并创建补偿记录", func(t *testing.T) {
		// center、store 保存中心与数据库。
		center, store, _ := newConsignGapCenter(t, &fakeMTop{consignOk: true}, false, false)
		execGapSQL(t, store, shippedAbort)
		// persistErrs 是确认发货后本地订单事实与补偿记录写入产生的错误列表，用于断言补偿链路。
		if persistErrs := center.actions.persistConfirmedShipment(ctx, task); len(persistErrs) < 2 {
			t.Fatalf("应包含写入失败与补偿记录: %v", persistErrs)
		}
	})
	t.Run("补偿存储未初始化", func(t *testing.T) {
		// center、store 保存中心与数据库。
		center, store, _ := newConsignGapCenter(t, &fakeMTop{consignOk: true}, false, false)
		execGapSQL(t, store, shippedAbort)
		store.Reconciliations = nil
		// persistErrs 是确认发货后本地订单事实与补偿记录写入产生的错误列表，用于断言补偿链路。
		if persistErrs := center.actions.persistConfirmedShipment(ctx, task); len(persistErrs) == 0 || !strings.Contains(persistErrs[len(persistErrs)-1].Error(), "补偿存储未初始化") {
			t.Fatalf("persistErrs=%v", persistErrs)
		}
	})
	t.Run("补偿记录创建失败", func(t *testing.T) {
		// center、store 保存中心与数据库。
		center, store, _ := newConsignGapCenter(t, &fakeMTop{consignOk: true}, false, false)
		execGapSQL(t, store, shippedAbort)
		execGapSQL(t, store, `DROP TABLE order_reconciliations`)
		// persistErrs 是确认发货后本地订单事实与补偿记录写入产生的错误列表，用于断言补偿链路。
		if persistErrs := center.actions.persistConfirmedShipment(ctx, task); len(persistErrs) == 0 || !strings.Contains(persistErrs[len(persistErrs)-1].Error(), "创建订单发货补偿记录") {
			t.Fatalf("persistErrs=%v", persistErrs)
		}
	})
}

// TestActionExecutorTextGapBranches 覆盖文本动作渲染为空和账号离线两种前置结果。
func TestActionExecutorTextGapBranches(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newAutomationTestStore(t)
	defer cleanup()
	// center 的发送器提供方报告账号离线。
	center := NewWithDependencies(store, offlineSenders{}, nil, CenterDependencies{})
	// task 是完整会话信息的任务。
	task := Task{AccountID: "cid", OrderID: "o", ChatID: "c", BuyerID: "b"}
	// execErr 是 executeRule 对本场景的执行结果，用于断言运行是否进入人工核对或被跳过；result 是发卡动作返回的已发送数量与发货凭证。
	if result, execErr := center.actions.executeActionWithProof(ctx, task, db.AutomationAction{ActionType: ActionSendText, MessageTemplate: "  "}, shipmentDeliveryProof{}); execErr != nil || result.sent != 0 {
		t.Fatalf("空文本应直接跳过: %+v %v", result, execErr)
	}
	// sendErr 是向买家发送消息或卡密失败时返回的错误，用于断言是否保留待核对快照。
	if sendErr := center.actions.sendText(ctx, task, "你好"); !errors.Is(sendErr, ErrMessageNotSent) {
		t.Fatalf("离线应为确定未发送: %v", sendErr)
	}
}
