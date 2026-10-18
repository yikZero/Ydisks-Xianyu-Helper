package adapter

import (
	"context"
	"errors"
	"testing"
	"time"

	accountmanager "xianyu-go/internal/account"
	chatapp "xianyu-go/internal/application/chat"
	"xianyu-go/internal/automation"
	domainchat "xianyu-go/internal/chat"
	"xianyu-go/internal/engine"
	"xianyu-go/internal/xianyu/mtop"
)

// refreshRuntimeSender 同时提供联系人与历史分页能力，用于驱动平台刷新适配器。
type refreshRuntimeSender struct {
	// MessageSender 占位自动化发送能力，刷新测试不会调用。
	automation.MessageSender
	// body 是平台分页响应原文。
	body map[string]any
	// myID 是平台返回的当前账号身份。
	myID string
	// err 是平台请求错误。
	err error
}

// FetchChatConversations 返回预设联系人页。
func (s refreshRuntimeSender) FetchChatConversations(context.Context, int64, int) (map[string]any, string, error) {
	return s.body, s.myID, s.err
}

// FetchChatHistory 返回预设历史页。
func (s refreshRuntimeSender) FetchChatHistory(context.Context, string, int64, int) (map[string]any, string, error) {
	return s.body, s.myID, s.err
}

// TestChatRefreshProviderBranches 覆盖联系人与历史刷新的离线、不支持、请求失败和成功路径。
func TestChatRefreshProviderBranches(t *testing.T) {
	ctx := context.Background()
	if NewChatRefreshProvider(nil, nil) != nil {
		t.Fatal("缺依赖应返回 nil")
	}
	if _, err := (chatRefreshProvider{}).RefreshConversations(ctx, "cid", 0, 10); !errors.Is(err, chatapp.ErrRefreshUnavailable) {
		t.Fatalf("空提供器会话: %v", err)
	}
	if _, err := (chatRefreshProvider{}).RefreshHistory(ctx, "cid", "c", 0, 10, chatapp.Session{}); !errors.Is(err, chatapp.ErrRefreshUnavailable) {
		t.Fatalf("空提供器历史: %v", err)
	}
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// sender 是当前查找返回的运行时替身，可由各用例替换。
	var sender automation.MessageSender
	// provider 通过闭包查找运行时。
	provider := chatRefreshProvider{service: domainchat.New(store), lookup: func(string) (automation.MessageSender, bool) { return sender, sender != nil }}
	if _, err := provider.RefreshConversations(ctx, "cid", 0, 10); !errors.Is(err, chatapp.ErrOffline) {
		t.Fatalf("离线会话: %v", err)
	}
	if _, err := provider.RefreshHistory(ctx, "cid", "c", 0, 10, chatapp.Session{}); !errors.Is(err, chatapp.ErrOffline) {
		t.Fatalf("离线历史: %v", err)
	}
	// 不支持分页能力的运行时。
	sender = &coverageAutomationSender{}
	if _, err := provider.RefreshConversations(ctx, "cid", 0, 10); err == nil {
		t.Fatal("不支持会话刷新应失败")
	}
	if _, err := provider.RefreshHistory(ctx, "cid", "c", 0, 10, chatapp.Session{}); err == nil {
		t.Fatal("不支持历史刷新应失败")
	}
	// 平台请求失败。
	sender = refreshRuntimeSender{err: errors.New("platform down")}
	if _, err := provider.RefreshConversations(ctx, "cid", 0, 10); err == nil {
		t.Fatal("平台失败应返回")
	}
	if _, err := provider.RefreshHistory(ctx, "cid", "c", 0, 10, chatapp.Session{}); err == nil {
		t.Fatal("平台失败应返回")
	}
	// 成功路径：空分页也应被正常落库。
	sender = refreshRuntimeSender{body: map[string]any{"hasMore": true, "nextCursor": float64(5)}, myID: "me"}
	// page、err 是联系人刷新结果。
	page, err := provider.RefreshConversations(ctx, "cid", 0, 10)
	if err != nil || !page.HasMore || page.NextCursor != 5 {
		t.Fatalf("联系人成功: %+v %v", page, err)
	}
	// history、historyErr 是历史刷新结果。
	history, historyErr := provider.RefreshHistory(ctx, "cid", "c", 0, 10, chatapp.Session{AccountID: "cid", ChatID: "c"})
	if historyErr != nil || history.Session.ChatID != "c" {
		t.Fatalf("历史成功: %+v %v", history, historyErr)
	}
	// 数据库关闭后落库失败应映射为刷新落库错误。
	cleanup()
	if _, err := provider.RefreshConversations(ctx, "cid", 0, 10); err != nil && !errors.Is(err, chatapp.ErrRefreshPersist) {
		t.Fatalf("落库错误映射: %v", err)
	}
}

// TestChatSubscriptionProviderForwardsAndStops 覆盖订阅适配器的参数保护、事件转换转发与幂等停止。
func TestChatSubscriptionProviderForwardsAndStops(t *testing.T) {
	ctx := context.Background()
	if NewChatSubscriptionProvider(nil) != nil {
		t.Fatal("空服务应返回 nil")
	}
	if _, _, err := (chatSubscriptionProvider{}).Subscribe(ctx, 1); !errors.Is(err, chatapp.ErrSubscriptionUnavailable) {
		t.Fatalf("空服务订阅: %v", err)
	}
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// service 是真实聊天领域服务。
	service := domainchat.New(store)
	// provider 是被测订阅适配器。
	provider := NewChatSubscriptionProvider(service)
	if _, _, err := provider.Subscribe(ctx, 0); !errors.Is(err, chatapp.ErrSubscriptionUnavailable) {
		t.Fatalf("非法用户: %v", err)
	}
	// owner 是测试账号所属用户。
	owner, err := store.Users.GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	// events、stop 是订阅通道与停止函数。
	events, stop, err := provider.Subscribe(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	// 入站消息会作为带会话摘要的应用事件被转发。
	if _, _, err := service.RecordIncoming(ctx, domainchat.Incoming{AccountID: "cid", ChatID: "chat-sub", BuyerID: "b", MessageID: "m1", Text: "hi", ObservedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	select {
	// event 测试中的局部对象，承载被测流程的输入、依赖或输出。
	case event := <-events:
		if event.Type == "" {
			t.Fatalf("事件类型为空: %+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到转发事件")
	}
	stop()
	stop()
	// 父上下文取消同样会让转发协程退出并关闭输出通道。
	cancelCtx, cancel := context.WithCancel(ctx)
	// err、events2、stop2 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；测试中的局部对象，承载被测流程的输入、依赖或输出；测试中的局部对象，承载被测流程的输入、依赖或输出。
	events2, stop2, err := provider.Subscribe(cancelCtx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	for range events2 {
	}
	stop2()
	if chatApplicationSession(nil).AccountID != "" {
		t.Fatal("空会话应为零值")
	}
}

// TestAutomationImageSenderGapPaths 覆盖图片发送器的空发送器、成功文本、空图片依赖与来源提供者成功路径。
func TestAutomationImageSenderGapPaths(t *testing.T) {
	ctx := context.Background()
	// runtimeSender 记录文本发送。
	runtimeSender := &coverageAutomationSender{}
	if err := (automationImageSender{sender: runtimeSender}).SendText(engine.WithOutgoingMessageKey(ctx, "k"), "chat", "buyer", "文本"); err != nil || runtimeSender.text != "文本" {
		t.Fatalf("文本发送: %v %q", err, runtimeSender.text)
	}
	if err := (automationImageSender{}).SendImage(ctx, "chat", "buyer", "u", 1, 0, 0); !errors.Is(err, automation.ErrMessageNotSent) {
		t.Fatalf("图片依赖缺失: %v", err)
	}
	(automationImageSender{}).UpdateCookie("ignored")
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// manager 登记一个在线账号。
	manager := accountmanager.NewManager(store, &Adapter{}, nil)
	// lifecycle、cancel 是账号运行时上下文。
	lifecycle, cancel := context.WithCancel(ctx)
	defer cancel()
	// provider 绑定管理器与上传器。
	provider := NewAutomationImageSenderProvider(store, manager, func() mtop.Client { return nil })
	// ok 保存查询结果是否存在或操作是否成功的布尔标记，用于断言。
	if _, ok := provider.Sender("cid"); ok {
		t.Fatal("账号未启动不应有发送器")
	}
	if err := manager.Start(lifecycle, "cid", "unb=1; _m_h5_tk=tk;"); err != nil {
		t.Fatal(err)
	}
	// ok、sender 依次为：保存查询结果是否存在或操作是否成功的布尔标记，用于断言；测试中的局部对象，承载被测流程的输入、依赖或输出。
	if sender, ok := provider.Sender("cid"); !ok || sender == nil {
		t.Fatalf("已启动账号应有发送器: %v %v", sender, ok)
	}
	manager.Stop("cid")
}

// TestDownloadAutomationImageInvalidInputs 覆盖图片下载对非法地址与不可达地址的拒绝。
func TestDownloadAutomationImageInvalidInputs(t *testing.T) {
	// urls 是各类应被拒绝的图片来源。
	urls := []string{"", "ftp://example.com/a.png", "http://user:pw@example.com/a.png", "http:///a.png", "http://a\x7f.com/a.png"}
	// rawURL 是当前被验证的来源地址。
	for _, rawURL := range urls {
		if _, _, _, err := downloadAutomationImage(context.Background(), rawURL); err == nil {
			t.Fatalf("应拒绝 %q", rawURL)
		}
	}
}
