package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"xianyu-go/internal/xianyu/renew"
	"xianyu-go/internal/xianyu/ws"
)

// scriptedWSConn 在基础连接替身上注入注册钩子与立即失败的心跳，用于驱动主循环的各种断线分支。
type scriptedWSConn struct {
	// fakeWSConn 提供基础连接生命周期、接收循环与关闭能力。
	*fakeWSConn
	// heartbeatErr 非空时心跳循环立即以该错误结束，模拟心跳先于接收循环发现断线。
	heartbeatErr error
	// onRegister 非空时在注册阶段被调用，其返回值作为 /reg 结果。
	onRegister func() error
}

// Register 优先执行注册钩子，缺省时委托基础替身。
func (c *scriptedWSConn) Register(ctx context.Context, deviceID, token string) error {
	if c.onRegister != nil {
		return c.onRegister()
	}
	return c.fakeWSConn.Register(ctx, deviceID, token)
}

// HeartbeatLoop 在配置了心跳错误时立即返回该错误，否则委托基础替身。
func (c *scriptedWSConn) HeartbeatLoop(ctx context.Context, interval time.Duration) error {
	if c.heartbeatErr != nil {
		return c.heartbeatErr
	}
	return c.fakeWSConn.HeartbeatLoop(ctx, interval)
}

// runAccountUntilDone 在独立协程运行账号主循环，并在有限时间内等待其自行退出，返回 Run 的错误。
func runAccountUntilDone(t *testing.T, acc *Account, ctx context.Context) error {
	t.Helper()
	// done 接收 Run 的返回值；Run 的所有权属于本辅助函数启动的协程。
	done := make(chan error, 1)
	go func() { done <- acc.Run(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run 未在预算内退出")
		return nil
	}
}

// runAccountUntilDials 运行账号直到拨号次数达到 want，随后取消并等待 Run 退出。
func runAccountUntilDials(t *testing.T, acc *Account, dialer *fakeDialer, want int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// done 接收 Run 的返回值。
	done := make(chan error, 1)
	go func() { done <- acc.Run(ctx) }()
	waitForDialCalls(t, dialer, want)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 Run 未退出")
	}
}

// TestConnectionCoordinatorNilAccountGuards 验证未初始化协调器的 run 与 Join 入口安全。
func TestConnectionCoordinatorNilAccountGuards(t *testing.T) {
	// coordinator 是没有绑定账号的协调器。
	coordinator := &connectionCoordinator{}
	if err := coordinator.run(context.Background()); err == nil {
		t.Fatal("未初始化协调器 run 应返回错误")
	}
	coordinator.waitForOwnedWorkers(context.Background())
}

// TestRunRejectsSecondStartAndStoppedLifecycle 验证生命周期已被占用或已停止时 Run 直接返回取消。
func TestRunRejectsSecondStartAndStoppedLifecycle(t *testing.T) {
	// acc、cleanup 是带数据库的账号及其释放函数。
	acc, _, _, cleanup := newRunAccount(t, &fakeRunMtop{token: "tok"})
	defer cleanup()
	// stopped 先停止账号，使后续启动被不可逆的停止围栏拒绝。
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := acc.StopContext(stopCtx); err != nil {
		t.Fatal(err)
	}
	if err := runAccountUntilDone(t, acc, context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("已停止账号的 Run 应返回 context.Canceled: %v", err)
	}
}

// TestRunConnectLimitStopsWithAuthExpired 验证服务端移除会话时账号进入认证失效并终止循环。
func TestRunConnectLimitStopsWithAuthExpired(t *testing.T) {
	// acc、cleanup 是带数据库的账号及其释放函数。
	acc, _, _, cleanup := newRunAccount(t, &fakeRunMtop{token: "tok"})
	defer cleanup()
	acc.wsDialer = &fakeDialer{results: []dialResult{{conn: &fakeWSConn{recvErr: &ws.RegError{Kind: ws.RegErrorConnectLimit, Code: 1, Reason: "limit"}}}}}
	if err := runAccountUntilDone(t, acc, context.Background()); err != nil {
		t.Fatalf("会话被移除应正常结束循环: %v", err)
	}
	// status 保存账号当前的运行时状态快照
	if status := acc.RuntimeStatus(); status.State != RuntimeAuthExpired {
		t.Fatalf("状态应为认证失效: %q", status.State)
	}
}

// TestRunReconnectsAfterKickoutHeartbeatAndUnknownErrors 验证踢下线与未知接收错误都会清 Token 后重连。
func TestRunReconnectsAfterKickoutHeartbeatAndUnknownErrors(t *testing.T) {
	// cases 描述第一条连接的断线方式。
	cases := []struct {
		name string
		// first 是第一条连接，随后总会再拨号一条阻塞连接。
		first *fakeWSConn
	}{
		{name: "踢下线", first: &fakeWSConn{recvErr: &ws.RegError{Kind: ws.RegErrorAuthentication, Code: 2, Reason: "kick"}}},
		{name: "未知接收错误", first: &fakeWSConn{recvErr: errors.New("weird failure")}},
	}
	// tc 是当前断线场景。
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// acc、cleanup 是带数据库的账号及其释放函数。
			acc, _, _, cleanup := newRunAccount(t, &fakeRunMtop{token: "tok"})
			defer cleanup()
			// dialer 先给出场景连接，再给出常驻阻塞连接。
			dialer := &fakeDialer{results: []dialResult{{conn: tc.first}, {conn: &fakeWSConn{recvBlock: true}}}}
			acc.wsDialer = dialer
			runAccountUntilDials(t, acc, dialer, 2)
		})
	}
}

// TestRunHeartbeatErrorReplacesNilReceiveError 验证心跳错误优先于接收循环的正常结束。
func TestRunHeartbeatErrorReplacesNilReceiveError(t *testing.T) {
	// acc、cleanup 是带数据库的账号及其释放函数。
	acc, _, _, cleanup := newRunAccount(t, &fakeRunMtop{token: "tok"})
	defer cleanup()
	// first 的心跳立即失败；其 Close 会令接收循环以 nil 结束。
	first := &scriptedWSConn{fakeWSConn: &fakeWSConn{recvBlock: true, closeCh: make(chan struct{})}, heartbeatErr: errors.New("heartbeat failed")}
	// dialer 把 scriptedWSConn 作为真实 WSConn 交给主循环。
	dialer := newScriptedDialer(first, &fakeWSConn{recvBlock: true})
	acc.wsDialer = dialer
	// ctx、cancel 控制 Run；拨号两次后取消。
	ctx, cancel := context.WithCancel(context.Background())
	// done 在后台 Run 协程退出时接收其返回值
	done := make(chan error, 1)
	go func() { done <- acc.Run(ctx) }()
	dialer.waitFor(t, 2)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 Run 未退出")
	}
}

// scriptedDialer 按顺序返回任意 WSConn 实现，用于需要自定义连接类型的场景。
type scriptedDialer struct {
	// conns 是依次返回的连接；超出后重复返回最后一个。
	conns []WSConn
	// calls 统计拨号次数，通过 notify 通道通知测试。
	calls chan struct{}
	// index 是下一次拨号的位置，只由主循环单协程访问。
	index int
}

// newScriptedDialer 创建按顺序返回 conns 的拨号器，并在构造时建立通知通道，避免与等待方竞争初始化。
func newScriptedDialer(conns ...WSConn) *scriptedDialer {
	return &scriptedDialer{conns: conns, calls: make(chan struct{}, 64)}
}

// Dial 返回下一条预设连接并通知拨号发生。
func (d *scriptedDialer) Dial(context.Context, ws.Config, *slog.Logger) (WSConn, error) {
	// conn 是本次拨号返回的连接。
	conn := d.conns[min(d.index, len(d.conns)-1)]
	d.index++
	d.calls <- struct{}{}
	return conn, nil
}

// waitFor 在有限时间内等待拨号次数达到 want。
func (d *scriptedDialer) waitFor(t *testing.T, want int) {
	t.Helper()
	// deadline 是等待拨号的整体预算。
	deadline := time.After(5 * time.Second)
	// received 表示已收到的拨号通知数量
	for received := 0; received < want; received++ {
		select {
		case <-d.calls:
		case <-deadline:
			t.Fatalf("拨号次数未达到 %d", want)
		}
	}
}

// TestRunStopsWhenAccountDisabledBetweenReconnects 验证重连前发现账号被禁用时循环正常结束。
func TestRunStopsWhenAccountDisabledBetweenReconnects(t *testing.T) {
	// acc、store、cleanup 是带数据库的账号、存储及释放函数。
	acc, _, store, cleanup := newRunAccount(t, &fakeRunMtop{token: "tok"})
	defer cleanup()
	// first 在接收阶段禁用账号并立即断线。
	first := &fakeWSConn{onReceive: func(func(map[string]any)) { store.Cookies.SetStatus(context.Background(), "cid", false) }}
	acc.wsDialer = &fakeDialer{results: []dialResult{{conn: first}}}
	if err := runAccountUntilDone(t, acc, context.Background()); err != nil {
		t.Fatalf("账号被禁用后应正常结束: %v", err)
	}
}

// TestRunRegisterOutcomesWithCancellation 验证注册期间凭证变化与注册失败在上下文取消时的退出。
func TestRunRegisterOutcomesWithCancellation(t *testing.T) {
	// 注册期间凭证被替换并取消上下文：循环重载后在顶部退出。
	acc, _, store, cleanup := newRunAccount(t, &fakeRunMtop{token: "tok"})
	defer cleanup()
	// ctx、cancel 控制 Run。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// replacing 在注册时改写数据库 Cookie 并取消上下文。
	replacing := &scriptedWSConn{fakeWSConn: &fakeWSConn{recvBlock: true}}
	replacing.onRegister = func() error {
		_ = store.Cookies.UpdateValueExisting(context.Background(), "cid", "unb=123; _m_h5_tk=tk_changed;")
		cancel()
		return nil
	}
	acc.wsDialer = newScriptedDialer(replacing)
	if err := runAccountUntilDone(t, acc, ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("凭证变化后取消应返回 context.Canceled: %v", err)
	}

	// 注册失败同时上下文已取消：返回取消错误而不是注册错误。
	acc2, _, _, cleanup2 := newRunAccount(t, &fakeRunMtop{token: "tok"})
	defer cleanup2()
	// ctx2、cancel2 控制第二个账号的 Run。
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	// failing 的注册取消上下文并返回错误。
	failing := &scriptedWSConn{fakeWSConn: &fakeWSConn{recvBlock: true}}
	failing.onRegister = func() error { cancel2(); return errors.New("register boom") }
	acc2.wsDialer = newScriptedDialer(failing)
	if err := runAccountUntilDone(t, acc2, ctx2); !errors.Is(err, context.Canceled) {
		t.Fatalf("注册失败且已取消应返回 context.Canceled: %v", err)
	}
}

// TestRunDialFailureWithCanceledContext 验证拨号失败时上下文已取消则返回取消错误。
func TestRunDialFailureWithCanceledContext(t *testing.T) {
	// acc、cleanup 是带数据库的账号及其释放函数。
	acc, _, _, cleanup := newRunAccount(t, &fakeRunMtop{token: "tok"})
	defer cleanup()
	// ctx、cancel 控制 Run。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	acc.wsDialer = cancelingDialer{cancel: cancel}
	if err := runAccountUntilDone(t, acc, ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("拨号失败且已取消应返回 context.Canceled: %v", err)
	}
}

// cancelingDialer 在拨号时取消运行上下文并返回错误，模拟关闭期间的握手失败。
type cancelingDialer struct {
	// cancel 取消主循环的运行上下文。
	cancel context.CancelFunc
}

// Dial 取消上下文后返回握手错误。
func (d cancelingDialer) Dial(context.Context, ws.Config, *slog.Logger) (WSConn, error) {
	d.cancel()
	return nil, errors.New("dial after cancel")
}

// TestRunRenewCancelsContextBeforeConnect 验证首次接口续期期间上下文被取消时 Run 返回取消错误。
func TestRunRenewCancelsContextBeforeConnect(t *testing.T) {
	// acc、cleanup 是带数据库的账号及其释放函数。
	acc, _, store, cleanup := newRunAccount(t, &fakeRunMtop{token: "tok"})
	defer cleanup()
	// ctx、cancel 控制 Run。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// initial 是带登录有效期标记的 Cookie，使接口续期真正发起请求。
	initial := "unb=123; havana_lgc_exp=" + fmt.Sprint(time.Now().Add(time.Hour).UnixMilli())
	if err := store.Cookies.UpdateValueExisting(context.Background(), "cid", initial); err != nil {
		t.Fatal(err)
	}
	acc.replaceCookieStr(initial)
	acc.wsDialer = newScriptedDialer(&fakeWSConn{recvBlock: true})
	// srv 在收到续期请求时取消运行上下文。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cancel()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	acc.renewer = renew.Service{HTTPClient: srv.Client(), SilentHasLoginURL: srv.URL, RetryDelay: -1}
	if err := runAccountUntilDone(t, acc, ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("续期期间取消应返回 context.Canceled: %v", err)
	}
}

// TestMarkConnectionOnlinePublishesRecovery 验证离线周期结束后的新连接清除离线标记并发布恢复事件。
func TestMarkConnectionOnlinePublishesRecovery(t *testing.T) {
	// acc、cleanup 是带数据库的账号及其释放函数。
	acc, _, _, cleanup := newRunAccount(t, &fakeRunMtop{token: "tok"})
	defer cleanup()
	acc.runtimeMu.Lock()
	acc.offlineNotified = true
	acc.offlineSince = time.Now().Add(-time.Minute)
	acc.runtimeMu.Unlock()
	acc.connection.markConnectionOnline(context.Background(), &fakeWSConn{})
	acc.runtimeMu.Lock()
	// notified 是恢复后离线通知标记的快照。
	notified := acc.offlineNotified
	acc.runtimeMu.Unlock()
	if notified || acc.RuntimeStatus().State != RuntimeOnline {
		t.Fatal("恢复在线后应清除离线标记并进入 online")
	}
}

// TestRunConnectionSessionRotatesWhenRefreshAtIsZero 验证未提供轮换时间的会话立即主动轮换。
func TestRunConnectionSessionRotatesWhenRefreshAtIsZero(t *testing.T) {
	// account 是用于直接调用会话组件的账号。
	account := New(Config{CookieID: "rotate-zero", CookieStr: "unb=1"})
	// conn 的 Close 会令接收循环结束。
	conn := &fakeWSConn{recvBlock: true, closeCh: make(chan struct{})}
	// result 是会话收束结果；零值 refreshAt 应触发立即轮换。
	result := account.runConnectionSession(context.Background(), conn, time.Time{})
	if !result.Rotated {
		t.Fatal("零值轮换时间应立即主动轮换")
	}
}

// TestPauseBeforeReconnectBacksOffRapidDisconnects 验证“连上即断”时第一次立即重连、连续第二次起退避，
// 持续时间超过阈值的会话会清零计数，且取消的 Context 会中断等待。
func TestPauseBeforeReconnectBacksOffRapidDisconnects(t *testing.T) {
	account, _, _, cleanup := newAccountForTest(t)
	defer cleanup()
	var delays []int
	account.dialRetryDelayFn = func(n int) time.Duration {
		delays = append(delays, n)
		return 0
	}
	coordinator := &connectionCoordinator{account: account}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := coordinator.pauseBeforeReconnect(ctx, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if len(delays) != 2 || delays[0] != 1 || delays[1] != 2 {
		t.Fatalf("第一次短连接应立即重连，之后按次数退避，got %v", delays)
	}
	if err := coordinator.pauseBeforeReconnect(ctx, ShortConnectionThreshold); err != nil {
		t.Fatal(err)
	}
	if account.rapidReconnects != 0 {
		t.Fatalf("长连接后计数应清零，got %d", account.rapidReconnects)
	}
	if err := coordinator.pauseBeforeReconnect(ctx, 0); err != nil || len(delays) != 2 {
		t.Fatalf("清零后的第一次短连接不应等待: err=%v delays=%v", err, delays)
	}
	account.dialRetryDelayFn = func(int) time.Duration { return time.Hour }
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := coordinator.pauseBeforeReconnect(canceled, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消的 Context 应中断退避，got %v", err)
	}
}

// TestBackoffDelayJitterIsBounded 验证未注入覆盖时 backoffDelay 使用拨号退避序列，抖动不超过 30%。
func TestBackoffDelayJitterIsBounded(t *testing.T) {
	account, _, _, cleanup := newAccountForTest(t)
	defer cleanup()
	for n := 1; n <= len(dialBackoffSteps)+1; n++ {
		base := dialBackoff(n)
		if got := account.backoffDelay(n, false); got != base {
			t.Fatalf("n=%d 无抖动退避 %s want %s", n, got, base)
		}
		if got := account.backoffDelay(n, true); got < base || got > base+base*3/10 {
			t.Fatalf("n=%d 抖动后的退避 %s 超出 [%s, %s]", n, got, base, base+base*3/10)
		}
	}
}
