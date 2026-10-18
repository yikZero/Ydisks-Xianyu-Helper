package renewal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/cookierefresh"
	apirenew "xianyu-go/internal/xianyu/renew"
)

// apiSuccessBody 是官网静默续期业务成功的响应体。
const apiSuccessBody = `{"content":{"data":{"processFinished":true,"resultCode":100}}}`

// apiFailureBody 是官网静默续期业务失败（结果码非 100）的响应体。
const apiFailureBody = `{"content":{"data":{"processFinished":true,"resultCode":500}}}`

// apiScheduler 构造指向本地续期桩服务的调度器，禁用请求间重试等待。
func apiScheduler(store *db.Store, server *httptest.Server, starter AccountStarter) *Scheduler {
	// scheduler 使用隔离冷却管理器。
	scheduler := NewScheduler(store, starter, nil, nil)
	scheduler.cooldown = NewCooldownManager()
	scheduler.api = apirenew.Service{HTTPClient: server.Client(), SilentHasLoginURL: server.URL, RetryDelay: -1}
	return scheduler
}

// apiCookie 构造带未来长登录到期时间的账号 Cookie，使续期选择静默续期而不是跳过。
func apiCookie() string {
	return "unb=1; havana_lgc_exp=" + futureSchedulerMillis()
}

// TestExecuteAPICookieRenewIteratesAccounts 验证批量接口续期遍历账号，并在间隔等待被取消后不再发起后续请求。
func TestExecuteAPICookieRenewIteratesAccounts(t *testing.T) {
	// store 是包含两个启用账号的本地仓储。
	store, cleanup := newSchedulerTestStore(t)
	defer cleanup()
	createSchedulerAccount(t, store, "a-first", apiCookie())
	createSchedulerAccount(t, store, "b-second", apiCookie())
	// ctx 由桩服务在首个请求到达时取消。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// requests 统计桩服务收到的续期请求数。
	requests := 0
	// server 在收到请求后取消上下文并返回失败码。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		cancel()
		_, _ = w.Write([]byte(apiFailureBody))
	}))
	defer server.Close()
	apiScheduler(store, server, nil).executeAPICookieRenew(ctx)
	if requests != 1 {
		t.Fatalf("requests=%d want 1", requests)
	}
}

// TestAPIRenewDisabledAndMutationDuringCall 验证账号在续期前后被停用/删除、或外部请求期间凭证被并发改写时的日志语义。
func TestAPIRenewDisabledAndMutationDuringCall(t *testing.T) {
	ctx := context.Background()
	// cases 描述每个场景在请求期间对账号做的修改与期望日志状态（空串表示不写日志）。
	cases := []struct {
		name      string
		beforeSQL string
		duringSQL string
		wantLog   string
	}{
		{"disabled-before", `INSERT INTO cookie_status (cookie_id, enabled) VALUES (?, 0)`, "", ""},
		{"deleted-during", "", `DELETE FROM cookies WHERE id = ?`, "failed"},
		{"disabled-during", "", `INSERT INTO cookie_status (cookie_id, enabled) VALUES (?, 0)`, "skipped"},
	}
	// tc 是当前场景。
	for _, tc := range cases {
		// cleanup 保存关闭测试仓储的清理函数；store 保存隔离的本地 SQLite 仓储。
		store, cleanup := newSchedulerTestStore(t)
		// account 保存测试用账号快照，不含真实凭证。
		account := createSchedulerAccount(t, store, "cid-"+tc.name, apiCookie())
		if tc.beforeSQL != "" {
			if _, err := store.DB.ExecContext(ctx, tc.beforeSQL, account.ID); err != nil {
				t.Fatal(err)
			}
		}
		// server 保存本地 httptest 桩服务，只监听回环地址，不访问真实平台。
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tc.duringSQL != "" {
				if _, err := store.DB.ExecContext(ctx, tc.duringSQL, account.ID); err != nil {
					t.Errorf("%s: %v", tc.name, err)
				}
			}
			_, _ = w.Write([]byte(apiSuccessBody))
		}))
		apiScheduler(store, server, nil).apiCookieRenewOne(ctx, "batch-"+tc.name, account)
		server.Close()
		if tc.wantLog != "" {
			// log 保存最近一条续期日志的快照，用于断言终态。
			if log := lastAPIRenewLog(t, store, account.ID); log.status != tc.wantLog {
				t.Fatalf("%s status=%q want %q", tc.name, log.status, tc.wantLog)
			}
		}
		cleanup()
	}
}

// TestAPIRenewConcurrentUpdateWithoutResponseCookies 验证续期期间凭证被并发更新且响应无 Set-Cookie 时，不把旧状态写回。
func TestAPIRenewConcurrentUpdateWithoutResponseCookies(t *testing.T) {
	// store 是本地仓储。
	store, cleanup := newSchedulerTestStore(t)
	defer cleanup()
	// account 是待续期账号。
	account := createSchedulerAccount(t, store, "cid-concurrent", apiCookie())
	// concurrentValue 是并发流程写入的新 Cookie。
	concurrentValue := "unb=1; concurrent=kept; havana_lgc_exp=" + futureSchedulerMillis()
	// server 保存本地 httptest 桩服务，只监听回环地址，不访问真实平台。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := store.Cookies.UpdateRenewalCookie(context.Background(), account.ID, concurrentValue, `{}`, time.Now().Unix()); err != nil {
			t.Errorf("concurrent update: %v", err)
		}
		_, _ = w.Write([]byte(apiSuccessBody))
	}))
	defer server.Close()
	apiScheduler(store, server, nil).apiCookieRenewOne(context.Background(), "batch-concurrent", account)
	// saved 是处理后的账号 Cookie，必须保留并发写入的值。
	saved, err := store.Cookies.GetValue(context.Background(), account.ID)
	if err != nil || !strings.Contains(saved, "concurrent=kept") {
		t.Fatalf("saved=%q err=%v", saved, err)
	}
}

// TestAPIRenewSnapshotRestartAndPersistenceFailures 验证完整 Cookie Jar 续期、账号重启失败、保存失败与请求失败的日志。
func TestAPIRenewSnapshotRestartAndPersistenceFailures(t *testing.T) {
	ctx := context.Background()
	// snapshotMetadata 是带完整 Cookie Jar 的账号 metadata。
	snapshotMetadata := func() string {
		return cookierefresh.MetadataWithSnapshot(`{}`, []cookierefresh.BrowserCookie{
			{Name: "unb", Value: "1", Domain: ".goofish.com", Path: "/", Secure: true},
			{Name: "havana_lgc_exp", Value: futureSchedulerMillis(), Domain: ".goofish.com", Path: "/", Secure: true},
		})
	}
	// cases 描述每个场景的响应、是否带快照、启动器与持久化故障，以及期望日志状态。
	cases := []struct {
		name     string
		body     string
		cookie   bool
		snapshot bool
		starter  AccountStarter
		block    bool
		wantLog  string
	}{
		{"restart-failure", apiSuccessBody, true, true, &schedulerContextStarter{err: context.DeadlineExceeded}, false, "failed"},
		{"flat-restart-ok", apiSuccessBody, true, false, &schedulerFakeStarter{}, false, "cookie_updated"},
		{"save-failure", apiSuccessBody, true, false, nil, true, "failed"},
		{"business-failure", apiFailureBody, false, false, nil, false, "failed"},
	}
	// tc 是当前场景。
	for _, tc := range cases {
		// cleanup 保存关闭测试仓储的清理函数；store 保存隔离的本地 SQLite 仓储。
		store, cleanup := newSchedulerTestStore(t)
		// account 保存测试用账号快照，不含真实凭证。
		account := createSchedulerAccount(t, store, "cid-"+tc.name, apiCookie())
		if tc.snapshot {
			if err := store.Cookies.UpdateRenewalCookie(ctx, account.ID, account.Value, snapshotMetadata(), time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
			account.MetadataJSON = snapshotMetadata()
		}
		if tc.block {
			blockCookieUpdates(t, store)
		}
		// server 保存本地 httptest 桩服务，只监听回环地址，不访问真实平台。
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if tc.cookie {
				http.SetCookie(w, &http.Cookie{Name: "sdkSilent", Value: futureSchedulerMillis(), Path: "/"})
			}
			_, _ = w.Write([]byte(tc.body))
		}))
		apiScheduler(store, server, tc.starter).apiCookieRenewOne(ctx, "batch-"+tc.name, account)
		server.Close()
		// log 保存最近一条续期日志的快照，用于断言终态。
		if log := lastAPIRenewLog(t, store, account.ID); log.status != tc.wantLog {
			t.Fatalf("%s status=%q want %q (%+v)", tc.name, log.status, tc.wantLog, log)
		}
		cleanup()
	}
	// 请求失败：服务已关闭，续期返回结果与错误并写失败日志。
	store, cleanup := newSchedulerTestStore(t)
	defer cleanup()
	// account 保存测试用账号快照，不含真实凭证。
	account := createSchedulerAccount(t, store, "cid-request-failure", apiCookie())
	// server 保存本地 httptest 桩服务，只监听回环地址，不访问真实平台。
	server := httptest.NewServer(http.NotFoundHandler())
	// scheduler 保存被测续期调度器。
	scheduler := apiScheduler(store, server, nil)
	server.Close()
	scheduler.apiCookieRenewOne(ctx, "batch-request-failure", account)
	// log 保存最近一条续期日志的快照，用于断言终态。
	if log := lastAPIRenewLog(t, store, account.ID); log.status != "failed" {
		t.Fatalf("status=%q", log.status)
	}
}

// pendingMode 描述迟到响应桩服务在放行后的行为。
type pendingMode int

const (
	// pendingCookieBody 放行后下发 Cookie 且返回完整成功正文。
	pendingCookieBody pendingMode = iota
	// pendingNoCookieBody 放行后不下发 Cookie，只返回完整成功正文。
	pendingNoCookieBody
	// pendingCookieTruncated 放行后下发 Cookie 但正文被截断，使底层响应读取失败。
	pendingCookieTruncated
)

// TestPendingAPIRenewLateResponseOutcomes 验证迟到响应在账号缺失、并发冲突、无变化、保存失败与正文读取失败时的最终日志。
func TestPendingAPIRenewLateResponseOutcomes(t *testing.T) {
	ctx := context.Background()
	// cases 描述放行前对账号做的修改、服务行为与期望的日志片段。
	cases := []struct {
		name    string
		mode    pendingMode
		prepare func(*testing.T, *db.Store, string)
		want    string
	}{
		{"account-missing", pendingCookieBody, func(t *testing.T, s *db.Store, id string) {
			if _, err := s.DB.ExecContext(ctx, `DELETE FROM cookies WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"conflict", pendingCookieBody, func(t *testing.T, s *db.Store, id string) {
			if err := s.Cookies.UpdateRenewalCookie(ctx, id, "unb=1; sdkSilent=1; havana_lgc_exp="+futureSchedulerMillis(), `{}`, time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
		}, "并发凭证更新"},
		{"unchanged", pendingNoCookieBody, nil, ""},
		{"save-failure", pendingCookieBody, func(t *testing.T, s *db.Store, _ string) { blockCookieUpdates(t, s) }, ""},
		{"truncated-body", pendingCookieTruncated, nil, ""},
	}
	// tc 是当前场景。
	for _, tc := range cases {
		// cleanup 保存关闭测试仓储的清理函数；store 保存隔离的本地 SQLite 仓储。
		store, cleanup := newSchedulerTestStore(t)
		// account 保存测试用账号快照，不含真实凭证。
		account := createSchedulerAccount(t, store, "cid-"+tc.name, apiCookie())
		// release 在续期调用返回 pending 后关闭，放行桩服务的响应。
		release := make(chan struct{})
		// server 保存本地 httptest 桩服务，只监听回环地址，不访问真实平台。
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			<-release
			if tc.mode != pendingNoCookieBody {
				http.SetCookie(w, &http.Cookie{Name: "sdkSilent", Value: futureSchedulerMillis(), Path: "/"})
			}
			if tc.mode == pendingCookieTruncated {
				w.Header().Set("Content-Length", "500")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("{"))
				return
			}
			_, _ = w.Write([]byte(apiSuccessBody))
		}))
		// scheduler 保存被测续期调度器。
		scheduler := apiScheduler(store, server, nil)
		scheduler.api.PromiseTimeout = time.Millisecond
		scheduler.apiCookieRenewOne(ctx, "batch-"+tc.name, account)
		if tc.prepare != nil {
			tc.prepare(t, store, account.ID)
		}
		close(release)
		scheduler.watchers.Wait()
		server.Close()
		if tc.want != "" {
			// log 保存最近一条续期日志的快照，用于断言终态。
			if log := lastAPIRenewLog(t, store, account.ID); !strings.Contains(log.errorMessage, tc.want) {
				t.Fatalf("%s log=%+v", tc.name, log)
			}
		}
		cleanup()
	}
}

// TestWatchPendingAPIRenewIgnoresMissingPending 验证没有待处理底层响应或缺少仓储时不启动观察协程。
func TestWatchPendingAPIRenewIgnoresMissingPending(t *testing.T) {
	// scheduler 的仓储为空。
	scheduler := NewScheduler(nil, nil, nil, nil)
	scheduler.watchPendingAPIRenew(context.Background(), "b", "c", nil, "", "")
	scheduler.watchPendingAPIRenew(context.Background(), "b", "c", &apirenew.Result{}, "", "")
	scheduler.watchers.Wait()
}

// TestAddAPILogNotificationReasonFallbacks 验证连续失败告警在错误信息缺失时依次回退到消息文本和未知错误。
func TestAddAPILogNotificationReasonFallbacks(t *testing.T) {
	// store 是本地仓储。
	store, cleanup := newSchedulerTestStore(t)
	defer cleanup()
	// notifier 记录告警正文。
	notifier := &schedulerFakeNotifier{}
	// scheduler 保存被测续期调度器。
	scheduler := NewScheduler(store, nil, nil, nil, notifier)
	// fallbacks 描述日志字段组合与告警正文应包含的原因。
	fallbacks := []struct {
		cookieID string
		message  string
		want     string
	}{
		{"cid-message", "消息原因", "消息原因"},
		{"cid-unknown", "", "未知错误"},
	}
	// fb 是当前日志字段组合。
	for _, fb := range fallbacks {
		for range 3 {
			scheduler.addAPILog(context.Background(), db.RenewalLog{BatchID: "b", CookieID: fb.cookieID, Status: "failed", Message: fb.message})
		}
		if !strings.Contains(notifier.message, fb.want) {
			t.Fatalf("message=%q want %q", notifier.message, fb.want)
		}
	}
}

// TestSchedulerCleanupRunAndSleepEdges 覆盖仓储缺失时日志清理、Run 参数防御与到时的可取消等待。
func TestSchedulerCleanupRunAndSleepEdges(t *testing.T) {
	// emptyStoreScheduler 持有没有续期仓储的空 Store。
	emptyStoreScheduler := NewScheduler(&db.Store{}, nil, nil, nil)
	emptyStoreScheduler.cleanupExpiredLogs(context.Background())
	// nilScheduler 保存空调度器指针，用于验证空接收者调用不会 panic。
	var nilScheduler *Scheduler
	nilScheduler.Run(context.Background())
	NewScheduler(nil, nil, nil, nil).Run(context.Background())
	// store 是真实本地仓储，用于验证空 Context 不会启动调度。
	store, cleanup := newSchedulerTestStore(t)
	defer cleanup()
	// nilCtx 模拟缺少生命周期 Context 的调用。
	var nilCtx context.Context
	NewScheduler(store, nil, nil, nil).Run(nilCtx)
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("err=%v", err)
	}
}

// TestRunFixedAndAPIFixedExecuteAfterTimer 验证固定周期任务在定时器触发后重新执行，并在任务内取消后退出。
func TestRunFixedAndAPIFixedExecuteAfterTimer(t *testing.T) {
	// store 保存启用开关与极短周期设置。
	store, cleanup := newSchedulerTestStore(t)
	defer cleanup()
	// ctx 与 cancel 控制固定循环退出。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// settings 是开关与周期（秒）设置。
	settings := map[string]string{
		"renewal.test.enabled": "true", "renewal.test.interval": "0.001",
		apiCookieRenewEnabledSetting: "true", apiCookieRenewIntervalSetting: "0.001",
	}
	// key 与 value 是当前设置项。
	for key, value := range settings {
		if err := store.Settings.Set(ctx, key, value); err != nil {
			t.Fatal(err)
		}
	}
	// scheduler 保存被测续期调度器。
	scheduler := NewScheduler(store, nil, nil, nil)
	// runs 统计固定任务被执行的次数，第二次执行时取消循环。
	runs := 0
	scheduler.runFixed(ctx, "test", "renewal.test.enabled", "renewal.test.interval", false, time.Hour, func(context.Context) {
		runs++
		if runs == 2 {
			cancel()
		}
	})
	if runs != 2 {
		t.Fatalf("runs=%d", runs)
	}
	// apiCtx 与 apiCancel 控制接口续期固定循环；没有账号时每轮都立即结束，取消由首个日志清理后的计时器等待发现。
	apiCtx, apiCancel := context.WithCancel(context.Background())
	defer apiCancel()
	// api 服务只要被访问就取消循环。
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { apiCancel() }))
	defer api.Close()
	createSchedulerAccount(t, store, "cid-fixed", apiCookie())
	apiScheduler(store, api, nil).runAPICookieRenewFixed(apiCtx)
}
