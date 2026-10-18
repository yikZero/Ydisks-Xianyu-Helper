package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"xianyu-go/internal/db"
)

// branchTarget 打开一个带完整迁移的临时目标库，返回仓储与关闭函数；用于验证种子写入。
func branchTarget(t *testing.T) (*db.Store, *sql.DB) {
	t.Helper()
	// database、dialect、err 是目标库连接、方言与打开错误。
	database, dialect, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "target.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return db.NewStore(database, dialect), database
}

// branchLooseSource 创建一个只含 item_info/orders/cards 三张宽松表（允许 NULL）的临时源库，用于触发扫描错误。
func branchLooseSource(t *testing.T) *sql.DB {
	t.Helper()
	// source、err 是临时源库连接与打开错误。
	source, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { source.Close() })
	// ddl 是三张宽松表的建表语句，字段不带 NOT NULL 以便插入 NULL 触发 Scan 失败。
	ddl := []string{
		`CREATE TABLE item_info (item_id TEXT, item_title TEXT, item_description TEXT, item_category TEXT, item_price TEXT, updated_at TEXT)`,
		`CREATE TABLE orders (order_id TEXT, item_id TEXT, quantity TEXT, amount TEXT, order_status TEXT, spec_name TEXT, spec_value TEXT, updated_at TEXT)`,
		`CREATE TABLE cards (name TEXT, type TEXT, delay_seconds INTEGER, updated_at TEXT)`,
	}
	// stmt 是当前建表语句。
	for _, stmt := range ddl {
		// execErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
		if _, execErr := source.Exec(stmt); execErr != nil {
			t.Fatal(execErr)
		}
	}
	return source
}

// TestSeedFromSQLiteRejectsEmptyCredentials 验证空测试用户名或密码会被拒绝。
func TestSeedFromSQLiteRejectsEmptyCredentials(t *testing.T) {
	// target 是目标仓储。
	target, _ := branchTarget(t)
	// source 是空源库。
	source := branchLooseSource(t)
	if _, err := seedFromSQLite(context.Background(), source, target, seedOptions{Username: "", Password: "x"}); err == nil {
		t.Fatal("空用户名应被拒绝")
	}
}

// TestSeedFromSQLiteAppliesDefaultsAndSeedsTextCards 覆盖默认管理员、非法 limit 回落、跳过无映射订单与 text 卡密/脏订单分支。
func TestSeedFromSQLiteAppliesDefaultsAndSeedsTextCards(t *testing.T) {
	ctx := context.Background()
	// target 是目标仓储。
	target, _ := branchTarget(t)
	// source 是宽松源库，写入一件商品、两个订单（一个无商品映射）与一张 text 卡密（名称为空）。
	source := branchLooseSource(t)
	// seedSQL 是源库预置数据。
	seedSQL := []string{
		`INSERT INTO item_info VALUES ('i1','','d','c','9.9','1')`,
		`INSERT INTO orders VALUES ('o1','i1','1','abc','paid','规格','红','1')`,
		`INSERT INTO orders VALUES ('o2','unknown-item','1','1.00','paid','','','2')`,
		`INSERT INTO cards VALUES ('','text',5,'1')`,
	}
	// stmt 是当前预置语句。
	for _, stmt := range seedSQL {
		if _, err := source.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	// result、err 是使用默认管理员与越界 limit 的种子结果。
	result, err := seedFromSQLite(ctx, source, target, seedOptions{Username: "fx", Password: "fx-pass", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if result.Items != 1 || result.Orders != 1 || result.Cards != 1 {
		t.Fatalf("result=%+v", result)
	}
	// admin、adminErr 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	if admin, adminErr := target.Users.GetByUsername(ctx, "docker_admin"); adminErr != nil || !admin.IsAdmin {
		t.Fatalf("默认管理员缺失: %+v %v", admin, adminErr)
	}
}

// TestSeedFromSQLiteSurfacesTargetFailures 通过重命名目标库的表，依次触发各阶段的写入失败返回。
func TestSeedFromSQLiteSurfacesTargetFailures(t *testing.T) {
	// tables 是依次被破坏的目标表。
	tables := []string{"cookies", "cookie_status", "item_info", "orders", "cards"}
	// table 是当前被破坏的表名。
	for _, table := range tables {
		t.Run(table, func(t *testing.T) {
			// target、database 是目标仓储与底层连接。
			target, database := branchTarget(t)
			// source 是含一条数据的宽松源库，确保各阶段都会执行写入。
			source := branchLooseSource(t)
			// stmt 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
			for _, stmt := range []string{
				`INSERT INTO item_info VALUES ('i1','t','d','c','9.9','1')`,
				`INSERT INTO orders VALUES ('o1','i1','1','1.00','paid','','','1')`,
				`INSERT INTO cards VALUES ('n','data',0,'1')`,
			} {
				if _, err := source.Exec(stmt); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := database.Exec("ALTER TABLE " + table + " RENAME TO broken_" + table); err != nil {
				t.Skipf("无法破坏表 %s: %v", table, err)
			}
			if _, err := seedFromSQLite(context.Background(), source, target, seedOptions{Username: "fx", Password: "p", Limit: 5}); err == nil {
				t.Fatalf("表 %s 损坏后应返回错误", table)
			}
		})
	}
}

// TestEnsureFixtureAdminBranches 覆盖创建冲突、数据库故障与已存在管理员的密码重置分支。
func TestEnsureFixtureAdminBranches(t *testing.T) {
	ctx := context.Background()
	// target、database 是目标仓储与连接。
	target, database := branchTarget(t)
	// 先占用 adm@docker.test 邮箱，使以 adm 创建管理员时因邮箱冲突失败。
	if ok, err := target.Users.Create(ctx, "other", "adm@docker.test", "pw-pw-pw"); err != nil || !ok {
		t.Fatalf("预置用户: %v %v", ok, err)
	}
	if err := ensureFixtureAdmin(ctx, target, "adm", "pw"); err == nil {
		t.Fatal("邮箱冲突应失败")
	}
	// 已存在用户走密码重置并升为管理员。
	if err := ensureFixtureAdmin(ctx, target, "other", "new-pass-1"); err != nil {
		t.Fatalf("已存在用户应被重置: %v", err)
	}
	database.Close()
	if err := ensureFixtureAdmin(ctx, target, "other", "x"); err == nil {
		t.Fatal("连接关闭后应失败")
	}
}

// TestPrepareFixtureUserBranches 覆盖创建冲突、清理失败与连接故障分支，并验证已有用户被重置为普通用户。
func TestPrepareFixtureUserBranches(t *testing.T) {
	ctx := context.Background()
	// target、database 是目标仓储与连接。
	target, database := branchTarget(t)
	// err、ok 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	if ok, err := target.Users.Create(ctx, "other", "u@docker.test", "pw-pw-pw"); err != nil || !ok {
		t.Fatalf("预置用户: %v %v", ok, err)
	}
	// 新用户名 u 对应邮箱 u@docker.test 已被占用，创建应失败。
	if _, err := prepareFixtureUser(ctx, target, seedOptions{Username: "u", Password: "p"}); err == nil {
		t.Fatal("邮箱冲突应失败")
	}
	// existing 是已存在用户的重置结果，应保持可用且非管理员。
	existing, err := prepareFixtureUser(ctx, target, seedOptions{Username: "other", Password: "newpass1"})
	if err != nil || existing.IsAdmin {
		t.Fatalf("existing=%+v err=%v", existing, err)
	}
	if _, err := database.Exec(`ALTER TABLE automation_rules RENAME TO broken_rules`); err != nil {
		t.Skipf("无法破坏表: %v", err)
	}
	if _, err := prepareFixtureUser(ctx, target, seedOptions{Username: "other", Password: "p"}); err == nil || !strings.Contains(err.Error(), "清理上次测试数据") {
		t.Fatalf("清理失败应返回: %v", err)
	}
	database.Close()
	if _, err := prepareFixtureUser(ctx, target, seedOptions{Username: "other", Password: "p"}); err == nil {
		t.Fatal("连接关闭后应失败")
	}
}

// TestSeedScansFailOnNullColumns 验证源库出现 NULL 主键/类型列时各 seed 函数返回扫描错误。
func TestSeedScansFailOnNullColumns(t *testing.T) {
	ctx := context.Background()
	// target 是目标仓储。
	target, _ := branchTarget(t)
	// source 是宽松源库，写入 NULL 关键列。
	source := branchLooseSource(t)
	// stmt 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	for _, stmt := range []string{
		`INSERT INTO item_info (item_title) VALUES ('x')`,
		`INSERT INTO orders (item_id) VALUES ('i')`,
		`INSERT INTO cards (name) VALUES ('x')`,
	} {
		if _, err := source.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := seedItems(ctx, source, target, "acc", 5); err == nil {
		t.Fatal("seedItems 应因 NULL item_id 扫描失败")
	}
	if _, err := seedOrders(ctx, source, target, "acc", map[string]string{}, 5); err == nil {
		t.Fatal("seedOrders 应因 NULL order_id 扫描失败")
	}
	if _, err := seedCards(ctx, source, target, 1, 5); err == nil {
		t.Fatal("seedCards 应因 NULL type 扫描失败")
	}
}

// TestSeedQueriesFailWhenSourceTablesMissing 验证源库缺少表时三个 seed 函数返回查询错误。
func TestSeedQueriesFailWhenSourceTablesMissing(t *testing.T) {
	ctx := context.Background()
	// target 是目标仓储。
	target, _ := branchTarget(t)
	// empty 是不含任何表的源库。
	empty, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer empty.Close()
	if _, _, err := seedItems(ctx, empty, target, "acc", 5); err == nil {
		t.Fatal("seedItems 应失败")
	}
	if _, err := seedOrders(ctx, empty, target, "acc", nil, 5); err == nil {
		t.Fatal("seedOrders 应失败")
	}
	if _, err := seedCards(ctx, empty, target, 1, 5); err == nil {
		t.Fatal("seedCards 应失败")
	}
}

// TestSeedWritesFailWhenTargetClosed 验证目标库不可写时 seedItems/seedOrders/seedCards 返回写入错误。
func TestSeedWritesFailWhenTargetClosed(t *testing.T) {
	ctx := context.Background()
	// target、database 是目标仓储与连接，稍后关闭。
	target, database := branchTarget(t)
	// source 是含合法数据的宽松源库。
	source := branchLooseSource(t)
	// stmt 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	for _, stmt := range []string{
		`INSERT INTO item_info VALUES ('i1','t','d','c','9','1')`,
		`INSERT INTO orders VALUES ('o1','i1','1','1.00','paid','','','1')`,
		`INSERT INTO cards VALUES ('n','text',0,'1')`,
	} {
		if _, err := source.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	database.Close()
	if _, _, err := seedItems(ctx, source, target, "acc", 5); err == nil {
		t.Fatal("seedItems 应失败")
	}
	if _, err := seedOrders(ctx, source, target, "acc", map[string]string{"i1": "docker-item-001"}, 5); err == nil {
		t.Fatal("seedOrders 应失败")
	}
	if _, err := seedCards(ctx, source, target, 1, 5); err == nil {
		t.Fatal("seedCards 应失败")
	}
}

// TestFallbackAndShortHash 验证空白回落默认值与短哈希的确定性和长度。
func TestFallbackAndShortHash(t *testing.T) {
	if fallback("  ", "默认") != "默认" || fallback("值", "默认") != "值" {
		t.Fatal("fallback 判断错误")
	}
	// first、second 是同一输入的两次哈希结果，用于验证确定性。
	first, second := shortHash("a"), shortHash("a")
	if first != second || first == shortHash("b") || len(first) != 12 {
		t.Fatal("shortHash 应确定、区分输入且为 12 个十六进制字符")
	}
}
