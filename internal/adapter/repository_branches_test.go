package adapter

import (
	"context"
	"errors"
	"testing"

	accountapp "xianyu-go/internal/application/account"
	"xianyu-go/internal/db"
)

// TestAuthenticationRepositoryVerifiedSessionLifecycle 覆盖带认证代次的登录校验、会话签发、会话校验及其失败映射。
func TestAuthenticationRepositoryVerifiedSessionLifecycle(t *testing.T) {
	// store、cleanup 是夹具数据库及清理函数。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// repository 是绑定夹具库的认证适配器。
	repository := NewAuthenticationRepository(store)
	ctx := context.Background()
	// user、matched、verifyErr 是正确密码的校验结果。
	user, matched, verifyErr := repository.VerifyLogin(ctx, "admin", "pw")
	if verifyErr != nil || !matched || user.ID == 0 {
		t.Fatalf("verify login user=%+v matched=%v err=%v", user, matched, verifyErr)
	}
	// _, wrongMatched、wrongErr 是错误密码映射结果。
	_, wrongMatched, wrongErr := repository.VerifyLogin(ctx, "admin", "bad")
	if wrongMatched || !errors.Is(wrongErr, accountapp.ErrPasswordMismatch) {
		t.Fatalf("wrong password matched=%v err=%v", wrongMatched, wrongErr)
	}
	// _, unknownMatched、unknownErr 是不存在用户的结果，不应被当作成功。
	_, unknownMatched, _ := repository.VerifyLogin(ctx, "nobody", "pw")
	if unknownMatched {
		t.Fatal("unknown user must not match")
	}
	// sessionID、sessionErr 是以当前认证代次签发的会话。
	sessionID, sessionErr := repository.CreateVerifiedSession(ctx, user)
	if sessionErr != nil || sessionID == "" {
		t.Fatalf("create verified session id=%q err=%v", sessionID, sessionErr)
	}
	// validateErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if validateErr := repository.ValidateSession(ctx, sessionID, user.ID); validateErr != nil {
		t.Fatalf("validate session: %v", validateErr)
	}
	// validateErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if validateErr := repository.ValidateSession(ctx, sessionID, user.ID+99); !errors.Is(validateErr, accountapp.ErrSessionInvalid) {
		t.Fatalf("foreign user err=%v", validateErr)
	}
	// validateErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if validateErr := repository.ValidateSession(ctx, "missing-session", user.ID); !errors.Is(validateErr, accountapp.ErrSessionInvalid) {
		t.Fatalf("missing session err=%v", validateErr)
	}
	// stale 使用过期的认证代次，模拟密码在校验与签发之间被修改。
	stale := user
	stale.AuthVersion += 5
	// staleErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if _, staleErr := repository.CreateVerifiedSession(ctx, stale); !errors.Is(staleErr, accountapp.ErrPasswordMismatch) {
		t.Fatalf("stale login err=%v", staleErr)
	}
	// uninitialized 是未装配存储的认证适配器。
	uninitialized := &AuthenticationRepository{}
	// createErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if _, createErr := uninitialized.CreateVerifiedSession(ctx, user); createErr == nil {
		t.Fatal("uninitialized create should fail")
	}
	// validateErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if validateErr := uninitialized.ValidateSession(ctx, "s", 1); validateErr == nil {
		t.Fatal("uninitialized validate should fail")
	}
	// loginErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if _, _, loginErr := uninitialized.VerifyLogin(ctx, "admin", "pw"); loginErr == nil {
		t.Fatal("uninitialized login should fail")
	}
}

// TestOrderRepositoryOwnerAndFind 覆盖订单仓储的账号所有者查询与订单存在性转换。
func TestOrderRepositoryOwnerAndFind(t *testing.T) {
	// store、cleanup 是夹具数据库及清理函数。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// repository 是订单仓储适配器。
	repository := NewOrderRepository(store)
	ctx := context.Background()
	// ownerID、ownerErr 是夹具账号 cid 的所属用户。
	ownerID, ownerErr := repository.GetOwnerID(ctx, "cid")
	if ownerErr != nil || ownerID == 0 {
		t.Fatalf("owner id=%d err=%v", ownerID, ownerErr)
	}
	// missing、found、findErr 是不存在订单的查询结果。
	missing, found, findErr := repository.FindOrder(ctx, "missing-order")
	if findErr != nil || found || missing != nil {
		t.Fatalf("missing order=%v found=%v err=%v", missing, found, findErr)
	}
	// upsertErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if upsertErr := store.Orders.Upsert(ctx, "order-1", db.OrderUpsertOpts{CookieID: "cid", ItemID: "item-1", BuyerID: "b", OrderStatus: "paid"}); upsertErr != nil {
		t.Fatal(upsertErr)
	}
	// order、exists、existsErr 是已存在订单的查询结果。
	order, exists, existsErr := repository.FindOrder(ctx, "order-1")
	if existsErr != nil || !exists || order == nil {
		t.Fatalf("existing order=%v exists=%v err=%v", order, exists, existsErr)
	}
}

// TestAdapterChatSessionRoleGuards 覆盖聊天服务缺失时的角色读取默认值与写入错误。
func TestAdapterChatSessionRoleGuards(t *testing.T) {
	ctx := context.Background()
	// nilAdapter 同时覆盖 nil 接收者与零值适配器。
	for _, target := range []*Adapter{nil, {}} {
		// session、roleErr 是缺少聊天服务时返回的未知角色会话。
		session, roleErr := target.ChatSessionRole(ctx, "a", "c", "i")
		if roleErr != nil || session.AccountRole != "unknown" || session.CookieID != "a" {
			t.Fatalf("session=%+v err=%v", session, roleErr)
		}
		// saveErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
		if saveErr := target.SaveChatSessionRole(ctx, "a", "c", "i", "seller", "b", "s", "local"); saveErr == nil {
			t.Fatal("缺少聊天服务时保存角色应失败")
		}
	}
}
