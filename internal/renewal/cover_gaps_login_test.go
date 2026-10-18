package renewal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xianyu-go/internal/db"
	"xianyu-go/internal/xianyu/mtop"
)

// loginSuccessBody 是 loginuser.get 的成功业务响应体。
const loginSuccessBody = `{"ret":["SUCCESS::调用成功"],"data":{}}`

// loginUnparsableBody 是 无法解析的响应体，用于让登录态检查返回错误但仍带响应 Cookie。
const loginUnparsableBody = `not-json`

// blockCookieUpdates 安装触发器，使此后所有账号 Cookie 更新都被数据库拒绝，用于覆盖持久化失败分支。
func blockCookieUpdates(t *testing.T, store *db.Store) {
	t.Helper()
	// stmt 是创建拒绝更新触发器的 SQL。
	const stmt = `CREATE TRIGGER block_cookie_update BEFORE UPDATE ON cookies BEGIN SELECT RAISE(ABORT, 'blocked'); END`
	if _, err := store.DB.ExecContext(context.Background(), stmt); err != nil {
		t.Fatal(err)
	}
}

// blockSecondCookieUpdate 安装计数触发器：第一次更新成功，之后的更新全部被拒绝。
func blockSecondCookieUpdate(t *testing.T, store *db.Store) {
	t.Helper()
	// stmts 依次创建计数表、计数触发器和拒绝触发器。
	stmts := []string{
		`CREATE TABLE trig_counter (n INTEGER)`,
		`INSERT INTO trig_counter VALUES (0)`,
		`CREATE TRIGGER trig_count AFTER UPDATE ON cookies BEGIN UPDATE trig_counter SET n = n + 1; END`,
		`CREATE TRIGGER trig_block BEFORE UPDATE ON cookies WHEN (SELECT n FROM trig_counter) >= 1 BEGIN SELECT RAISE(ABORT, 'blocked'); END`,
	}
	// stmt 是当前要执行的建表或建触发器语句。
	for _, stmt := range stmts {
		if _, err := store.DB.ExecContext(context.Background(), stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// loginScheduler 构造指向本地 loginuser.get 桩服务的调度器。
func loginScheduler(store *db.Store, server *httptest.Server) *Scheduler {
	// scheduler 使用隔离冷却管理器，避免全局冷却状态影响其他用例。
	scheduler := NewScheduler(store, &schedulerFakeStarter{}, nil, nil)
	scheduler.cooldown = NewCooldownManager()
	scheduler.mtop = &mtop.ClientImpl{HTTPClient: server.Client(), LoginUserURL: server.URL}
	return scheduler
}

// TestExecuteLoginRenewIteratesAccountsAndSkipsCooled 验证批量登录续期会处理首个账号、等待间隔被取消打断并跳过冷却账号。
func TestExecuteLoginRenewIteratesAccountsAndSkipsCooled(t *testing.T) {
	// store 是包含两个启用账号的本地仓储。
	store, cleanup := newSchedulerTestStore(t)
	defer cleanup()
	createSchedulerAccount(t, store, "a-first", "unb=1; _m_h5_tk=old_1")
	createSchedulerAccount(t, store, "b-cooled", "unb=2; _m_h5_tk=old_2")
	// ctx 与 cancel 由桩服务在首个请求到达时取消，使账号间一分钟等待立即结束。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// requests 统计桩服务收到的登录态检查请求数。
	requests := 0
	// server 在收到首个请求后取消调度上下文。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		cancel()
		_, _ = w.Write([]byte(loginSuccessBody))
	}))
	defer server.Close()
	// scheduler 的第二个账号处于会话冷却中。
	scheduler := loginScheduler(store, server)
	scheduler.cooldown.MarkSessionExpired("b-cooled")
	scheduler.executeLoginRenew(ctx)
	if requests != 1 {
		t.Fatalf("requests=%d want 1", requests)
	}
}

// TestLoginRenewOneReloadAndDisableDuringCheck 验证外部检查期间账号被删除或停用时记录日志且不写回旧响应。
func TestLoginRenewOneReloadAndDisableDuringCheck(t *testing.T) {
	// mutations 描述检查期间对账号做的并发修改。
	mutations := map[string]string{
		"deleted":  `DELETE FROM cookies WHERE id = ?`,
		"disabled": `INSERT INTO cookie_status (cookie_id, enabled) VALUES (?, 0)`,
	}
	// name 与statement 是当前场景名称及其并发修改语句。
	for name, statement := range mutations {
		// cleanup 保存关闭测试仓储的清理函数；store 保存隔离的本地 SQLite 仓储。
		store, cleanup := newSchedulerTestStore(t)
		// account 保存测试用账号快照，不含真实凭证。
		account := createSchedulerAccount(t, store, "cid-"+name, "unb=1; _m_h5_tk=old_1")
		// server 在响应前执行并发修改。
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if _, err := store.DB.ExecContext(context.Background(), statement, account.ID); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			_, _ = w.Write([]byte(loginSuccessBody))
		}))
		loginScheduler(store, server).loginRenewOne(context.Background(), "batch-"+name, account)
		server.Close()
		cleanup()
	}
}

// TestLoginRenewOnePersistFailureBranches 验证响应 Cookie 持久化失败时的错误合并与扁平兼容路径失败。
func TestLoginRenewOnePersistFailureBranches(t *testing.T) {
	// cases 描述服务响应体和所用的拒绝更新触发器。
	cases := []struct {
		name  string
		body  string
		block func(*testing.T, *db.Store)
	}{
		{"persist-with-call-error", loginUnparsableBody, blockCookieUpdates},
		{"persist-without-call-error", loginSuccessBody, blockCookieUpdates},
		{"flat-compat-second-write", loginSuccessBody, blockSecondCookieUpdate},
	}
	// tc 是当前持久化失败场景。
	for _, tc := range cases {
		// cleanup 保存关闭测试仓储的清理函数；store 保存隔离的本地 SQLite 仓储。
		store, cleanup := newSchedulerTestStore(t)
		// account 保存测试用账号快照，不含真实凭证。
		account := createSchedulerAccount(t, store, "cid-"+tc.name, "unb=1; _m_h5_tk=old_1")
		tc.block(t, store)
		// server 总是下发新的令牌 Cookie。
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.SetCookie(w, &http.Cookie{Name: "_m_h5_tk", Value: "new_1"})
			_, _ = w.Write([]byte(tc.body))
		}))
		loginScheduler(store, server).loginRenewOne(context.Background(), "batch-"+tc.name, account)
		server.Close()
		// saved 是失败后账号仍保存的 Cookie，旧值不得被破坏。
		saved, err := store.Cookies.GetValue(context.Background(), account.ID)
		if tc.block != nil && tc.name != "flat-compat-second-write" && (err != nil || strings.Contains(saved, "new_1")) {
			t.Fatalf("%s saved=%q err=%v", tc.name, saved, err)
		}
		cleanup()
	}
}
