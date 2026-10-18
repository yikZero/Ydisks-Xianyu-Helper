package db

import (
	"context"
	"testing"
	"time"
)

// TestOpenRemoteDialectsFailFastWithoutServer 验证 PostgreSQL/MySQL URL 在无服务端可达时返回明确错误，且不遗留连接池。
func TestOpenRemoteDialectsFailFastWithoutServer(t *testing.T) {
	// cases 是各方言的不可达或非法连接串；端口 1 保证本机无监听。
	cases := map[string]string{
		"postgres 不可达": "postgres://user:pw@127.0.0.1:1/db?sslmode=disable&connect_timeout=1",
		"postgres 非法":  "postgres://user:pw@127.0.0.1:notaport/db",
		"mysql 不可达":    "mysql://user:pw@tcp(127.0.0.1:1)/db",
	}
	// name、url 是用例名与连接串。
	for name, url := range cases {
		t.Run(name, func(t *testing.T) {
			// ctx 限制单次连接尝试时长。
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// err、handle 保存该步骤的返回值或错误，用于断言预期结果或在故障注入场景中忽略其结果。
			if handle, _, err := Open(ctx, url); err == nil {
				_ = handle.Close()
				t.Fatal("无服务端时 Open 应失败")
			}
		})
	}
}

// TestMigrateRejectsUnknownDialect 验证未知方言时 Migrate 返回错误且不影响后续 SQLite 迁移。
func TestMigrateRejectsUnknownDialect(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()
	// ctx 是迁移调用上下文。
	ctx := context.Background()
	if err := Migrate(ctx, store.DB, Dialect("oracle")); err == nil {
		t.Fatal("未知方言应报错")
	}
	// 恢复 goose 全局方言，避免影响后续测试。
	if err := Migrate(ctx, store.DB, DialectSQLite); err != nil {
		t.Fatal(err)
	}
}
