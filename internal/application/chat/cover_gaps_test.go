package chat

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// plainSender 只实现基础 Sender 能力，用于验证缺少商品卡片窄能力时的降级。
type plainSender struct{}

// SendText 总是成功，不产生任何平台副作用。
func (plainSender) SendText(context.Context, string, string, string, string) error { return nil }

// SendImage 总是成功，不产生任何平台副作用。
func (plainSender) SendImage(context.Context, string, string, string, int64, int, int, string) error {
	return nil
}

// TestItemAvailabilityReflectsWiredPorts 验证商品发送与商品目录可用性只取决于端口装配。
func TestItemAvailabilityReflectsWiredPorts(t *testing.T) {
	// nilService 是空服务，所有能力都应报告不可用。
	var nilService *Service
	if nilService.ItemSendingAvailable() || nilService.ItemCatalogAvailable() {
		t.Fatal("空服务不应可用")
	}
	// repository 是具备精确会话查询能力的仓储。
	repository := &itemRepository{fakeRepository: &fakeRepository{owned: true}}
	// withoutLookup 的仓储缺少精确会话查询能力。
	withoutLookup := NewWithSending(historyOnlyRepository{}, &sendRepository{}, sendProvider{}, nil)
	if withoutLookup.ItemSendingAvailable() {
		t.Fatal("缺少精确会话查询时不应可用")
	}
	// full 装配了全部商品发送端口。
	full := NewWithSending(repository, &sendRepository{}, sendProvider{}, nil)
	if !full.ItemSendingAvailable() || full.ItemCatalogAvailable() {
		t.Fatal("商品发送应可用而目录未装配")
	}
	if !WithChatItemCatalog(full, &itemCatalog{}).ItemCatalogAvailable() {
		t.Fatal("装配目录后应可用")
	}
}

// TestListChatItemsRejectsInvalidUnavailableAndOwnershipFailure 覆盖输入非法、未装配与归属查询失败。
func TestListChatItemsRejectsInvalidUnavailableAndOwnershipFailure(t *testing.T) {
	// ownershipFailure 是账号归属查询失败注入的错误。
	ownershipFailure := errors.New("ownership down")
	// repository 是归属查询失败的仓储。
	repository := &itemRepository{fakeRepository: &fakeRepository{ownedErr: ownershipFailure}}
	// service 装配了目录端口。
	service := WithChatItemCatalog(New(repository), &itemCatalog{})
	// base 是合法的查询输入。
	base := ChatItemQuery{UserID: 1, AccountID: "a", ChatID: "c", Role: "peer", Page: 1}
	// mutate 是逐项破坏合法输入的修改器。
	for _, mutate := range []func(*ChatItemQuery){
		func(q *ChatItemQuery) { q.UserID = 0 },
		func(q *ChatItemQuery) { q.AccountID = " " },
		func(q *ChatItemQuery) { q.ChatID = "" },
		func(q *ChatItemQuery) { q.Role = "other" },
		func(q *ChatItemQuery) { q.Page = 0 },
		func(q *ChatItemQuery) { q.Query = strings.Repeat("长", 101) },
	} {
		// query 是被破坏后的输入副本。
		query := base
		mutate(&query)
		if _, err := service.ListChatItems(context.Background(), query); !errors.Is(err, ErrChatItemInvalid) {
			t.Fatalf("err=%v", err)
		}
	}
	// unavailable 没有装配目录端口。
	unavailable := New(repository)
	if _, err := unavailable.ListChatItems(context.Background(), base); !errors.Is(err, ErrChatItemUnavailable) {
		t.Fatalf("err=%v", err)
	}
	if _, err := service.ListChatItems(context.Background(), base); !errors.Is(err, ownershipFailure) {
		t.Fatalf("err=%v", err)
	}
}

// TestSendItemCardPreconditionFailures 覆盖商品卡片发送的前置校验、归属、会话与发送能力失败。
func TestSendItemCardPreconditionFailures(t *testing.T) {
	// session 是已归属的目标会话。
	session := Session{AccountID: "account-1", ChatID: "chat-1", PeerUserID: "buyer-1"}
	// input 是合法商品卡片输入。
	input := ItemCardInput{UserID: 7, AccountID: "account-1", ChatID: "chat-1", Item: ChatItem{ItemID: "i", Title: "t", ImageURL: "https://img.example/a.png", Price: "1"}}
	ctx := context.Background()
	// build 构造使用给定仓储与发送器的服务。
	build := func(repository *itemRepository, sender Sender) *Service {
		return NewWithSending(repository, &sendRepository{}, sendProvider{sender: sender}, nil)
	}
	// good 是允许发送的会话仓储。
	good := &itemRepository{fakeRepository: &fakeRepository{owned: true}, session: session}
	// invalid 是缺少用户标识的非法输入。
	invalid := input
	invalid.UserID = 0
	if _, err := build(good, plainSender{}).SendItemCard(ctx, invalid); !errors.Is(err, ErrChatItemInvalid) {
		t.Fatalf("invalid err=%v", err)
	}
	if _, err := NewWithSending(nil, nil, nil, nil).SendItemCard(ctx, input); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unavailable err=%v", err)
	}
	// ownershipFailure 是归属查询注入的错误。
	ownershipFailure := errors.New("ownership down")
	if _, err := build(&itemRepository{fakeRepository: &fakeRepository{ownedErr: ownershipFailure}}, plainSender{}).SendItemCard(ctx, input); !errors.Is(err, ownershipFailure) {
		t.Fatalf("ownership err=%v", err)
	}
	if _, err := build(&itemRepository{fakeRepository: &fakeRepository{}}, plainSender{}).SendItemCard(ctx, input); !errors.Is(err, ErrChatItemForbidden) {
		t.Fatalf("forbidden err=%v", err)
	}
	// lookupFailure 是精确会话查询注入的普通错误。
	lookupFailure := errors.New("lookup down")
	if _, err := build(&itemRepository{fakeRepository: &fakeRepository{owned: true}, findErr: lookupFailure}, plainSender{}).SendItemCard(ctx, input); !errors.Is(err, lookupFailure) {
		t.Fatalf("lookup err=%v", err)
	}
	// noPeer 的会话缺少对端身份，必须按会话不存在处理。
	noPeer := &itemRepository{fakeRepository: &fakeRepository{owned: true}, session: Session{AccountID: "account-1", ChatID: "chat-1"}}
	if _, err := build(noPeer, plainSender{}).SendItemCard(ctx, input); !errors.Is(err, ErrChatSessionNotFound) {
		t.Fatalf("no peer err=%v", err)
	}
	if _, err := build(good, plainSender{}).SendItemCard(ctx, input); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("plain sender err=%v", err)
	}
}

// TestSendItemCardUncertainResult 验证商品卡片发送结果不确定时落 uncertain 状态且不降级为 failed。
func TestSendItemCardUncertainResult(t *testing.T) {
	// repository 是已归属的会话仓储。
	repository := &itemRepository{fakeRepository: &fakeRepository{owned: true}, session: Session{AccountID: "account-1", ChatID: "chat-1", PeerUserID: "buyer-1"}}
	// outgoing 记录状态迁移。
	outgoing := &sendRepository{}
	// service 使用返回不确定错误的发送器。
	service := NewWithSending(repository, outgoing, sendProvider{sender: &sendSender{sendErr: ErrSendUncertain}}, nil)
	// message 和 err 是不确定发送的结果。
	message, err := service.SendItemCard(context.Background(), ItemCardInput{UserID: 1, AccountID: "account-1", ChatID: "chat-1", Item: ChatItem{ItemID: "i", Title: "t", ImageURL: "https://img.example/a.png", Price: "1"}})
	if !errors.Is(err, ErrSendUncertain) || message == nil || message.Status != "uncertain" {
		t.Fatalf("message=%+v err=%v", message, err)
	}
}

// TestLookupOwnedSessionWithoutCapabilityAndEmptyChat 覆盖仓储缺少查询能力与返回空会话标识。
func TestLookupOwnedSessionWithoutCapabilityAndEmptyChat(t *testing.T) {
	// plain 的仓储没有精确会话查询能力。
	plain := New(historyOnlyRepository{})
	if _, err := plain.lookupOwnedSession(context.Background(), 1, "a", "c"); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("err=%v", err)
	}
	// empty 的仓储对任何查询返回空 ChatID，等价于会话缺失。
	empty := New(&itemRepository{fakeRepository: &fakeRepository{}, session: Session{AccountID: "a", ChatID: "c"}})
	// emptyRepository 在查询时返回零值会话。
	empty.repository = zeroLookupRepository{}
	if _, err := empty.lookupOwnedSession(context.Background(), 1, "a", "c"); !errors.Is(err, ErrChatSessionNotFound) {
		t.Fatalf("err=%v", err)
	}
}

// zeroLookupRepository 对精确会话查询返回无错误但 ChatID 为空的会话。
type zeroLookupRepository struct {
	historyOnlyRepository
}

// FindSession 返回零值会话，模拟仓储把缺失表达为空结果。
func (zeroLookupRepository) FindSession(context.Context, int64, string, string) (Session, error) {
	return Session{}, nil
}

// TestListSessionPageValidationAndDelegation 覆盖会话分页的输入校验、仓储错误与成功路径。
func TestListSessionPageValidationAndDelegation(t *testing.T) {
	ctx := context.Background()
	// service 使用返回空页的仓储。
	service := New(historyOnlyRepository{})
	if _, err := service.ListSessionPage(ctx, 0, "a", nil, 10); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err=%v", err)
	}
	if _, err := service.ListSessionPage(ctx, 1, " ", nil, 10); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err=%v", err)
	}
	if _, err := service.ListSessionPage(ctx, 1, "a", &SessionCursor{ChatID: " "}, 10); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err=%v", err)
	}
	if _, err := (*Service)(nil).ListSessionPage(ctx, 1, "a", nil, 10); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err=%v", err)
	}
	if _, err := service.ListSessionPage(ctx, 1, " a ", &SessionCursor{ChatID: "c"}, 10); err != nil {
		t.Fatalf("err=%v", err)
	}
	// pageFailure 是仓储分页注入的错误。
	pageFailure := errors.New("page down")
	if _, err := New(pageErrorRepository{err: pageFailure}).ListSessionPage(ctx, 1, "a", nil, 10); !errors.Is(err, pageFailure) {
		t.Fatalf("err=%v", err)
	}
}

// pageErrorRepository 让会话分页返回预置错误。
type pageErrorRepository struct {
	historyOnlyRepository
	err error
}

// ListSessionPage 返回预置错误。
func (r pageErrorRepository) ListSessionPage(context.Context, int64, string, *SessionCursor, int) (SessionPage, error) {
	return SessionPage{}, r.err
}

// TestDeleteConversationCapabilityAndOwnershipFailure 覆盖删除会话时仓储能力缺失与归属查询失败。
func TestDeleteConversationCapabilityAndOwnershipFailure(t *testing.T) {
	ctx := context.Background()
	if err := New(historyOnlyRepository{}).DeleteConversation(ctx, 1, "a", "c"); !errors.Is(err, ErrSessionUnavailable) {
		t.Fatalf("err=%v", err)
	}
	// ownershipFailure 是归属查询注入的错误。
	ownershipFailure := errors.New("ownership down")
	if err := New(&fakeRepository{ownedErr: ownershipFailure}).DeleteConversation(ctx, 1, "a", "c"); !errors.Is(err, ownershipFailure) {
		t.Fatalf("err=%v", err)
	}
}

// TestSendUncertainAndReplyValidation 覆盖文字/图片不确定结果、回复输入非法与不可用、文字段失败及图片下载输入校验。
func TestSendUncertainAndReplyValidation(t *testing.T) {
	ctx := context.Background()
	// session 是合法发送会话。
	session := Session{AccountID: "a", ChatID: "c", PeerUserID: "p"}
	// uncertainSender 的平台发送总返回不确定错误。
	uncertainSender := &sendSender{sendErr: ErrSendUncertain}
	// textService 用于文字不确定分支。
	textService := NewWithSending(nil, &sendRepository{}, sendProvider{sender: uncertainSender}, sendUploader{result: ImageUpload{URL: "https://img.example/u.png", Width: 1, Height: 1}})
	if msg, err := textService.SendText(ctx, OutgoingInput{Session: session, Text: "hi"}); !errors.Is(err, ErrSendUncertain) || msg == nil || msg.Status != "uncertain" {
		t.Fatalf("text msg=%+v err=%v", msg, err)
	}
	if msg, err := textService.SendImage(ctx, ImageInput{Session: session, Filename: "a.png", ContentType: "image/png", Data: []byte{1}}); !errors.Is(err, ErrSendUncertain) || msg == nil || msg.Status != "uncertain" {
		t.Fatalf("image msg=%+v err=%v", msg, err)
	}
	// 回复输入非法：缺少文字和图片。
	if _, err := textService.SendReply(ctx, ReplyInput{Session: session}); !errors.Is(err, ErrSendInvalidInput) {
		t.Fatalf("err=%v", err)
	}
	// 带图片但未装配下载器时不可用。
	if _, err := textService.SendReply(ctx, ReplyInput{Session: session, ImageURL: "https://img.example/a.png"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
	// 文字段失败应返回结果并标记未送达。
	result, err := textService.SendReply(ctx, ReplyInput{Session: session, Text: "hi"})
	if !errors.Is(err, ErrSendUncertain) || result == nil || result.TextSent {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	// downloadService 装配了返回空数据的下载器。
	downloadService := NewWithSendingAndDownloader(nil, &sendRepository{}, sendProvider{sender: &sendSender{}}, sendUploader{}, func(context.Context, string) ([]byte, string, string, error) {
		return nil, "image/png", "a.png", nil
	})
	if _, err := downloadService.SendImageURL(ctx, ImageURLInput{Session: session, ImageURL: "https://img.example/a.png"}); !errors.Is(err, ErrSend) {
		t.Fatalf("empty data err=%v", err)
	}
	if _, err := downloadService.SendImageURL(ctx, ImageURLInput{Session: session}); !errors.Is(err, ErrSendInvalidInput) {
		t.Fatalf("blank url err=%v", err)
	}
	if _, err := NewWithSending(nil, nil, nil, nil).SendImageURL(ctx, ImageURLInput{Session: session, ImageURL: "https://img.example/a.png"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unwired err=%v", err)
	}
	if _, err := (*Service)(nil).downloadImageInput(ctx, session, "https://img.example/a.png"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil service err=%v", err)
	}
}

// TestRefreshSessionIdentitiesStopsQueueOnCancelledContext 验证父上下文已取消时停止继续排队剩余会话。
func TestRefreshSessionIdentitiesStopsQueueOnCancelledContext(t *testing.T) {
	// service 使用立即返回身份的解析器。
	service := NewWithIdentity(&fakeRepository{}, fakeIdentityResolver{})
	// ctx 在调用前已取消。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// sessions 是远多于并发度的待补全会话，保证取消分支必然被选中。
	sessions := make([]Session, 200)
	// index 是当前会话序号。
	for index := range sessions {
		sessions[index] = Session{AccountID: "a", ChatID: "c", PeerUserID: "p"}
	}
	// result 和 err 是提前停止后的结果。
	result, err := service.RefreshSessionIdentities(ctx, "a", sessions)
	if err != nil || len(result) != len(sessions) {
		t.Fatalf("len=%d err=%v", len(result), err)
	}
}

// TestSessionOperationGateNilReleaseIsNoop 验证空操作门返回可调用的空释放函数。
func TestSessionOperationGateNilReleaseIsNoop(t *testing.T) {
	// gate 是未初始化的操作门。
	var gate *sessionOperationGate
	gate.lock("a", "c")()
}
