package db

import (
	"context"
	"testing"
)

// faultScenario 描述一个故障注入场景：name 为子测试名，setup 在未布防时准备数据并返回待测调用。
type faultScenario struct {
	name string
	// setup 准备数据并返回在故障布防后执行的调用。
	setup func(t *testing.T, store *Store) func(ctx context.Context) error
}

// runFaultScenarios 为每个场景启动并行子测试，共享同一个已迁移的模板库。
func runFaultScenarios(t *testing.T, scenarios []faultScenario) {
	t.Helper()
	// template 是迁移后的空库文件。
	template := faultTemplate(t)
	// scenario 是当前待执行的场景。
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			runFaultScenario(t, template, scenario.setup)
		})
	}
}

// TestFaultInjectionAccountsUsersSessionsChat 在账号、用户、会话、聊天仓储的每个数据库操作点注入故障，覆盖事务内错误回滚分支。
func TestFaultInjectionAccountsUsersSessionsChat(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("重型故障注入遍历在竞态检测模式下耗时过长，常规模式已覆盖")
	}
	// boolTrue、boolFalse 是设置更新使用的布尔指针取值。
	boolTrue, boolFalse := true, false
	// text 是设置更新使用的字符串指针取值。
	text := "更新值"
	// pause 是暂停分钟数指针。
	pause := 5
	// seed 创建管理员与账号并返回其 ID。
	seed := func(t *testing.T, store *Store) (int64, string) { return seedAccount(t, store) }
	runFaultScenarios(t, []faultScenario{
		{"UpdateSettings", func(t *testing.T, store *Store) func(context.Context) error {
			// userID、cookieID 是种子账号归属。
			userID, cookieID := seed(t, store)
			// channelIDs 是空通知绑定，使事务进入绑定替换分支。
			channelIDs := []int64{}
			return func(ctx context.Context) error {
				_, err := store.Cookies.UpdateSettings(ctx, cookieID, AccountSettingsUpdate{UserID: userID, Value: &text, Remark: &text, AutoConfirm: &boolTrue, AutoConsign: &boolFalse, AutoBargain: &boolTrue, PauseDuration: &pause, Username: &text, Password: &text, ShowBrowser: &boolTrue, ChannelIDs: &channelIDs})
				return err
			}
		}},
		{"CookieWrites", func(t *testing.T, store *Store) func(context.Context) error {
			// userID、cookieID 是种子账号归属。
			userID, cookieID := seed(t, store)
			return func(ctx context.Context) error {
				_ = store.Cookies.CreateOwned(ctx, "acc-new", "cv=new", userID)
				_ = store.Cookies.UpdateValueOwned(ctx, cookieID, "cv=owned", userID)
				_ = store.Cookies.UpdateValueExisting(ctx, cookieID, "cv=existing")
				_ = store.Cookies.Save(ctx, "acc-saved", "cv=saved", userID)
				_ = store.Cookies.UpdateLoginInfo(ctx, cookieID, "u", "p", true)
				_ = store.Cookies.MarkLogin(ctx, cookieID, "qr", 1)
				_ = store.Cookies.UpdateProfile(ctx, cookieID, "nick", "http://a/b.png")
				_, _ = store.Cookies.SetPause(ctx, cookieID, 3)
				_ = store.Cookies.SetStatusWithReason(ctx, cookieID, false, "原因")
				return store.Cookies.Delete(ctx, cookieID)
			}
		}},
		{"CookieReads", func(t *testing.T, store *Store) func(context.Context) error {
			// cookieID 是种子账号。
			_, cookieID := seed(t, store)
			return func(ctx context.Context) error {
				_, _ = store.Cookies.GetValue(ctx, cookieID)
				_, _ = store.Cookies.GetDetails(ctx, cookieID)
				_, _, _ = store.Cookies.IsPaused(ctx, cookieID)
				_, _ = store.Cookies.GetAutoConfirm(ctx, cookieID)
				_, _ = store.Cookies.GetAutoConsign(ctx, cookieID)
				_, _ = store.Cookies.GetAutoBargain(ctx, cookieID)
				_, _, err := store.Cookies.StatusWithReason(ctx, cookieID)
				return err
			}
		}},
		{"Users", func(t *testing.T, store *Store) func(context.Context) error {
			// userID 是种子管理员。
			userID, _ := seed(t, store)
			return func(ctx context.Context) error {
				_, _ = store.Users.IsSystemInitialized(ctx)
				_, _ = store.Users.Create(ctx, "second", "second@e.com", "pw2")
				_, _, _ = store.Users.VerifyAndUpgrade(ctx, "admin", "pw")
				_, _ = store.Users.UpdatePassword(ctx, "admin", "newpw")
				_ = store.Users.UpdateCredentials(ctx, userID, "admin2", "newpw2")
				_ = store.Users.SetAdmin(ctx, "admin2")
				return store.Users.Delete(ctx, userID)
			}
		}},
		{"UsersDeleteOther", func(t *testing.T, store *Store) func(context.Context) error {
			seed(t, store)
			// err、ok 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
			if ok, err := store.Users.Create(context.Background(), "victim", "v@e.com", "pw"); err != nil || !ok {
				t.Fatalf("create victim ok=%v err=%v", ok, err)
			}
			// victim 是待删除的普通用户。
			victim, err := store.Users.GetByUsername(context.Background(), "victim")
			if err != nil {
				t.Fatal(err)
			}
			return func(ctx context.Context) error { return store.Users.Delete(ctx, victim.ID) }
		}},
		{"Sessions", func(t *testing.T, store *Store) func(context.Context) error {
			seed(t, store)
			// user 是会话所属用户快照。
			user, err := store.Users.GetByUsername(context.Background(), "admin")
			if err != nil {
				t.Fatal(err)
			}
			return func(ctx context.Context) error {
				_, _ = store.Sessions.Create(ctx, user)
				// sessionID 是校验创建出的会话。
				sessionID, _ := store.Sessions.CreateVerified(ctx, user, user.AuthVersion)
				_, _ = store.Sessions.Get(ctx, sessionID)
				_, _ = store.Sessions.DeleteExpired(ctx)
				return store.Sessions.Delete(ctx, sessionID)
			}
		}},
		{"Chat", func(t *testing.T, store *Store) func(context.Context) error {
			// userID、cookieID 是种子账号归属。
			userID, cookieID := seed(t, store)
			// session 是被测聊天会话。
			session := ChatSession{CookieID: cookieID, ChatID: "chat-1", BuyerID: "buyer-1", BuyerName: "买家", ItemID: "item-1", LastMessageAt: 10}
			return func(ctx context.Context) error {
				_ = store.Chats.UpsertSession(ctx, session)
				_, _, _ = store.Chats.SaveMessage(ctx, session, ChatMessage{MessageKey: "m1", Direction: "incoming", SenderID: "buyer-1", MessageType: "text", Content: "你好", SentAt: 100}, true)
				_, _, _ = store.Chats.SaveMessage(ctx, session, ChatMessage{MessageKey: "m2", Direction: "outgoing", MessageType: "text", Content: "回复", SentAt: 200}, false)
				_, _ = store.Chats.FindChatIDsByBuyerAndItem(ctx, cookieID, "buyer-1", "item-1")
				_ = store.Chats.SetSessionVisible(ctx, cookieID, "chat-1", true)
				_ = store.Chats.SyncSessionSummary(ctx, cookieID, "chat-1", "摘要", 300, 300, 1)
				_ = store.Chats.UpdateSessionIdentity(ctx, cookieID, "chat-1", "buyer-1", "新昵称", "http://a/b.png")
				_, _ = store.Chats.LatestUnmaskedPeerName(ctx, cookieID, "chat-1")
				_, _ = store.Chats.BuyerNicknameForAutomation(ctx, cookieID, "chat-1")
				_, _ = store.Chats.GetMessageByKey(ctx, cookieID, "m1")
				_ = store.Chats.UpdateMessageContent(ctx, cookieID, "m1", "text", "新内容")
				_ = store.Chats.UpdateMessageMediaDuration(ctx, cookieID, "m1", 3)
				_, _ = store.Chats.ListSessions(ctx, userID, cookieID, 10)
				_, _ = store.Chats.FindSession(ctx, userID, cookieID, "chat-1")
				_, _ = store.Chats.ListSessionPage(ctx, userID, cookieID, nil, 1)
				_, _ = store.Chats.ListMessages(ctx, userID, cookieID, "chat-1", 0, 10)
				_ = store.Chats.MarkRead(ctx, userID, cookieID, "chat-1")
				_, _ = store.Chats.CountUnreadUserMessages(ctx, cookieID, "chat-1")
				_, _ = store.Chats.UpdateMessageStatus(ctx, cookieID, "m2", "sent")
				_, _ = store.Chats.MarkMessageRead(ctx, cookieID, "m2", 400)
				_, _ = store.Chats.MarkLatestOutgoingRead(ctx, cookieID, "chat-1", 500)
				_ = store.Chats.UpdateSessionRole(ctx, cookieID, "chat-1", "item-1", "seller", "b", "s", "local_item")
				_, _ = store.Chats.SessionRole(ctx, cookieID, "chat-1", "item-1")
				_, _ = store.Chats.CreateQuickReply(ctx, cookieID, "快捷回复")
				_, _ = store.Chats.ListQuickReplies(ctx, cookieID)
				_, _ = store.Chats.DeleteQuickReply(ctx, cookieID, 1)
				_, _ = store.Chats.SaveBuyerNote(ctx, ChatBuyerNote{CookieID: cookieID, BuyerID: "buyer-1", Content: "备注"})
				_, _, _ = store.Chats.GetBuyerNote(ctx, cookieID, "buyer-1")
				_ = store.Chats.DeleteEmptySessions(ctx, cookieID)
				_, _ = store.Chats.HideAndClearSession(ctx, userID, cookieID, "chat-1", 600)
				return store.Chats.DeleteSession(ctx, cookieID, "chat-1")
			}
		}},
	})
}
