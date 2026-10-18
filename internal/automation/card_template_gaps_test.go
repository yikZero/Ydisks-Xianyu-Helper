package automation

import (
	"context"
	"errors"
	"strings"
	"testing"

	"xianyu-go/internal/db"
)

// scriptedAPIFetcher 按预置结果返回 API 卡密响应，记录请求次数，用于覆盖取卡成功、拒绝与结果不明分支。
type scriptedAPIFetcher struct {
	// result 是每次请求返回的卡密内容与是否已派发标志。
	result APICardResult
	err    error
	// calls 统计 Fetch 被调用的次数，用于断言失败后不再重复取卡。
	calls int
}

// Fetch 返回预置结果并累计调用次数。
func (f *scriptedAPIFetcher) Fetch(context.Context, APICardRequest) (APICardResult, error) {
	f.calls++
	return f.result, f.err
}

// anySenderProvider 向所有账号返回同一个任意类型的发送器替身。
type anySenderProvider struct {
	// sender 是被返回的发送器替身。
	sender MessageSender
}

// Sender 返回固定发送器并报告在线。
func (p anySenderProvider) Sender(string) (MessageSender, bool) { return p.sender, true }

// imageFailSender 在发送图片时返回平台错误，其余行为沿用文本发送替身。
type imageFailSender struct {
	// testSender 提供文本发送与 Cookie 更新的默认实现。
	*testSender
	// imageErr 是图片发送返回的错误。
	imageErr error
}

// SendImage 返回预置错误，模拟图片发送结果无法确认。
func (s imageFailSender) SendImage(context.Context, string, string, string, int64, int, int) error {
	return s.imageErr
}

// TestSendCardImageGapBranches 覆盖图片卡与文本卡发送中的读取失败、离线与结果不明分支。
func TestSendCardImageGapBranches(t *testing.T) {
	ctx := context.Background()
	// task 是完整会话信息的发货任务。
	task := Task{AccountID: "cid", OrderID: "card-gap", ChatID: "chat", BuyerID: "buyer", Quantity: "1"}
	// newCard 创建指定类型的卡密组并返回对应发卡动作。
	newCard := func(t *testing.T, store *db.Store, card *db.CardFull) db.AutomationAction {
		t.Helper()
		// admin 是卡密组归属用户。
		admin, _ := store.Users.GetByUsername(ctx, "admin")
		card.UserID = admin.ID
		card.Enabled = true
		// cardID、createErr 保存卡密组创建结果。
		cardID, createErr := store.Cards.Create(ctx, card)
		if createErr != nil {
			t.Fatal(createErr)
		}
		return db.AutomationAction{ActionType: ActionSendCard, Enabled: true, CardID: cardID, DeliveryCount: 1, ConfigJSON: `{}`}
	}

	t.Run("图片卡发送成功记录图片凭证", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		// action 绑定图片卡。
		action := newCard(t, store, &db.CardFull{Name: "img", Type: "image", ImageURL: "http://img/a.png"})
		// center 使用默认文本发送替身，其图片发送恒成功。
		center := New(store, testSenderProvider{sender: &testSender{}}, nil)
		// result 保存发卡结果。
		result, sendErr := center.actions.sendCardWithProof(ctx, task, action)
		if sendErr != nil || result.sent != 1 || len(result.proof.picList) != 1 {
			t.Fatalf("result=%+v err=%v", result, sendErr)
		}
	})
	t.Run("图片卡账号离线为确定未发送", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		// action 绑定图片卡。
		action := newCard(t, store, &db.CardFull{Name: "img", Type: "image", ImageURL: "http://img/a.png"})
		// center 的发送器提供方报告账号离线。
		center := New(store, offlineSenders{}, nil)
		// result 保存发卡结果；确定未发送时不应留下待核对凭证。
		result, sendErr := center.actions.sendCardWithProof(ctx, task, action)
		if !errors.Is(sendErr, ErrMessageNotSent) || len(result.reviewProof.messages) != 0 {
			t.Fatalf("result=%+v err=%v", result, sendErr)
		}
	})
	t.Run("图片发送结果不明保留待核对凭证", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		// action 绑定图片卡。
		action := newCard(t, store, &db.CardFull{Name: "img", Type: "image", ImageURL: "http://img/a.png"})
		// center 的图片发送返回无法分类的平台错误。
		center := New(store, anySenderProvider{sender: imageFailSender{testSender: &testSender{}, imageErr: errors.New("连接中断")}}, nil)
		// result 保存发卡结果。
		result, sendErr := center.actions.sendCardWithProof(ctx, task, action)
		if sendErr == nil || len(result.reviewProof.messages) != 1 || result.reviewProof.messages[0].Kind != "image" {
			t.Fatalf("result=%+v err=%v", result, sendErr)
		}
	})
	t.Run("文本卡内容为空读取失败", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		// action 绑定文本卡，随后把正文清空。
		action := newCard(t, store, &db.CardFull{Name: "txt", Type: "text", TextContent: "CARD"})
		execGapSQL(t, store, `UPDATE cards SET text_content=''`)
		// center 使用默认发送替身。
		center := New(store, testSenderProvider{sender: &testSender{}}, nil)
		// sendErr 是向买家发送消息或卡密失败时返回的错误，用于断言是否保留待核对快照。
		if _, sendErr := center.actions.sendCardWithProof(ctx, task, action); sendErr == nil {
			t.Fatal("空正文应报错")
		}
	})
	t.Run("文本卡账号离线为确定未发送", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		// action 绑定文本卡。
		action := newCard(t, store, &db.CardFull{Name: "txt", Type: "text", TextContent: "CARD"})
		// center 的发送器提供方报告账号离线。
		center := New(store, offlineSenders{}, nil)
		// sendErr 是向买家发送消息或卡密失败时返回的错误，用于断言是否保留待核对快照。
		if _, sendErr := center.actions.sendCardWithProof(ctx, task, action); !errors.Is(sendErr, ErrMessageNotSent) {
			t.Fatalf("err=%v", sendErr)
		}
	})
	t.Run("文本发送结果不明保留渲染文本", func(t *testing.T) {
		store, cleanup := newAutomationTestStore(t)
		defer cleanup()
		// action 绑定文本卡。
		action := newCard(t, store, &db.CardFull{Name: "txt", Type: "text", TextContent: "CARD"})
		// center 的文本发送返回无法分类的平台错误。
		center := New(store, testSenderProvider{sender: &testSender{err: errors.New("连接中断")}}, nil)
		// result 保存发卡结果。
		result, sendErr := center.actions.sendCardWithProof(ctx, task, action)
		if sendErr == nil || len(result.reviewProof.messages) != 1 || result.reviewProof.messages[0].Content != "CARD" {
			t.Fatalf("result=%+v err=%v", result, sendErr)
		}
	})
}

// TestSendAPICardGapBranches 覆盖 API 卡发货的取卡失败、空响应与买家消息发送失败；失败后不得重复取卡。
func TestSendAPICardGapBranches(t *testing.T) {
	ctx := context.Background()
	// task 是完整会话信息的发货任务。
	task := Task{AccountID: "cid", OrderID: "api-gap", ChatID: "chat", BuyerID: "buyer", Quantity: "1"}
	// card 是 API 卡密组。
	card := &db.CardFull{ID: 7, Type: "api", APIConfig: `{}`}
	// run 用指定取卡结果与发送器执行一次 API 卡发货。
	run := func(fetcher *scriptedAPIFetcher, sender MessageSender) (actionExecutionResult, error) {
		// executor 只注入 API 取卡与发送依赖。
		executor := automationActionExecutor{senders: anySenderProvider{sender: sender}, apiFetcher: func() APICardFetcher { return fetcher }}
		return executor.sendAPICardWithProof(ctx, task, db.AutomationAction{ID: 3}, card, 1)
	}

	t.Run("请求已派发后失败视为结果不明", func(t *testing.T) {
		// fetcher 的请求已派发但返回错误。
		fetcher := &scriptedAPIFetcher{result: APICardResult{Dispatched: true}, err: errors.New("响应超时")}
		// uncertain 用于识别不确定错误。
		var uncertain *uncertainActionError
		// apiErr 是 API 卡发货在本场景下返回的错误，用于断言结果不明与可重试的分类。
		if _, apiErr := run(fetcher, &testSender{}); !errors.As(apiErr, &uncertain) {
			t.Fatalf("err=%v", apiErr)
		}
	})
	t.Run("请求未派发失败禁止自动重试", func(t *testing.T) {
		// fetcher 的请求在派发前失败。
		fetcher := &scriptedAPIFetcher{err: errors.New("配置非法")}
		// apiErr 是 API 卡发货在本场景下返回的错误，用于断言结果不明与可重试的分类。
		if _, apiErr := run(fetcher, &testSender{}); apiErr == nil || strings.Contains(apiErr.Error(), "uncertain") {
			t.Fatalf("err=%v", apiErr)
		}
		if fetcher.calls != 1 {
			t.Fatalf("calls=%d", fetcher.calls)
		}
	})
	t.Run("响应没有内容视为结果不明", func(t *testing.T) {
		// fetcher 返回空内容。
		fetcher := &scriptedAPIFetcher{}
		// uncertain 用于识别不确定错误。
		var uncertain *uncertainActionError
		// apiErr 是 API 卡发货在本场景下返回的错误，用于断言结果不明与可重试的分类。
		if _, apiErr := run(fetcher, &testSender{}); !errors.As(apiErr, &uncertain) {
			t.Fatalf("err=%v", apiErr)
		}
	})
	t.Run("取得卡密后发送失败只保留快照", func(t *testing.T) {
		// fetcher 返回卡密内容。
		fetcher := &scriptedAPIFetcher{result: APICardResult{Content: "API-SECRET"}}
		// result 保存发卡结果。
		result, apiErr := run(fetcher, &testSender{err: errors.New("连接中断")})
		if apiErr == nil || len(result.reviewProof.messages) != 1 || fetcher.calls != 1 {
			t.Fatalf("result=%+v err=%v calls=%d", result, apiErr, fetcher.calls)
		}
	})
}

// TestTemplateAPILoadGapBranches 覆盖模板变量的缺失绑定与 API 取卡失败分类。
func TestTemplateAPILoadGapBranches(t *testing.T) {
	ctx := context.Background()
	// task 是完整会话信息的发货任务。
	task := Task{AccountID: "cid", OrderID: "tpl-gap", Quantity: "1"}
	// executor 不需要数据库，因为本测试只驱动 API 变量加载。
	executor := automationActionExecutor{}
	// bindingFor 构造 API 卡变量绑定，每份取 1 个单位。
	bindingFor := func(key string) templateBindingCard {
		return templateBindingCard{binding: db.DeliveryTemplateBinding{VariableKey: key}, card: &db.CardFull{ID: 9, Type: "api", APIConfig: `{}`}, count: 1}
	}

	t.Run("变量缺少卡密绑定", func(t *testing.T) {
		// state 没有任何绑定。
		state := templateDeliveryState{bindingCards: map[string]templateBindingCard{}, values: map[string]string{}, loadedKeys: map[string]bool{}}
		// loadErr 是模板变量加载（API 取卡或库存消费）失败时的错误，用于断言不确定性分类。
		if _, _, loadErr := executor.loadTemplateMessageValues(ctx, task, db.AutomationAction{}, &state, "你好 {{cards.missing}}"); loadErr == nil {
			t.Fatal("缺少绑定应报错")
		}
	})
	t.Run("API客户端缺失", func(t *testing.T) {
		// state 没有 API 取卡客户端。
		state := templateDeliveryState{}
		// failure 表示门禁是否因业务原因拒绝后续处理；loadErr 是模板变量加载（API 取卡或库存消费）失败时的错误，用于断言不确定性分类。
		if _, _, _, failure, _, _, loadErr := executor.loadTemplateAPILines(ctx, task, db.AutomationAction{}, &state, bindingFor("a"), nil); loadErr == nil || !failure {
			t.Fatalf("failure=%v err=%v", failure, loadErr)
		}
	})
	t.Run("API取卡失败的三类分类", func(t *testing.T) {
		// cases 描述取卡替身与期望：dispatched 为请求是否已派发，empty 为是否空响应。
		cases := []struct {
			name       string
			fetcher    *scriptedAPIFetcher
			dispatched bool
			empty      bool
		}{
			{"已派发失败", &scriptedAPIFetcher{result: APICardResult{Dispatched: true}, err: errors.New("超时")}, true, false},
			{"未派发失败", &scriptedAPIFetcher{err: errors.New("配置非法")}, false, false},
			{"空响应", &scriptedAPIFetcher{result: APICardResult{Dispatched: true}}, true, true},
		}
		// tc 表示当前取卡失败用例。
		for _, tc := range cases {
			// state 注入该用例的取卡客户端。
			state := templateDeliveryState{apiFetcher: tc.fetcher}
			// dispatched 表示 API 取卡请求是否已经派发到远端（已派发则结果可能已产生副作用）；empty 表示 API 卡响应是否没有可发送内容；failure 表示门禁是否因业务原因拒绝后续处理；loadErr 是模板变量加载（API 取卡或库存消费）失败时的错误，用于断言不确定性分类。
			_, _, _, failure, dispatched, empty, loadErr := executor.loadTemplateAPILines(ctx, task, db.AutomationAction{}, &state, bindingFor("a"), nil)
			if loadErr == nil || !failure || dispatched != tc.dispatched || empty != tc.empty {
				t.Fatalf("%s: failure=%v dispatched=%v empty=%v err=%v", tc.name, failure, dispatched, empty, loadErr)
			}
		}
	})
	t.Run("取卡失败后的错误分类", func(t *testing.T) {
		// cases 描述模板变量加载失败时 loadTemplateMessageValues 返回的错误是否带不确定标记。
		cases := []struct {
			name           string
			fetcher        *scriptedAPIFetcher
			alreadyFetched bool
			wantUncertain  bool
		}{
			{"已派发失败", &scriptedAPIFetcher{result: APICardResult{Dispatched: true}, err: errors.New("超时")}, false, true},
			{"未派发失败", &scriptedAPIFetcher{err: errors.New("配置非法")}, false, false},
			{"此前已成功取卡后失败", &scriptedAPIFetcher{err: errors.New("配置非法")}, true, true},
		}
		// tc 表示当前用例。
		for _, tc := range cases {
			// state 绑定 API 变量 a，并按用例标记此前是否已取过卡。
			state := templateDeliveryState{
				apiFetcher: tc.fetcher, apiFetched: tc.alreadyFetched,
				bindingCards: map[string]templateBindingCard{"a": bindingFor("a")}, values: map[string]string{}, loadedKeys: map[string]bool{},
			}
			// message 引用变量 a。
			message := "卡密 {{cards.a}}"
			// loadErr 是模板变量加载（API 取卡或库存消费）失败时的错误，用于断言不确定性分类。
			_, _, loadErr := executor.loadTemplateMessageValues(ctx, task, db.AutomationAction{}, &state, message)
			// uncertain 用于识别不确定错误。
			var uncertain *uncertainActionError
			if loadErr == nil || errors.As(loadErr, &uncertain) != tc.wantUncertain {
				t.Fatalf("%s: err=%v", tc.name, loadErr)
			}
		}
	})
}
