package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"xianyu-go/internal/db"
)

// errBranchWrite 是测试写入器与读取器注入的固定失败原因，用于确认 CLI 会原样包装并返回 I/O 错误。
var errBranchWrite = errors.New("注入的 I/O 失败")

// failingWriter 在第 failAt 次写入时返回错误，用于逐个触发 CLI 输出提示失败的分支。
type failingWriter struct {
	// calls 记录已经发生的写入次数，从 1 开始计数。
	calls int
	// failAt 是从 1 开始的失败写入序号，到达该序号时 Write 返回 errBranchWrite。
	failAt int
}

// Write 实现 io.Writer；p 为待写入的提示文本，返回成功写入的字节数或注入的失败。
func (w *failingWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.failAt {
		return 0, errBranchWrite
	}
	return len(p), nil
}

// failingReader 每次读取都返回非 EOF 错误，用于验证读取密码失败会被向上传递。
type failingReader struct{}

// Read 实现 io.Reader；总是返回注入的非 EOF 错误。
func (failingReader) Read(_ []byte) (int, error) { return 0, errBranchWrite }

// TestRunWriteFailuresSweep 逐个让第 N 次输出失败，覆盖新建管理员与重置管理员流程中的全部写入错误分支。
func TestRunWriteFailuresSweep(t *testing.T) {
	// scenarios 描述两种流程：新建管理员（空库）与重置管理员（已有管理员并回答 y）。
	scenarios := []struct {
		name string
		// preseed 表示运行前是否先创建一个管理员。
		preseed bool
		// input 是喂给 CLI 的模拟标准输入。
		input string
	}{
		{name: "create", preseed: false, input: "a@example.com\nsecret\nsecret\n"},
		{name: "reset", preseed: true, input: "y\nsecret\nsecret\n"},
	}
	// sc 是当前遍历的场景。
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			// failAt 是要触发失败的写入序号；超过总写入次数后流程应成功，作为循环终点。
			for failAt := 1; failAt < 40; failAt++ {
				// dbPath 是本轮独立的临时 SQLite 文件，避免污染真实数据。
				dbPath := filepath.Join(t.TempDir(), "sweep.db")
				if sc.preseed {
					// seedErr 是预置管理员阶段的失败原因。
					if seedErr := run(context.Background(), dbPath, bufio.NewReader(strings.NewReader("a@example.com\nold\nold\n")), &bytes.Buffer{}); seedErr != nil {
						t.Fatalf("预置管理员失败: %v", seedErr)
					}
				}
				// writer 是在第 failAt 次写入失败的输出端。
				writer := &failingWriter{failAt: failAt}
				// runErr 是本轮 run 的返回值。
				runErr := run(context.Background(), dbPath, bufio.NewReader(strings.NewReader(sc.input)), writer)
				if runErr == nil {
					if writer.calls < failAt {
						return
					}
					t.Fatalf("failAt=%d 时应返回错误", failAt)
				}
				if !errors.Is(runErr, errBranchWrite) {
					t.Fatalf("failAt=%d 错误未包装注入原因: %v", failAt, runErr)
				}
			}
			t.Fatal("写入次数异常多，扫描未收敛")
		})
	}
}

// TestRunOpenDatabaseFailure 验证数据库无法打开时 run 返回带上下文的错误。
func TestRunOpenDatabaseFailure(t *testing.T) {
	// runErr 是打开不可创建路径时的错误。
	runErr := run(context.Background(), filepath.Join(t.TempDir(), "missing-dir", "x.db"), bufio.NewReader(strings.NewReader("")), &bytes.Buffer{})
	if runErr == nil || !strings.Contains(runErr.Error(), "打开数据库失败") {
		t.Fatalf("err=%v", runErr)
	}
}

// TestRunRejectsEmailAlreadyUsedByNormalUser 验证邮箱被普通用户占用时创建管理员失败并提示。
func TestRunRejectsEmailAlreadyUsedByNormalUser(t *testing.T) {
	// dbPath 是临时数据库路径。
	dbPath := filepath.Join(t.TempDir(), "dup.db")
	// database、dialect、openErr 是预置普通用户用的连接与方言。
	database, dialect, openErr := db.Open(context.Background(), dbPath)
	if openErr != nil {
		t.Fatal(openErr)
	}
	// store 提供用户仓储，用来占用邮箱。
	store := db.NewStore(database, dialect)
	// created、createErr 表示普通用户是否创建成功。
	created, createErr := store.Users.Create(context.Background(), "bob", "dup@example.com", "pw-secret")
	if createErr != nil || !created {
		t.Fatalf("预置用户失败: %v %v", created, createErr)
	}
	database.Close()
	// runErr 是用被占邮箱创建管理员时的错误。
	runErr := run(context.Background(), dbPath, bufio.NewReader(strings.NewReader("dup@example.com\nsecret\nsecret\n")), &bytes.Buffer{})
	if runErr == nil || !strings.Contains(runErr.Error(), "创建 admin 用户失败") {
		t.Fatalf("err=%v", runErr)
	}
}

// TestRunPasswordReadFailure 验证输入流在读取密码时出现非 EOF 错误会被包装返回。
func TestRunPasswordReadFailure(t *testing.T) {
	// dbPath 是临时数据库路径。
	dbPath := filepath.Join(t.TempDir(), "pw.db")
	// reader 先提供邮箱行，之后读取密码时遇到注入错误。
	reader := bufio.NewReaderSize(&sequenceReader{first: "a@example.com\n"}, 16)
	// runErr 是 run 返回的错误。
	runErr := run(context.Background(), dbPath, reader, &bytes.Buffer{})
	if runErr == nil || !strings.Contains(runErr.Error(), "读取密码失败") {
		t.Fatalf("err=%v", runErr)
	}
}

// sequenceReader 先返回 first 内容，之后每次读取都失败，用于模拟输入中途出错。
type sequenceReader struct {
	// first 是第一次读取返回的文本。
	first string
	// done 表示 first 是否已被消费。
	done bool
}

// Read 实现 io.Reader：首次返回 first，之后返回注入的失败。
func (r *sequenceReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errBranchWrite
	}
	r.done = true
	return copy(p, r.first), nil
}

// TestPromptPasswordTwiceBranches 覆盖空密码重试、读取失败（第一次与第二次）和提示写入失败。
func TestPromptPasswordTwiceBranches(t *testing.T) {
	// out 收集提示文本。
	var out bytes.Buffer
	// got、err 是先空密码再两次一致输入的结果。
	got, err := promptPasswordTwice(bufio.NewReader(strings.NewReader("\nabc\nabc\n")), &out)
	if err != nil || got != "abc" || !strings.Contains(out.String(), "密码不能为空") {
		t.Fatalf("got=%q err=%v out=%s", got, err, out.String())
	}
	// firstErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
	if _, firstErr := promptPasswordTwice(bufio.NewReader(failingReader{}), &bytes.Buffer{}); !errors.Is(firstErr, errBranchWrite) {
		t.Fatalf("第一次读取失败应返回注入错误: %v", firstErr)
	}
	// second 让第一次读取成功、第二次读取失败。
	second := bufio.NewReaderSize(&sequenceReader{first: "abc\n"}, 16)
	// secondErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
	if _, secondErr := promptPasswordTwice(second, &bytes.Buffer{}); !errors.Is(secondErr, errBranchWrite) {
		t.Fatalf("第二次读取失败应返回注入错误: %v", secondErr)
	}
	// 针对每个提示写入点注入失败：空密码提示、第二次提示、不一致提示。
	for _, tc := range []struct {
		// input 是模拟输入。
		input string
		// failAt 是失败的写入序号。
		failAt int
	}{{"\n", 2}, {"a\nb\n", 3}, {"a\n", 3}} {
		// writeErr 保存下方被测调用返回的错误，用于断言该路径按预期成功或失败。
		if _, writeErr := promptPasswordTwice(bufio.NewReader(strings.NewReader(tc.input)), &failingWriter{failAt: tc.failAt}); !errors.Is(writeErr, errBranchWrite) {
			t.Fatalf("input=%q failAt=%d err=%v", tc.input, tc.failAt, writeErr)
		}
	}
}

// TestReadPasswordNoEchoFallback 覆盖非终端回退路径：提示写入失败、读取错误、无换行 EOF 与换行写入失败。
func TestReadPasswordNoEchoFallback(t *testing.T) {
	if _, err := readPasswordNoEcho(bufio.NewReader(strings.NewReader("x\n")), "p:", &failingWriter{failAt: 1}); !errors.Is(err, errBranchWrite) {
		t.Fatalf("提示写入失败应返回: %v", err)
	}
	if _, err := readPasswordNoEcho(bufio.NewReader(failingReader{}), "p:", &bytes.Buffer{}); !errors.Is(err, errBranchWrite) {
		t.Fatalf("读取失败应返回: %v", err)
	}
	// got、err 是无换行直接 EOF 的输入，应被当作完整密码并去除空白。
	got, err := readPasswordNoEcho(bufio.NewReader(strings.NewReader(" pw ")), "p:", &bytes.Buffer{})
	if err != nil || got != "pw" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	if _, err := readPasswordNoEcho(bufio.NewReader(strings.NewReader("x\n")), "p:", &failingWriter{failAt: 2}); !errors.Is(err, errBranchWrite) {
		t.Fatalf("换行写入失败应返回: %v", err)
	}
}

// TestIsNotFound 验证 isNotFound 只识别（包装的）db.ErrNotFound。
func TestIsNotFound(t *testing.T) {
	if !isNotFound(db.ErrNotFound) || isNotFound(errBranchWrite) {
		t.Fatal("isNotFound 判断错误")
	}
}
