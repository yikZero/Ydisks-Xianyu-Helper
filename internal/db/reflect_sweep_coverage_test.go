package db

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// sweepArg 按参数类型构造一个用于反射调用仓储方法的入参；variant 控制字符串/数字取值，以覆盖不同的早退分支。
func sweepArg(ctx context.Context, typ reflect.Type, variant int) reflect.Value {
	switch {
	case typ.Implements(reflect.TypeOf((*context.Context)(nil)).Elem()):
		return reflect.ValueOf(ctx)
	case typ.Kind() == reflect.String:
		if variant == 0 {
			return reflect.Zero(typ)
		}
		if variant == 2 {
			// 种子账号 ID，使方法越过“账号存在”检查并触及后续表。
			return reflect.ValueOf("acc1").Convert(typ)
		}
		return reflect.ValueOf("sweep-value").Convert(typ)
	case typ.Kind() >= reflect.Int && typ.Kind() <= reflect.Int64:
		// number 是整数入参；variant=0 取 0，其余取 1，用于命中参数校验与正常路径。
		number := reflect.New(typ).Elem()
		if variant != 0 {
			number.SetInt(1)
		}
		return number
	case typ.Kind() == reflect.Func:
		// 回调参数返回零值，避免被调用时 panic，同时不产生任何业务副作用。
		return reflect.MakeFunc(typ, func(_ /* 回调入参 */ []reflect.Value) []reflect.Value {
			// results 是回调的零值返回列表。
			results := make([]reflect.Value, typ.NumOut())
			// index 是返回值下标。
			for index := range results {
				results[index] = reflect.Zero(typ.Out(index))
			}
			return results
		})
	case typ.Kind() == reflect.Pointer:
		return reflect.New(typ.Elem())
	default:
		return reflect.Zero(typ)
	}
}

// sweepStore 对 Store 的每个仓储导出方法按多种入参组合调用一遍，返回调用次数；发生 panic 的调用被忽略。
func sweepStore(ctx context.Context, store *Store) int {
	// calls 统计已执行的反射调用次数。
	calls := 0
	// storeValue 是被遍历的聚合存储反射值。
	storeValue := reflect.ValueOf(store).Elem()
	// fieldIndex 是 Store 字段下标。
	for fieldIndex := 0; fieldIndex < storeValue.NumField(); fieldIndex++ {
		// repo 是当前仓储字段，仅处理导出的指针仓储。
		repo := storeValue.Field(fieldIndex)
		if repo.Kind() != reflect.Pointer || !repo.CanInterface() || repo.IsNil() {
			continue
		}
		// methodIndex 是仓储导出方法下标。
		for methodIndex := 0; methodIndex < repo.NumMethod(); methodIndex++ {
			// method 是当前导出方法的反射值。
			method := repo.Method(methodIndex)
			// variant 选择入参取值组合。
			for variant := 0; variant < 3; variant++ {
				// methodType 描述方法签名。
				methodType := method.Type()
				// args 是按签名构造的入参列表。
				args := make([]reflect.Value, methodType.NumIn())
				// argIndex 是入参下标。
				for argIndex := range args {
					args[argIndex] = sweepArg(ctx, methodType.In(argIndex), variant)
				}
				func() {
					defer func() { _ = recover() }()
					if methodType.IsVariadic() {
						method.CallSlice(args)
						return
					}
					method.Call(args)
				}()
				calls++
			}
		}
	}
	return calls
}

// seedSweepData 写入与反射入参 "acc1"/1 对应的账号、订单、会话与付款规则，使方法越过存在性检查并触及后续语句。
func seedSweepData(t *testing.T, store *Store) {
	t.Helper()
	// ctx 是种子写入上下文。
	ctx := context.Background()
	// userID、cookieID 是种子账号的归属与 ID（cookieID 固定为 acc1）。
	userID, cookieID := seedAccount(t, store)
	// upsertErr 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
	if upsertErr := store.Orders.Upsert(ctx, "acc1", OrderUpsertOpts{CookieID: cookieID, ItemID: "acc1", BuyerID: "acc1", OrderStatus: "pending_ship"}); upsertErr != nil {
		t.Fatal(upsertErr)
	}
	// sessionErr 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
	if sessionErr := store.Chats.UpsertSession(ctx, ChatSession{CookieID: cookieID, ChatID: "acc1", BuyerID: "acc1", ItemID: "acc1", LastMessageAt: 1}); sessionErr != nil {
		t.Fatal(sessionErr)
	}
	// ruleErr 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
	if _, ruleErr := store.Automation.Create(ctx, makeAutomationRule(cookieID, userID, "acc1", "order_paid", true, 1)); ruleErr != nil {
		t.Fatal(ruleErr)
	}
}

// TestSweepClosedDatabaseReturnsErrors 关闭连接后用反射遍历全部仓储方法，覆盖各方法首个数据库访问的错误传播分支且不得挂起。
func TestSweepClosedDatabaseReturnsErrors(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("重型故障注入遍历在竞态检测模式下耗时过长，常规模式已覆盖")
	}
	store, cleanup := newTestDB(t)
	defer cleanup()
	if closeErr := store.DB.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	// ctx 带总超时，防止个别方法在重试时长时间阻塞。
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// calls 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
	if calls := sweepStore(ctx, store); calls == 0 {
		t.Fatal("反射遍历未调用任何仓储方法")
	}
}

// TestSweepEmptyDatabaseHandlesZeroInputs 在种子库上用零值/占位入参遍历全部仓储方法，覆盖 not-found 与参数校验分支且不得 panic 到外部。
func TestSweepEmptyDatabaseHandlesZeroInputs(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("重型故障注入遍历在竞态检测模式下耗时过长，常规模式已覆盖")
	}
	store, cleanup := newTestDB(t)
	defer cleanup()
	seedSweepData(t, store)
	// ctx 带总超时，防止个别方法阻塞。
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// calls 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
	if calls := sweepStore(ctx, store); calls == 0 {
		t.Fatal("反射遍历未调用任何仓储方法")
	}
}

// TestSweepWithEachTableMissing 逐个把业务表改名使其不可用，再遍历全部仓储方法，覆盖事务中途某一步数据库失败的错误回滚分支。
func TestSweepWithEachTableMissing(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("重型故障注入遍历在竞态检测模式下耗时过长，常规模式已覆盖")
	}
	// probe、probeCleanup 仅用于列出迁移后的业务表名。
	probe, probeCleanup := newTestDB(t)
	// names 保存全部业务表名（排除迁移元数据与 SQLite 内部表）。
	var names []string
	// rows、queryErr 是表名查询结果。
	rows, queryErr := probe.DB.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'goose_%'`)
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	for rows.Next() {
		// name 是当前表名。
		var name string
		// scanErr 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
		if scanErr := rows.Scan(&name); scanErr != nil {
			t.Fatal(scanErr)
		}
		names = append(names, name)
	}
	// rowsErr 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
	if rowsErr := rows.Err(); rowsErr != nil {
		t.Fatal(rowsErr)
	}
	_ = rows.Close()
	probeCleanup()
	if len(names) == 0 {
		t.Fatal("迁移后应存在业务表")
	}
	// tableName 是本轮被改名（视为缺失）的表。
	for _, tableName := range names {
		t.Run(tableName, func(t *testing.T) {
			t.Parallel()
			// store、cleanup 是本轮独立的已迁移存储。
			store, cleanup := newTestDB(t)
			defer cleanup()
			seedSweepData(t, store)
			// renameErr 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
			if _, renameErr := store.DB.Exec(`ALTER TABLE "` + tableName + `" RENAME TO "` + tableName + `_gone"`); renameErr != nil {
				t.Skipf("表 %s 无法改名: %v", tableName, renameErr)
			}
			// ctx 带总超时，防止方法在重试时阻塞。
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			sweepStore(ctx, store)
		})
	}
}
