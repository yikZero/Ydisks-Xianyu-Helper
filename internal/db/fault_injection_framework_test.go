package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// errInjectedFault 是故障注入驱动返回的固定错误，用于确认调用方把底层数据库失败原样向上传播。
var errInjectedFault = errors.New("injected database fault")

// faultState 保存一次故障注入的倒计时：remaining 为 n>0 时，第 n 次数据库操作失败。
// 字段使用原子类型，允许连接池的不同 goroutine 并发访问。
type faultState struct {
	// remaining 是距离注入故障还剩的数据库操作次数；0 表示未布防。
	remaining atomic.Int64
	// fired 记录故障是否已经被触发。
	fired atomic.Bool
}

// tick 消耗一次数据库操作额度；额度耗尽时返回注入错误。
func (s *faultState) tick() error {
	if s.remaining.Load() <= 0 {
		return nil
	}
	if s.remaining.Add(-1) == 0 {
		s.fired.Store(true)
		return errInjectedFault
	}
	return nil
}

// faultConnector 包装真实 SQLite 连接器，使每条连接都受同一个 faultState 控制。
type faultConnector struct {
	// base 是真实 SQLite 驱动的连接器。
	base driver.Connector
	// state 是所有连接共享的故障倒计时。
	state *faultState
}

// Connect 建立真实连接并包上故障注入层。
func (c *faultConnector) Connect(ctx context.Context) (driver.Conn, error) {
	// conn、err 是底层真实连接及其错误。
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: conn, state: c.state}, nil
}

// Driver 返回底层驱动。
func (c *faultConnector) Driver() driver.Driver { return c.base.Driver() }

// faultConn 在 Exec/Query/Prepare/Begin 前消耗故障额度。
type faultConn struct {
	driver.Conn
	// state 是共享的故障倒计时。
	state *faultState
}

// ExecContext 先检查故障再透传到底层连接。
func (c *faultConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := c.state.tick(); err != nil {
		return nil, err
	}
	// execer、ok 是底层连接的可选 Exec 能力。
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.ExecContext(ctx, query, args)
}

// QueryContext 先检查故障再透传到底层连接。
func (c *faultConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.state.tick(); err != nil {
		return nil, err
	}
	// queryer、ok 是底层连接的可选 Query 能力。
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return queryer.QueryContext(ctx, query, args)
}

// PrepareContext 先检查故障再透传到底层连接。
func (c *faultConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := c.state.tick(); err != nil {
		return nil, err
	}
	// preparer、ok 是底层连接的可选 Prepare 能力。
	preparer, ok := c.Conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Prepare(query)
	}
	return preparer.PrepareContext(ctx, query)
}

// BeginTx 先检查故障再开启真实事务，并包装事务以便注入提交失败。
func (c *faultConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if err := c.state.tick(); err != nil {
		return nil, err
	}
	// beginner、ok 是底层连接的可选带上下文开启事务能力。
	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return c.Begin() //nolint:staticcheck // 底层驱动未实现 ConnBeginTx 时只能回退到旧接口
	}
	// tx、err 是真实事务及其错误。
	tx, err := beginner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &faultTx{Tx: tx, state: c.state}, nil
}

// CheckNamedValue 透传底层参数转换规则，保持与未包装连接一致。
func (c *faultConn) CheckNamedValue(value *driver.NamedValue) error {
	// checker、ok 是底层连接的可选参数检查能力。
	checker, ok := c.Conn.(driver.NamedValueChecker)
	if !ok {
		return driver.ErrSkip
	}
	return checker.CheckNamedValue(value)
}

// faultTx 在提交前消耗故障额度，回滚永远透传。
type faultTx struct {
	driver.Tx
	// state 是共享的故障倒计时。
	state *faultState
}

// Commit 先检查故障；注入失败时回滚真实事务，模拟提交失败不留下半成品。
func (t *faultTx) Commit() error {
	if err := t.state.tick(); err != nil {
		_ = t.Rollback()
		return err
	}
	return t.Tx.Commit()
}

// dsnConnector 用驱动的 Open 实现 driver.Connector。
type dsnConnector struct {
	// drv 是真实 SQLite 驱动。
	drv driver.Driver
	// dsn 是连接串。
	dsn string
}

// Connect 打开一条新连接。
func (c *dsnConnector) Connect(context.Context) (driver.Conn, error) { return c.drv.Open(c.dsn) }

// Driver 返回底层驱动。
func (c *dsnConnector) Driver() driver.Driver { return c.drv }

// faultTemplate 迁移出一个空 SQLite 库文件，供每次注入迭代复制，避免重复执行迁移。
func faultTemplate(t *testing.T) string {
	t.Helper()
	// path 是模板库文件路径。
	path := filepath.Join(t.TempDir(), "template.db")
	// handle、openErr 是迁移后的临时连接池。
	handle, _, openErr := Open(context.Background(), path)
	if openErr != nil {
		t.Fatal(openErr)
	}
	if closeErr := handle.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	return path
}

// openFaultStore 复制模板库并以故障注入连接器打开，返回存储、故障状态与清理函数。
func openFaultStore(t *testing.T, template string) (*Store, *faultState, func()) {
	t.Helper()
	// path 是本次迭代独占的库文件。
	path := filepath.Join(t.TempDir(), "fault.db")
	// content、readErr 是模板库文件内容（模板已关闭，WAL 已合并）。
	content, readErr := os.ReadFile(template)
	if readErr != nil {
		t.Fatal(readErr)
	}
	// writeErr 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
	if writeErr := os.WriteFile(path, content, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	// probe 仅用于取得 SQLite 驱动实例。
	probe, probeErr := sql.Open("sqlite", sqliteDSN(path))
	if probeErr != nil {
		t.Fatal(probeErr)
	}
	// base 是基于驱动 Open 的最小连接器（modernc 驱动未实现 DriverContext）。
	base := &dsnConnector{drv: probe.Driver(), dsn: sqliteDSN(path)}
	_ = probe.Close()
	// state 是本迭代的故障倒计时。
	state := &faultState{}
	// handle 是带故障注入的连接池。
	handle := sql.OpenDB(&faultConnector{base: base, state: state})
	return NewStore(handle, DialectSQLite), state, func() { _ = handle.Close() }
}

// maxFaultPoints 是单个场景最多尝试的注入点数量，防止场景异常膨胀。
const maxFaultPoints = 400

// runFaultScenario 对一个“准备 + 调用”场景依次在第 1..n 次数据库操作处注入故障，直到调用不再触及更多操作。
// setup 在故障未布防时准备数据并返回待测调用；调用的错误结果不做断言（允许成功或失败），重点是执行全部失败回滚分支且不 panic 或挂起。
func runFaultScenario(t *testing.T, template string, setup func(t *testing.T, store *Store) func(ctx context.Context) error) {
	t.Helper()
	// point 是本轮注入故障的数据库操作序号。
	for point := int64(1); point <= maxFaultPoints; point++ {
		// store、state、cleanup 是本轮独立的故障注入存储。
		store, state, cleanup := openFaultStore(t, template)
		// call 是待测调用。
		call := setup(t, store)
		// ctx 防止调用在注入失败后的重试逻辑中挂起。
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		state.remaining.Store(point)
		_ = call(ctx)
		state.remaining.Store(0)
		cancel()
		// fired 表示本轮故障是否真的被触发；未触发说明调用的操作数已被超过。
		fired := state.fired.Load()
		cleanup()
		if !fired {
			return
		}
	}
}
