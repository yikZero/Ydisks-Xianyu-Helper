package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

// fakePgxConn 是实现全部可选 driver 接口的假连接，记录最后收到的 SQL 以验证占位符改写与透传。
type fakePgxConn struct {
	// lastQuery 保存最近一次收到的（已改写）SQL。
	lastQuery string
	// valid 是 IsValid 的返回值。
	valid bool
	// pingErr 是 Ping 返回的错误。
	pingErr error
	// resetCalled 记录 ResetSession 是否被透传。
	resetCalled bool
}

// Prepare 实现 driver.Conn，测试中不会使用。
func (f *fakePgxConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }

// Close 实现 driver.Conn。
func (f *fakePgxConn) Close() error { return nil }

// Begin 实现 driver.Conn，测试中不会使用。
func (f *fakePgxConn) Begin() (driver.Tx, error) { return nil, errors.New("unused") }

// ExecContext 记录 SQL 并返回空结果。
func (f *fakePgxConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	f.lastQuery = query
	return driver.RowsAffected(1), nil
}

// QueryContext 记录 SQL 并返回 nil 行集。
func (f *fakePgxConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	f.lastQuery = query
	return nil, nil
}

// PrepareContext 记录 SQL 并返回 nil 语句。
func (f *fakePgxConn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	f.lastQuery = query
	return nil, nil
}

// BeginTx 返回 nil 事务。
func (f *fakePgxConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) { return nil, nil }

// Ping 返回预置错误。
func (f *fakePgxConn) Ping(context.Context) error { return f.pingErr }

// IsValid 返回预置有效性。
func (f *fakePgxConn) IsValid() bool { return f.valid }

// CheckNamedValue 接受任意参数。
func (f *fakePgxConn) CheckNamedValue(*driver.NamedValue) error { return nil }

// ResetSession 记录被调用。
func (f *fakePgxConn) ResetSession(context.Context) error {
	f.resetCalled = true
	return nil
}

// barePgxConn 只实现必需的 driver.Conn，用于覆盖可选接口缺失时的降级分支。
type barePgxConn struct{}

// Prepare 实现 driver.Conn。
func (barePgxConn) Prepare(string) (driver.Stmt, error) { return nil, nil }

// Close 实现 driver.Conn。
func (barePgxConn) Close() error { return nil }

// Begin 实现 driver.Conn。
func (barePgxConn) Begin() (driver.Tx, error) { return nil, nil }

// fakePgxDriver 是返回固定连接或错误的假 driver，同时充当 connector。
type fakePgxDriver struct {
	// conn 是 Open/Connect 成功时返回的连接。
	conn driver.Conn
	// err 非空时 Open/Connect 失败。
	err error
}

// Open 实现 driver.Driver。
func (d *fakePgxDriver) Open(string) (driver.Conn, error) { return d.conn, d.err }

// Connect 实现 driver.Connector。
func (d *fakePgxDriver) Connect(context.Context) (driver.Conn, error) { return d.conn, d.err }

// Driver 实现 driver.Connector。
func (d *fakePgxDriver) Driver() driver.Driver { return d }

// TestQConnDelegatesWithRewrittenPlaceholders 验证包装连接把 ? 改写为 $N 后透传，并原样透传其它可选接口。
func TestQConnDelegatesWithRewrittenPlaceholders(t *testing.T) {
	// fake 是被包装的底层假连接。
	fake := &fakePgxConn{valid: true}
	// conn 是被测包装连接。
	conn := &qConn{Conn: fake}
	// ctx 是调用上下文。
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, "UPDATE t SET a=? WHERE b=?", nil); err != nil || fake.lastQuery != "UPDATE t SET a=$1 WHERE b=$2" {
		t.Fatalf("exec query=%q err=%v", fake.lastQuery, err)
	}
	if _, err := conn.QueryContext(ctx, "SELECT ?", nil); err != nil || fake.lastQuery != "SELECT $1" {
		t.Fatalf("query=%q err=%v", fake.lastQuery, err)
	}
	if _, err := conn.PrepareContext(ctx, "SELECT '?' , ?"); err != nil || fake.lastQuery != "SELECT '?' , $1" {
		t.Fatalf("prepare=%q err=%v", fake.lastQuery, err)
	}
	if _, err := conn.BeginTx(ctx, driver.TxOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	fake.pingErr = errors.New("ping failed")
	if err := conn.Ping(ctx); err == nil {
		t.Fatal("ping 错误应被透传")
	}
	if !conn.IsValid() {
		t.Fatal("底层有效时应有效")
	}
	if err := conn.CheckNamedValue(&driver.NamedValue{}); err != nil {
		t.Fatal(err)
	}
	if err := conn.ResetSession(ctx); err != nil || !fake.resetCalled {
		t.Fatalf("reset called=%v err=%v", fake.resetCalled, err)
	}
}

// TestQConnFallsBackWhenOptionalInterfacesMissing 验证底层连接缺少可选接口时的降级行为。
func TestQConnFallsBackWhenOptionalInterfacesMissing(t *testing.T) {
	// conn 包装只实现必需接口的连接。
	conn := &qConn{Conn: barePgxConn{}}
	// ctx 是调用上下文。
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, "x", nil); !errors.Is(err, driver.ErrSkip) {
		t.Fatalf("exec err=%v", err)
	}
	if _, err := conn.QueryContext(ctx, "x", nil); !errors.Is(err, driver.ErrSkip) {
		t.Fatalf("query err=%v", err)
	}
	if _, err := conn.PrepareContext(ctx, "x"); !errors.Is(err, driver.ErrSkip) {
		t.Fatalf("prepare err=%v", err)
	}
	if _, err := conn.BeginTx(ctx, driver.TxOptions{}); !errors.Is(err, driver.ErrSkip) {
		t.Fatalf("begin err=%v", err)
	}
	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("ping err=%v", err)
	}
	if !conn.IsValid() {
		t.Fatal("缺少 Validator 时应视为有效")
	}
	if err := conn.CheckNamedValue(&driver.NamedValue{}); !errors.Is(err, driver.ErrSkip) {
		t.Fatalf("check err=%v", err)
	}
	if err := conn.ResetSession(ctx); err != nil {
		t.Fatalf("reset err=%v", err)
	}
}

// TestQPgDriverAndConnectorWrapBase 验证 driver/connector 在成功与失败时的包装与错误透传。
func TestQPgDriverAndConnectorWrapBase(t *testing.T) {
	// ok 是总是成功的底层 driver。
	ok := &fakePgxDriver{conn: barePgxConn{}}
	// bad 是总是失败的底层 driver。
	bad := &fakePgxDriver{err: errors.New("boom")}
	// c、err 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
	if c, err := (&qPgDriver{base: ok}).Open("dsn"); err != nil || c == nil {
		t.Fatalf("open c=%v err=%v", c, err)
	}
	if _, err := (&qPgDriver{base: bad}).Open("dsn"); err == nil {
		t.Fatal("Open 错误应透传")
	}
	// c、err 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
	if c, err := (&qPgConnector{base: ok}).Connect(context.Background()); err != nil || c == nil {
		t.Fatalf("connect c=%v err=%v", c, err)
	}
	if _, err := (&qPgConnector{base: bad}).Connect(context.Background()); err == nil {
		t.Fatal("Connect 错误应透传")
	}
	if (&qPgConnector{base: ok}).Driver() == nil {
		t.Fatal("Driver 不应为空")
	}
}

// TestNewPgxCompatConnectorParsesDSN 验证 DSN 解析（含 pgx:// 别名、UTC 时区注入、非法 DSN）及对不可达地址的连接失败。
func TestNewPgxCompatConnectorParsesDSN(t *testing.T) {
	if _, err := newPgxCompatConnector("::not a dsn::\x00"); err == nil {
		t.Fatal("非法 DSN 应报错")
	}
	// config 是 pgx:// 别名解析后的配置。
	config, err := parsePgxConfig("pgx://user:pw@127.0.0.1:1/db?sslmode=disable")
	if err != nil || config.RuntimeParams["timezone"] != "UTC" || config.Host != "127.0.0.1" {
		t.Fatalf("config=%+v err=%v", config, err)
	}
	// connector 指向本机未监听端口，仅用于验证连接失败被透传。
	connector, err := newPgxCompatConnector("postgres://user:pw@127.0.0.1:1/db?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	// ctx 限制连接尝试时长。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := connector.Connect(ctx); err == nil {
		t.Fatal("不可达地址应连接失败")
	}
	if connector.Driver() == nil {
		t.Fatal("Driver 不应为空")
	}
}
