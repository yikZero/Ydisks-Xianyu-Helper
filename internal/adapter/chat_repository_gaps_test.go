package adapter

import (
	"context"
	"errors"
	"testing"

	accountmanager "xianyu-go/internal/account"
	chatapp "xianyu-go/internal/application/chat"
	"xianyu-go/internal/xianyu/mtop"
)

// scriptedIdentityClient 返回预置的身份结果，并可在请求期间改写凭证模拟并发更新。
type scriptedIdentityClient struct {
	// Client 占位其余 MTOP 能力。
	mtop.Client
	// info、err 是身份查询的预置结果与错误。
	info *mtop.ChatUserInfo
	err  error
	// hook 在请求内部执行，用于制造凭证并发变化。
	hook func()
}

// FetchChatUserInfo 先执行钩子再返回预置身份结果。
func (c scriptedIdentityClient) FetchChatUserInfo(context.Context, string, string) (*mtop.ChatUserInfo, error) {
	if c.hook != nil {
		c.hook()
	}
	return c.info, c.err
}

// TestChatIdentityResolverBranches 覆盖身份解析的依赖缺失、凭证缺失、平台错误、空结果与运行时同步路径。
func TestChatIdentityResolverBranches(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if NewChatIdentityResolver(nil, nil, nil) != nil {
		t.Fatal("缺依赖应返回 nil")
	}
	if _, err := (chatIdentityResolver{}).Resolve(ctx, "cid", "chat"); !errors.Is(err, chatapp.ErrUnavailable) {
		t.Fatalf("空存储: %v", err)
	}
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// resolve 用给定客户端与管理器发起一次身份查询。
	resolve := func(client mtop.Client, manager *accountmanager.Manager, account string) (chatapp.Identity, error) {
		return NewChatIdentityResolver(store, func() mtop.Client { return client }, manager).Resolve(ctx, account, "chat")
	}
	if _, err := resolve(scriptedIdentityClient{}, nil, "missing"); err == nil {
		t.Fatal("账号缺失应失败")
	}
	if _, err := resolve(&coverageChatItemClient{}, nil, "cid"); err == nil {
		t.Fatal("不支持身份查询应失败")
	}
	if _, err := resolve(scriptedIdentityClient{err: errors.New("platform")}, nil, "cid"); err == nil {
		t.Fatal("平台错误应返回")
	}
	// err、identity 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存解析得到的对端展示身份，用于断言。
	if identity, err := resolve(scriptedIdentityClient{}, nil, "cid"); err != nil || identity != (chatapp.Identity{}) {
		t.Fatalf("空结果: %+v %v", identity, err)
	}
	// 响应带新 Cookie 时写回数据库，并同步到在线运行时（含未运行与已运行两种情况）。
	manager := accountmanager.NewManager(store, &Adapter{}, nil)
	// withCookie 测试中的局部对象，承载被测流程的输入、依赖或输出。
	withCookie := scriptedIdentityClient{info: &mtop.ChatUserInfo{Nickname: "买家", AvatarURL: "a", UpdatedCookies: "unb=1; _m_h5_tk=r1"}}
	// err、identity 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存解析得到的对端展示身份，用于断言。
	if identity, err := resolve(withCookie, manager, "cid"); err != nil || identity.PeerName != "买家" {
		t.Fatalf("未运行同步: %+v %v", identity, err)
	}
	if err := manager.Start(ctx, "cid", "unb=1; _m_h5_tk=r1"); err != nil {
		t.Fatal(err)
	}
	withCookie.info = &mtop.ChatUserInfo{Nickname: "买家", UpdatedCookies: "unb=1; _m_h5_tk=r2"}
	if _, err := resolve(withCookie, manager, "cid"); err != nil {
		t.Fatalf("已运行同步: %v", err)
	}
	manager.Stop("cid")
	// 写回失败：请求内关闭数据库，既有平台错误又有写回错误时合并返回。
	failing := scriptedIdentityClient{err: errors.New("platform"), info: &mtop.ChatUserInfo{UpdatedCookies: "unb=1; z=1"}, hook: func() { _ = store.DB.Close() }}
	if _, err := resolve(failing, nil, "cid"); err == nil {
		t.Fatal("合并错误应返回")
	}
}

// TestChatIdentityResolverPersistErrorWithoutFetchError 覆盖平台成功但响应凭证写回失败时返回写回错误。
func TestChatIdentityResolverPersistErrorWithoutFetchError(t *testing.T) {
	ctx := context.Background()
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// client 在请求期间关闭数据库，使写回阶段失败。
	client := scriptedIdentityClient{info: &mtop.ChatUserInfo{UpdatedCookies: "unb=1; z=2"}, hook: func() { _ = store.DB.Close() }}
	if _, err := NewChatIdentityResolver(store, func() mtop.Client { return client }, nil).Resolve(ctx, "cid", "chat"); err == nil {
		t.Fatal("写回错误应返回")
	}
	// 凭证为空时拒绝查询。
	store2, cleanup2 := newAdapterTestStore(t)
	defer cleanup2()
	if err := store2.Cookies.UpdateValueExisting(ctx, "cid", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := NewChatIdentityResolver(store2, func() mtop.Client { return scriptedIdentityClient{} }, nil).Resolve(ctx, "cid", "chat"); err == nil {
		t.Fatal("空凭证应拒绝")
	}
}

// TestChatRepositoryEdgeBranches 覆盖聊天仓储的诊断存储缺失、数据库故障与快捷回复上限映射。
func TestChatRepositoryEdgeBranches(t *testing.T) {
	ctx := context.Background()
	if _, err := (chatRepository{}).FindInboundParsedJSONContaining(ctx, "cid", "x", 1); err == nil {
		t.Fatal("缺诊断存储应失败")
	}
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// repository 是完整装配的聊天仓储。
	repository := chatRepository{store: store}
	// created 记录成功创建的快捷回复数，用于确认上限映射确实触发。
	created := 0
	for created < 60 {
		if _, err := repository.CreateQuickReply(ctx, "cid", "回复"); err != nil {
			if !errors.Is(err, chatapp.ErrQuickReplyLimitReached) {
				t.Fatalf("上限错误映射: %v", err)
			}
			break
		}
		created++
	}
	if created == 0 || created >= 60 {
		t.Fatalf("未触发上限: %d", created)
	}
	cleanup()
	if _, err := repository.FindSession(ctx, 1, "cid", "chat"); err == nil {
		t.Fatal("数据库故障应传播")
	}
	if _, err := repository.CreateQuickReply(ctx, "cid", "x"); err == nil {
		t.Fatal("数据库故障应传播")
	}
}
