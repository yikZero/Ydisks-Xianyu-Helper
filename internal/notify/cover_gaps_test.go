package notify

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"testing"

	"xianyu-go/internal/db"
)

// queueRepository 在脚本化 outbox 仓储上提供渠道列表并捕获入队内容，用于验证通知入队分支。
type queueRepository struct {
	*scriptedOutboxRepository
	// channels 是账号绑定的已启用渠道。
	channels []db.NotificationChannel
	// enqueueErr 是入队时应返回的持久化错误。
	enqueueErr error
	// enqueued 保存最近一次入队的消息，供断言事件类别。
	enqueued []db.NotificationOutboxInput
}

// AccountChannels 返回预置渠道。
func (r *queueRepository) AccountChannels(context.Context, string) ([]db.NotificationChannel, error) {
	return r.channels, nil
}

// EnqueueOutbox 记录入队消息并返回预置错误。
func (r *queueRepository) EnqueueOutbox(_ context.Context, messages []db.NotificationOutboxInput) error {
	r.enqueued = messages
	return r.enqueueErr
}

// TestAutomationEventTypeMapsEveryTrigger 验证全部自动化触发编码到通知类别的映射及默认回退。
func TestAutomationEventTypeMapsEveryTrigger(t *testing.T) {
	// cases 是触发编码到期望通知类别的映射。
	cases := map[string]string{
		"order_created":          EventAutomationOrderCreated,
		" order_paid ":           EventAutomationOrderPaid,
		"bargain_pending":        EventAutomationOrderPaid,
		"buyer_reviewed":         EventAutomationBuyerReviewed,
		"review_missing_timeout": EventAutomationReviewMissingTimeout,
		"unknown":                EventDeliveryResult,
	}
	// trigger 和 want 是当前触发编码及期望类别。
	for trigger, want := range cases {
		if got := automationEventType(trigger); got != want {
			t.Fatalf("trigger=%q got=%s want=%s", trigger, got, want)
		}
	}
}

// TestNotifyManualInterventionQueuesWithSourceFallback 验证人工处理通知保留原自动化类别兼容订阅，并忽略缺少账号或幂等键的请求。
func TestNotifyManualInterventionQueuesWithSourceFallback(t *testing.T) {
	// repository 提供一个只订阅原自动化类别的渠道。
	repository := &queueRepository{scriptedOutboxRepository: &scriptedOutboxRepository{}, channels: []db.NotificationChannel{{ID: 1, Type: "webhook", EventTypes: EventAutomationBuyerReviewed}}}
	// notifier 使用该仓储。
	notifier := NewWithRepository("cid", repository, nilLogger())
	// nilNotifier 保存空通知器指针，用于验证空接收者调用不会 panic。
	var nilNotifier *Notifier
	nilNotifier.NotifyManualIntervention(context.Background(), "x", "cid", "o", "i", "b", "act", "why", "chat", "key")
	notifier.NotifyManualIntervention(context.Background(), "buyer_reviewed", " ", "o", "i", "b", "act", "why", "chat", "key")
	notifier.NotifyManualIntervention(context.Background(), "buyer_reviewed", "cid", "o", "i", "b", "act", "why", "chat", " ")
	if repository.enqueued != nil {
		t.Fatal("无效请求不应入队")
	}
	notifier.NotifyManualIntervention(context.Background(), "buyer_reviewed", "cid", "", "", "", "", "", "", " key ")
	if len(repository.enqueued) != 1 || repository.enqueued[0].EventType != EventManualInterventionRequired || repository.enqueued[0].IdempotencyKey != "key" {
		t.Fatalf("enqueued=%+v", repository.enqueued)
	}
}

// TestNotifyEventNilContextAndEnqueueFailure 覆盖缺少生命周期 Context 与 outbox 持久化失败两个入队分支。
func TestNotifyEventNilContextAndEnqueueFailure(t *testing.T) {
	// repository 提供一个订阅全部事件的渠道。
	repository := &queueRepository{scriptedOutboxRepository: &scriptedOutboxRepository{}, channels: []db.NotificationChannel{{ID: 1, Type: "webhook"}}}
	// notifier 使用该仓储。
	notifier := NewWithRepository("cid", repository, nilLogger())
	// nilCtx 模拟调用方遗漏生命周期 Context。
	var nilCtx context.Context
	notifier.NotifyEvent(nilCtx, NotificationEvent{AccountID: "cid", Type: EventSystemError})
	if repository.enqueued != nil {
		t.Fatal("nil Context 不应入队")
	}
	repository.enqueueErr = errors.New("db down")
	notifier.started.Store(true)
	notifier.NotifyEvent(context.Background(), NotificationEvent{AccountID: "cid", Type: EventSystemError})
	if len(repository.enqueued) != 1 {
		t.Fatalf("enqueued=%+v", repository.enqueued)
	}
}

// TestWaitContextBeforeStartReturnsImmediately 验证未启动的通知器等待立即返回。
func TestWaitContextBeforeStartReturnsImmediately(t *testing.T) {
	// notifier 尚未调用 Start。
	notifier := NewWithRepository("cid", &scriptedOutboxRepository{}, nilLogger())
	if err := notifier.WaitContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestDrainOutboxCompletionBranches 覆盖无效渠道清理失败/租约转移、发送成功后隔离失败/租约转移、完成确认租约转移与重试未更新。
func TestDrainOutboxCompletionBranches(t *testing.T) {
	// server 是始终返回成功的本地 webhook 目标。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	// webhook 是指向本地服务的渠道配置。
	webhook := &db.NotificationChannel{ID: 7, Type: "webhook", Config: `{"webhook_url":"` + server.URL + `"}`}
	// repository 依次被各子场景重配置。
	repository := &scriptedOutboxRepository{}
	// notifier 使用该替身仓储。
	notifier := NewWithRepository("cid", repository, nilLogger())
	// message 构造一条待投递消息。
	message := func() []db.NotificationOutboxMessage {
		return []db.NotificationOutboxMessage{{ID: 1, ChannelID: 7, EventType: EventSystemError, Body: "b", AttemptCount: 1}}
	}
	// 无效渠道：清理出错。
	repository.pending, repository.channel, repository.completeErr = message(), nil, errors.New("清理失败")
	notifier.drainOutbox(context.Background())
	// 无效渠道：租约转移（完成确认返回 false）。
	repository.pending, repository.completeErr, repository.completeResult = message(), nil, false
	notifier.drainOutbox(context.Background())
	// 发送成功但确认失败且隔离也失败。
	repository.pending, repository.channel, repository.completeErr, repository.uncertainErr = message(), webhook, errors.New("确认失败"), errors.New("隔离失败")
	notifier.drainOutbox(context.Background())
	// 发送成功但确认失败，隔离时租约已转移。
	repository.pending, repository.uncertainErr, repository.uncertainResult = message(), nil, false
	notifier.drainOutbox(context.Background())
	if repository.uncertainCalls != 2 {
		t.Fatalf("uncertainCalls=%d", repository.uncertainCalls)
	}
	// 发送成功，完成确认返回租约已转移。
	repository.pending, repository.completeErr, repository.completeResult = message(), nil, false
	notifier.drainOutbox(context.Background())
	// 发送成功并确认完成。
	repository.pending, repository.completeResult = message(), true
	notifier.drainOutbox(context.Background())
	// 渠道查询失败且重试状态未更新（租约转移）。
	repository.pending, repository.channelErr, repository.retryResult = message(), errors.New("渠道失败"), false
	notifier.drainOutbox(context.Background())
	if repository.retryCalls != 1 {
		t.Fatalf("retryCalls=%d", repository.retryCalls)
	}
}

// TestSettingsRepositoryWithoutSettingsStore 验证系统设置存储缺失时两个读取入口返回明确错误。
func TestSettingsRepositoryWithoutSettingsStore(t *testing.T) {
	// repository 使用没有设置仓储的空 Store。
	repository := Repository(storeRepository{store: &db.Store{}, cookieID: "cid"})
	if _, err := repository.GetSetting(context.Background(), "k"); err == nil {
		t.Fatal("缺少设置仓储必须报错")
	}
	// scoped 是支持按用户读取设置的仓储能力。
	scoped, ok := repository.(userScopedSettings)
	if !ok {
		t.Fatal("仓储应支持用户作用域读取")
	}
	if _, err := scoped.GetSettingForUser(context.Background(), 1, "k"); err == nil {
		t.Fatal("缺少设置仓储必须报错")
	}
}

// TestDingTalkAndPostJSONRequestConstructionErrors 覆盖带签名的钉钉地址解析失败与 JSON 请求构造失败。
func TestDingTalkAndPostJSONRequestConstructionErrors(t *testing.T) {
	// notifier 是未绑定仓储的通知器。
	notifier := New("cid", nil, nilLogger())
	if err := notifier.sendDingTalk(map[string]any{"webhook_url": "http://[::1", "secret": "s"}, "m"); err == nil {
		t.Fatal("非法地址必须报错")
	}
	if err := notifier.postJSON("http://[::1", map[string]string{}); err == nil {
		t.Fatal("非法地址必须报错")
	}
}

// TestSettingValueAndSMTPConfigModes 覆盖设置读取的空仓储、自定义模式取值与回退。
func TestSettingValueAndSMTPConfigModes(t *testing.T) {
	ctx := context.Background()
	if got := (&Notifier{}).settingValue(ctx, "k"); got != "" {
		t.Fatalf("got=%q", got)
	}
	// notifier 使用返回空设置的仓储。
	notifier := NewWithRepository("cid", &scriptedOutboxRepository{}, nilLogger())
	if got := notifier.configOrSetting(ctx, map[string]any{}, "k", "fallback"); got != "fallback" {
		t.Fatalf("got=%q", got)
	}
	// custom 是显式开启渠道自定义 SMTP 的配置。
	custom := map[string]any{"use_custom_smtp": true, "smtp_server": " host "}
	if got := notifier.smtpConfigValue(ctx, custom, "smtp_server", "d"); got != "host" {
		t.Fatalf("got=%q", got)
	}
	if got := notifier.smtpConfigValue(ctx, custom, "smtp_user", "d"); got != "d" {
		t.Fatalf("got=%q", got)
	}
	// system 是显式使用系统 SMTP 的配置，系统设置为空时回退默认值。
	system := map[string]any{"use_custom_smtp": false}
	if got := notifier.smtpConfigValue(ctx, system, "smtp_user", "d"); got != "d" {
		t.Fatalf("got=%q", got)
	}
}

// smtpScript 描述脚本化 SMTP 服务器在各阶段的应答，空串表示使用成功默认值。
type smtpScript struct {
	// greeting 是连接问候行。
	greeting string
	// ehlo 是 EHLO 多行应答。
	ehlo string
	// replies 以大写命令前缀为键覆盖对应应答；键 DATAEND 表示邮件正文结束后的应答。
	replies map[string]string
}

// serveScriptedSMTP 启动按脚本应答的本地 SMTP 服务并返回监听地址。
func serveScriptedSMTP(t *testing.T, script smtpScript) string {
	t.Helper()
	// listener 和 err 是本地回环监听及其创建错误。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			// conn 和 acceptErr 是新接入的客户端连接及接收错误。
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				// reader 逐行读取客户端命令。
				reader := bufio.NewReader(conn)
				// reply 向客户端写一行或多行应答。
				reply := func(text string) { _, _ = conn.Write([]byte(text)) }
				// greeting 保存脚本中配置的 SMTP 问候行，空串使用默认值。
				greeting := script.greeting
				if greeting == "" {
					greeting = "220 scripted\r\n"
				}
				reply(greeting)
				for {
					// line 和 readErr 是一条客户端命令及读取错误。
					line, readErr := reader.ReadString('\n')
					if readErr != nil {
						return
					}
					// upper 是大写命令文本。
					upper := strings.ToUpper(strings.TrimSpace(line))
					// override 是脚本对当前命令的覆盖应答。
					override := ""
					// prefix 和 text 是脚本中的命令前缀及应答。
					for prefix, text := range script.replies {
						if strings.HasPrefix(upper, prefix) {
							override = text
						}
					}
					switch {
					case override != "" && !strings.HasPrefix(upper, "DATA"):
						reply(override)
						if strings.HasPrefix(upper, "QUIT") {
							return
						}
					case strings.HasPrefix(upper, "EHLO"):
						// ehlo 保存脚本中配置的 EHLO 应答，空串使用默认值。
						ehlo := script.ehlo
						if ehlo == "" {
							ehlo = "250 scripted\r\n"
						}
						reply(ehlo)
					case strings.HasPrefix(upper, "DATA"):
						if override != "" {
							reply(override)
							continue
						}
						reply("354 go\r\n")
						for {
							// body 是正文的一行。
							body, bodyErr := reader.ReadString('\n')
							if bodyErr != nil {
								return
							}
							if strings.TrimSpace(body) == "." {
								break
							}
						}
						// end 保存脚本中配置的邮件正文结束应答，空串使用默认值。
						if end := script.replies["DATAEND"]; end != "" {
							reply(end)
						} else {
							reply("250 OK\r\n")
						}
					case strings.HasPrefix(upper, "QUIT"):
						reply("221 bye\r\n")
						return
					default:
						reply("250 OK\r\n")
					}
				}
			}()
		}
	}()
	return listener.Addr().String()
}

// TestSendPublicSMTPProtocolFailures 逐阶段覆盖 SMTP 会话失败：问候、STARTTLS、认证、发件人、收件人、DATA、正文提交与退出。
func TestSendPublicSMTPProtocolFailures(t *testing.T) {
	ctx := context.Background()
	// auth 是允许在回环地址上明文使用的标准认证器。
	auth := smtp.PlainAuth("", "u", "p", "127.0.0.1")
	// cases 描述每个失败阶段的脚本、传输选项与是否带认证。
	cases := []struct {
		name    string
		script  smtpScript
		options smtpTransportOptions
		auth    smtp.Auth
	}{
		{"greeting", smtpScript{greeting: "554 no service\r\n"}, smtpTransportOptions{}, nil},
		{"starttls", smtpScript{ehlo: "250-x\r\n250 STARTTLS\r\n", replies: map[string]string{"STARTTLS": "454 tls unavailable\r\n"}}, smtpTransportOptions{UseSTARTTLS: true}, nil},
		{"auth", smtpScript{ehlo: "250-x\r\n250 AUTH PLAIN\r\n", replies: map[string]string{"AUTH": "535 denied\r\n"}}, smtpTransportOptions{}, auth},
		{"mail", smtpScript{replies: map[string]string{"MAIL": "550 no\r\n"}}, smtpTransportOptions{}, nil},
		{"rcpt", smtpScript{replies: map[string]string{"RCPT": "550 no\r\n"}}, smtpTransportOptions{}, nil},
		{"data", smtpScript{replies: map[string]string{"DATA": "554 no\r\n"}}, smtpTransportOptions{}, nil},
		{"dataend", smtpScript{replies: map[string]string{"DATAEND": "554 rejected\r\n"}}, smtpTransportOptions{}, nil},
		{"quit", smtpScript{replies: map[string]string{"QUIT": "421 bye\r\n"}}, smtpTransportOptions{}, nil},
	}
	// tc 是当前失败阶段。
	for _, tc := range cases {
		// addr 是本阶段脚本服务地址。
		addr := serveScriptedSMTP(t, tc.script)
		if err := sendPublicSMTP(ctx, addr, "127.0.0.1", tc.auth, "a@e.com", "b@e.com", []byte("hi"), tc.options); err == nil {
			t.Fatalf("%s 阶段必须失败", tc.name)
		}
	}
	// 隐式 TLS：服务器返回明文问候导致握手失败。
	addr := serveScriptedSMTP(t, smtpScript{})
	if err := sendPublicSMTP(ctx, addr, "127.0.0.1", nil, "a@e.com", "b@e.com", []byte("hi"), smtpTransportOptions{UseImplicitTLS: true}); err == nil || !strings.Contains(err.Error(), "SSL 握手失败") {
		t.Fatalf("err=%v", err)
	}
	// 非法地址：缺少端口。
	if err := sendPublicSMTP(ctx, "noport", "x", nil, "a@e.com", "b@e.com", nil); err == nil {
		t.Fatal("缺少端口必须失败")
	}
}

// TestSendEmailAddressHandling 覆盖旧版发件人解析、收件人格式与显示名处理。
func TestSendEmailAddressHandling(t *testing.T) {
	// addr 和 received 是可成功收信的本地 SMTP 服务。
	addr, received := fakeSMTPServer(t)
	// host 和 port 是服务地址的两部分。
	host, port, _ := strings.Cut(addr, ":")
	// notifier 是未绑定仓储的通知器。
	notifier := New("cid", nil, nilLogger())
	// base 构造带指定旧版发件人与收件人的渠道配置。
	base := func(legacy, to string) map[string]any {
		return map[string]any{"smtp_server": host, "smtp_port": port, "smtp_user": "u@e.com", "smtp_password": "pw", "smtp_use_tls": false, "smtp_from": legacy, "to_email": to}
	}
	if err := notifier.sendEmail(base("通知中心 <from@e.com>", "收件人 <to@e.com>"), "x"); err != nil {
		t.Fatal(err)
	}
	// body 保存被测函数返回或读取的正文。
	if body := received.String(); !strings.Contains(body, "from@e.com") || !strings.Contains(body, "To: ") {
		t.Fatalf("body=%s", body)
	}
	if err := notifier.sendEmail(base("仅名称", "to@e.com"), "x"); err != nil {
		t.Fatal(err)
	}
	if err := notifier.sendEmail(base("", "not-an-address"), "x"); err == nil || !strings.Contains(err.Error(), "收件邮箱") {
		t.Fatalf("err=%v", err)
	}
}

// settingErrorRepository 让系统设置读取始终失败，用于验证通知器降级为空值。
type settingErrorRepository struct {
	*scriptedOutboxRepository
}

// GetSetting 返回读取失败错误。
func (settingErrorRepository) GetSetting(context.Context, string) (string, error) {
	return "", errors.New("settings down")
}

// TestSettingValueIgnoresReadFailure 验证系统设置读取失败时返回空串而不是传播错误。
func TestSettingValueIgnoresReadFailure(t *testing.T) {
	// notifier 使用设置读取失败的仓储。
	notifier := NewWithRepository("cid", settingErrorRepository{&scriptedOutboxRepository{}}, nilLogger())
	if got := notifier.settingValue(context.Background(), "k"); got != "" {
		t.Fatalf("got=%q", got)
	}
}
