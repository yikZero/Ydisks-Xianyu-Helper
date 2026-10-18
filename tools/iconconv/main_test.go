package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// withArgs 临时替换进程命令行参数并在测试结束后恢复，使 main 可在进程内被驱动。
func withArgs(t *testing.T, args ...string) {
	t.Helper()
	// original 是替换前的命令行参数。
	original := os.Args
	os.Args = append([]string{"iconconv"}, args...)
	t.Cleanup(func() { os.Args = original })
}

// mustPanic 执行 fn 并断言其触发 panic，用于覆盖 main 中以 panic 报告 I/O 失败的分支。
func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("期望 panic")
		}
	}()
	fn()
}

// TestMainWrapsPNGIntoICO 验证 main 生成的 ICO 含正确头部、256 尺寸编码、数据长度与偏移，并保留原 PNG 字节。
func TestMainWrapsPNGIntoICO(t *testing.T) {
	// dir 是临时工作目录。
	dir := t.TempDir()
	// input、output 是输入 PNG 与输出 ICO 路径。
	input, output := filepath.Join(dir, "in.png"), filepath.Join(dir, "out.ico")
	// payload 是占位的 PNG 字节，转换器不解析其内容。
	payload := []byte("fake-png-bytes")
	if err := os.WriteFile(input, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	withArgs(t, input, output)
	main()
	// got、err 是输出的 ICO 内容。
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(got[2:4]) != 1 || binary.LittleEndian.Uint16(got[4:6]) != 1 {
		t.Fatalf("ICONDIR 错误: %v", got[:6])
	}
	if got[6] != 0 || got[7] != 0 || binary.LittleEndian.Uint32(got[14:18]) != uint32(len(payload)) || binary.LittleEndian.Uint32(got[18:22]) != 22 {
		t.Fatalf("目录项错误: %v", got[6:22])
	}
	if !bytes.Equal(got[22:], payload) {
		t.Fatal("PNG 数据未原样嵌入")
	}
}

// TestMainPanicsOnIOFailures 验证输入不可读或输出不可写时 main 以 panic 终止。
func TestMainPanicsOnIOFailures(t *testing.T) {
	// dir 是临时工作目录。
	dir := t.TempDir()
	withArgs(t, filepath.Join(dir, "missing.png"), filepath.Join(dir, "out.ico"))
	mustPanic(t, main)
	// input 是存在的输入；输出目录不存在导致写入失败。
	input := filepath.Join(dir, "in.png")
	if err := os.WriteFile(input, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	withArgs(t, input, filepath.Join(dir, "no-dir", "out.ico"))
	mustPanic(t, main)
}
