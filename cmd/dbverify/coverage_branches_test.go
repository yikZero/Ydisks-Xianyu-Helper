package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"xianyu-go/internal/db"
	"xianyu-go/internal/logsafe"
)

// newBranchStore 在临时 SQLite 上打开迁移完成的仓储集合；返回仓储、底层连接关闭函数，供各分支测试复用。
func newBranchStore(t *testing.T) (*db.Store, func()) {
	t.Helper()
	// database、dialect、err 是临时库连接、方言与打开错误。
	database, dialect, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "branch.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db.NewStore(database, dialect), func() { database.Close() }
}

// branchUserID 在仓储中创建一个验证用户并返回其 ID，满足 cookies 表对用户的外键约束。
func branchUserID(t *testing.T, store *db.Store) int64 {
	t.Helper()
	// ok、err 表示用户创建是否成功及失败原因。
	ok, err := store.Users.Create(context.Background(), "branch_user", "branch_user@test.local", "branch-pass-123")
	if err != nil || !ok {
		t.Fatalf("创建用户: ok=%v err=%v", ok, err)
	}
	// user、getErr 是刚创建的用户记录及查询错误。
	user, getErr := store.Users.GetByUsername(context.Background(), "branch_user")
	if getErr != nil {
		t.Fatalf("查询用户: %v", getErr)
	}
	return user.ID
}

// TestVerifyCookieAndSettingsSucceeds 验证成功路径：Cookie 与设置 upsert 全部通过且无错误。
func TestVerifyCookieAndSettingsSucceeds(t *testing.T) {
	// store、closeDB 是临时仓储及其关闭函数。
	store, closeDB := newBranchStore(t)
	defer closeDB()
	if err := verifyCookieAndSettings(context.Background(), store, "acc_branch", branchUserID(t, store)); err != nil {
		t.Fatalf("verifyCookieAndSettings: %v", err)
	}
}

// TestVerifyCookieAndSettingsReportsFirstFailure 验证数据库不可用时返回第一步（保存 cookie）的包装错误。
func TestVerifyCookieAndSettingsReportsFirstFailure(t *testing.T) {
	// store、closeDB 是临时仓储及其关闭函数；先关闭连接模拟数据库故障。
	store, closeDB := newBranchStore(t)
	closeDB()
	// err 是在已关闭连接上执行验证得到的错误。
	err := verifyCookieAndSettings(context.Background(), store, "acc_branch", 1)
	if err == nil || !strings.Contains(err.Error(), "保存 cookie 失败") {
		t.Fatalf("err=%v", err)
	}
}

// TestVerifyCookieAndSettingsStagedFailures 通过在各步骤前删除对应表，依次覆盖状态、二次保存与设置写入失败分支。
func TestVerifyCookieAndSettingsStagedFailures(t *testing.T) {
	// stages 描述要删除的表与期望出现的错误片段。
	stages := []struct {
		// drop 是执行验证前要执行的破坏性 SQL。
		drop string
		// want 是期望错误中包含的文本。
		want string
	}{
		{drop: `DROP TABLE cookie_status`, want: "禁用验证账号失败"},
		{drop: `DROP TABLE system_settings`, want: "系统设置 Set 失败"},
	}
	// st 是当前遍历的失败阶段。
	for _, st := range stages {
		t.Run(st.want, func(t *testing.T) {
			// store、closeDB 是该阶段独立的临时仓储。
			store, closeDB := newBranchStore(t)
			defer closeDB()
			// userID 是满足外键的验证用户 ID。
			userID := branchUserID(t, store)
			if _, err := store.DB.Exec(st.drop); err != nil {
				t.Skipf("当前 schema 无法执行 %q: %v", st.drop, err)
			}
			err := verifyCookieAndSettings(context.Background(), store, "acc_stage", userID)
			if err == nil || !strings.Contains(err.Error(), st.want) {
				t.Fatalf("err=%v want %q", err, st.want)
			}
		})
	}
}

// TestSafeDiagnosticArgsRedactsErrorsOnly 验证错误参数被脱敏替换，非错误参数及原切片保持不变。
func TestSafeDiagnosticArgsRedactsErrorsOnly(t *testing.T) {
	// secretErr 是含连接凭证文本的错误，输出时必须经过 logsafe 处理。
	secretErr := errors.New("dial mysql://user:topsecret@host/db failed")
	// original 是调用方传入的参数切片，函数不得原地修改。
	original := []any{"plain", 7, secretErr}
	// got 是脱敏后的参数副本。
	got := safeDiagnosticArgs(original)
	if got[0] != "plain" || got[1] != 7 {
		t.Fatalf("非错误参数被改动: %v", got)
	}
	if got[2] != logsafe.Error(secretErr) {
		t.Fatalf("错误参数未脱敏: %v", got[2])
	}
	if original[2] != secretErr {
		t.Fatal("原切片被修改")
	}
}

// TestMaskURLWithoutCredentials 覆盖带已知 scheme 但没有 '@' 的 URL（原样返回）。
func TestMaskURLWithoutCredentials(t *testing.T) {
	if got := maskURL("postgres://host:5432/db"); got != "postgres://host:5432/db" {
		t.Fatalf("got=%q", got)
	}
}

// TestCleanupVerifyDataFailsOnClosedDatabase 验证数据库不可用时清理返回错误而非静默成功。
func TestCleanupVerifyDataFailsOnClosedDatabase(t *testing.T) {
	// store、closeDB 是临时仓储，先关闭以模拟故障。
	store, closeDB := newBranchStore(t)
	closeDB()
	if err := cleanupVerifyData(context.Background(), store, newVerifyIDs(1)); err == nil {
		t.Fatal("已关闭连接时清理应失败")
	}
}
