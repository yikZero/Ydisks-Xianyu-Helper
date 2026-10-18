package server

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pngHeaderForTest 是 PNG 魔数，足以让 http.DetectContentType 识别为 image/png。
var pngHeaderForTest = []byte("\x89PNG\r\n\x1a\n")

// zipEntryForTest 描述测试 zip 中的一个条目：name 为包内路径，data 为内容，dir 表示目录条目。
type zipEntryForTest struct {
	name string
	data []byte
	dir  bool
}

// buildImageZipForTest 按 entries 顺序生成图片 zip 字节。
func buildImageZipForTest(t *testing.T, entries []zipEntryForTest) []byte {
	t.Helper()
	// buf 保存 zip 输出。
	var buf bytes.Buffer
	// zw 负责写入条目。
	zw := zip.NewWriter(&buf)
	// entry 表示当前要写入的条目。
	for _, entry := range entries {
		// w、err 是条目写入器及创建错误。
		w, err := zw.Create(entry.name)
		if err != nil {
			t.Fatal(err)
		}
		if entry.dir {
			continue
		}
		if _, err = w.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExtractPublishImagesZipErrorBranches 覆盖解压流程的各类拒绝与跳过分支。
func TestExtractPublishImagesZipErrorBranches(t *testing.T) {
	if err := extractPublishImagesZip([]byte("nope"), t.TempDir()); err == nil {
		t.Error("invalid zip should fail")
	}

	// mixed 包含目录、空文件、非图片和合法图片，只有最后一项应落盘。
	mixed := buildImageZipForTest(t, []zipEntryForTest{
		{name: "dir/", dir: true},
		{name: "dir/empty.png", data: nil},
		{name: "dir/text.png", data: []byte("plain text")},
		{name: "dir/ok.png", data: append(append([]byte{}, pngHeaderForTest...), 1, 2, 3)},
	})
	// dest 是解压目标目录。
	dest := t.TempDir()
	if err := extractPublishImagesZip(mixed, dest); err != nil {
		t.Fatalf("mixed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "dir", "ok.png")); err != nil {
		t.Errorf("image should be extracted: %v", err)
	}

	// manyEntries 的文件数刚好超过上限。
	var manyEntries []zipEntryForTest
	// i 循环序号，用于生成不同的测试数据
	for i := 0; i <= maxItemPublishZipFiles; i++ {
		manyEntries = append(manyEntries, zipEntryForTest{name: "f" + strings.Repeat("a", i%7) + string(rune('A'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676)) + ".txt", data: []byte("x")})
	}
	if err := extractPublishImagesZip(buildImageZipForTest(t, manyEntries), t.TempDir()); err == nil {
		t.Error("too many files should fail")
	}

	// oversize 的单个图片超过 10 MiB。
	oversize := buildImageZipForTest(t, []zipEntryForTest{{name: "big.png", data: append(append([]byte{}, pngHeaderForTest...), make([]byte, 10<<20)...)}})
	if err := extractPublishImagesZip(oversize, t.TempDir()); err == nil {
		t.Error("oversize image should fail")
	}

	// img 是一个合法的小图片条目内容。
	img := append(append([]byte{}, pngHeaderForTest...), 9)
	if err := extractPublishImagesZip(buildImageZipForTest(t, []zipEntryForTest{{name: "../x.png", data: img}}), t.TempDir()); err == nil {
		t.Error("unsafe path should fail")
	}
	if err := extractPublishImagesZip(buildImageZipForTest(t, []zipEntryForTest{{name: "x.png", data: img}}), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing dest should fail")
	}

	// blockedDest 中 a 是普通文件，导致创建 a/ 子目录失败。
	blockedDest := t.TempDir()
	if err := os.WriteFile(filepath.Join(blockedDest, "a"), []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extractPublishImagesZip(buildImageZipForTest(t, []zipEntryForTest{{name: "a/x.png", data: img}}), blockedDest); err == nil {
		t.Error("mkdir conflict should fail")
	}

	// dirDest 中目标文件名已是目录，导致写文件失败。
	dirDest := t.TempDir()
	if err := os.Mkdir(filepath.Join(dirDest, "x.png"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := extractPublishImagesZip(buildImageZipForTest(t, []zipEntryForTest{{name: "x.png", data: img}}), dirDest); err == nil {
		t.Error("write onto directory should fail")
	}

	// rawZip 含未知压缩算法的条目，Open 必须失败。
	var rawBuf bytes.Buffer
	// rawWriter 写入原始条目。
	rawWriter := zip.NewWriter(&rawBuf)
	// rawEntry、err 是未知压缩方法条目的写入器及错误。
	rawEntry, err := rawWriter.CreateRaw(&zip.FileHeader{Name: "x.png", Method: 99})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = rawEntry.Write([]byte("zz"))
	_ = rawWriter.Close()
	if err = extractPublishImagesZip(rawBuf.Bytes(), t.TempDir()); err == nil {
		t.Error("unsupported compression should fail")
	}
}
