package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// errRowsIteration 是测试驱动在结果集逐行读取中途返回的数据库错误。
var errRowsIteration = errors.New("rows iteration interrupted")

// rowsIterationDriverName 是只供本测试使用的 database/sql 驱动注册名称。
const rowsIterationDriverName = "ydisks-rows-iteration-error"

// rowsIterationRegisterOnce 保证并发测试执行时仅注册一次测试驱动。
var rowsIterationRegisterOnce sync.Once

// rowsIterationState 记录测试连接执行过的写语句；共享给驱动打开的每个连接，读写由 mu 保护。
type rowsIterationState struct {
	// mu 保护 executed，database/sql 可能在不同 goroutine 中使用连接。
	mu sync.Mutex
	// executed 保存已执行的写 SQL；行流失败后不得出现领取或删除语句。
	executed []string
}

// rowsIterationSharedState 是测试驱动所有连接共用的状态记录器。
var rowsIterationSharedState = &rowsIterationState{}

// rowsIterationDriver 打开所有查询都在首行前失败的测试连接。
type rowsIterationDriver struct{}

// Open 返回共享状态记录器的测试连接。
func (rowsIterationDriver) Open(string) (driver.Conn, error) {
	return &rowsIterationConn{state: rowsIterationSharedState}, nil
}

// rowsIterationConn 实现被测仓储方法所需的最小查询与执行接口。
type rowsIterationConn struct {
	// state 保存本连接观察到的写语句。
	state *rowsIterationState
}

// Prepare 不支持非 Context 预处理路径；被测方法不会使用该接口。
func (*rowsIterationConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unsupported")
}

// Close 释放测试连接；共享状态无需额外清理。
func (*rowsIterationConn) Close() error { return nil }

// Begin 不支持事务；被测方法均不开启事务。
func (*rowsIterationConn) Begin() (driver.Tx, error) { return nil, errors.New("unsupported") }

// ExecContext 记录写语句，供测试断言行流失败后没有继续领取或删除。
func (connection *rowsIterationConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	connection.state.mu.Lock()
	defer connection.state.mu.Unlock()
	connection.state.executed = append(connection.state.executed, query)
	return driver.RowsAffected(1), nil
}

// QueryContext 对 ListIssues 的死信任务查询返回正常空结果，使测试只观察第一段运行查询的迭代错误；
// 其他查询返回在首行前就中断的结果集。
func (*rowsIterationConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "dead_letter") {
		return emptyRows{}, nil
	}
	return interruptedRows{}, nil
}

// emptyRows 是正常结束的空结果集。
type emptyRows struct{}

// Columns 返回单列名称；空结果集不会触发 Scan。
func (emptyRows) Columns() []string { return []string{"id"} }

// Close 关闭空结果集。
func (emptyRows) Close() error { return nil }

// Next 立即返回 io.EOF，表示结果集正常结束。
func (emptyRows) Next([]driver.Value) error { return io.EOF }

// interruptedRows 模拟连接中断等导致结果集提前结束的情况：Next 直接返回迭代错误。
type interruptedRows struct{}

// Columns 返回单列名称；首行前即失败，因此列数不影响被测方法的 Scan。
func (interruptedRows) Columns() []string { return []string{"id"} }

// Close 关闭测试行流；迭代错误通过 Next 返回。
func (interruptedRows) Close() error { return nil }

// Next 返回非 EOF 的迭代错误，database/sql 会将其保存为 rows.Err() 而不是视为结果集正常结束。
func (interruptedRows) Next([]driver.Value) error { return errRowsIteration }

// TestRepositoriesReturnRowsIterationErrors 验证四个仓储方法遇到结果集中途中断时返回原始错误，
// 不把截断的结果当作完整列表，也不继续执行领取或删除。
func TestRepositoriesReturnRowsIterationErrors(t *testing.T) {
	rowsIterationRegisterOnce.Do( /* 当前回调注册仅供本测试使用的最小 database/sql 驱动。 */ func() {
		sql.Register(rowsIterationDriverName, rowsIterationDriver{})
	})
	// database 保存测试驱动提供的临时数据库连接。
	database, openErr := sql.Open(rowsIterationDriverName, "")
	if openErr != nil {
		t.Fatalf("打开测试数据库失败: %v", openErr)
	}
	defer database.Close()
	ctx := context.Background()
	// automation 是绑定测试驱动的自动化规则仓储，覆盖问题列表与延迟任务领取。
	automation := &AutomationRules{DB: database}
	// notifications 是绑定测试驱动的通知仓储，覆盖发件箱领取。
	notifications := &Notifications{DB: database}
	// keywords 是绑定测试驱动的关键字仓储，覆盖按下标删除。
	keywords := &Keywords{DB: database}
	// cases 覆盖问题列表、到期延迟任务领取、通知发件箱领取和按下标删除关键字。
	cases := []struct {
		// name 是被测方法；call 执行该方法并返回其错误。
		name string
		call func() error
	}{
		{name: "ListIssues", call: func() error {
			// err 是问题列表读取遇到行流中断后的返回值。
			_, _, err := automation.ListIssues(ctx, 1)
			return err
		}},
		{name: "ClaimDueDeferredTasks", call: func() error {
			// err 是延迟任务领取遇到行流中断后的返回值。
			_, err := automation.ClaimDueDeferredTasks(ctx, 10)
			return err
		}},
		{name: "ClaimOutbox", call: func() error {
			// err 是通知发件箱领取遇到行流中断后的返回值。
			_, err := notifications.ClaimOutbox(ctx, "worker", time.Now(), 10)
			return err
		}},
		{name: "DeleteByIndex", call: func() error { return keywords.DeleteByIndex(ctx, "cookie-1", 0) }},
	}
	// testCase 是当前验证的仓储方法。
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// err 是被测方法遇到行流中断后的返回值。
			if err := testCase.call(); !errors.Is(err, errRowsIteration) {
				t.Fatalf("应返回原始迭代错误，got %v", err)
			}
		})
	}
	rowsIterationSharedState.mu.Lock()
	defer rowsIterationSharedState.mu.Unlock()
	if len(rowsIterationSharedState.executed) != 0 {
		t.Fatalf("行流失败后不应继续执行写语句: %v", rowsIterationSharedState.executed)
	}
}
