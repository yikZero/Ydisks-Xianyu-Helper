package server

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xianyu-go/internal/netguard"
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

// TestReadBatchImageFileBranches 覆盖批次图片读取的路径、缺失、空文件、超限和非图片分支。
func TestReadBatchImageFileBranches(t *testing.T) {
	// dir 是模拟批次上传目录。
	dir := t.TempDir()
	// writeFile 在 dir 中写入一个文件。
	writeFile := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("empty.png", nil)
	writeFile("text.png", []byte("hello"))
	writeFile("big.png", append(append([]byte{}, pngHeaderForTest...), make([]byte, 10<<20)...))
	writeFile("ok.png", append(append([]byte{}, pngHeaderForTest...), 1))

	if _, _, _, err := readBatchImageFile(dir, "../x.png"); err == nil {
		t.Error("unsafe ref should fail")
	}
	if _, _, _, err := readBatchImageFile(filepath.Join(dir, "nope"), "ok.png"); err == nil {
		t.Error("missing dir should fail")
	}
	// ref 表示每个应失败的图片引用。
	for _, ref := range []string{"missing.png", "empty.png", "text.png", "big.png"} {
		if _, _, _, err := readBatchImageFile(dir, ref); err == nil {
			t.Errorf("%s should fail", ref)
		}
	}
	// data、contentType、name、err 是成功读取的结果。
	data, contentType, name, err := readBatchImageFile(dir, "ok.png")
	if err != nil || len(data) == 0 || contentType != "image/png" || name != "ok.png" {
		t.Fatalf("ok read: %v %q %q", err, contentType, name)
	}
}

// TestWriteFileWithinRootAndValidateRef 覆盖根目录不存在、目标为目录以及图片引用校验。
func TestWriteFileWithinRootAndValidateRef(t *testing.T) {
	if err := writeFileWithinRoot(filepath.Join(t.TempDir(), "missing"), "a", []byte("x")); err == nil {
		t.Error("missing root should fail")
	}
	// dir 是上传根目录，其中 sub 为目录。
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := writeFileWithinRoot(dir, "sub", []byte("x")); err == nil {
		t.Error("writing onto directory should fail")
	}
	if err := validateBatchImageRef(dir, "https://example.test/a.png"); err != nil {
		t.Errorf("http ref: %v", err)
	}
	if err := validateBatchImageRef(dir, "../a.png"); err == nil {
		t.Error("unsafe ref should fail")
	}
	if err := validateBatchImageRef(dir, "sub"); err == nil {
		t.Error("directory ref should fail")
	}
	if err := validateBatchImageRef(dir, "missing.png"); err == nil {
		t.Error("missing ref should fail")
	}
	if got := randomHex(4); len(got) != 8 {
		t.Errorf("randomHex length = %d", len(got))
	}
}

// TestDownloadImageURLInvalidAndOversize 覆盖 URL 非法与远程图片超限分支。
func TestDownloadImageURLInvalidAndOversize(t *testing.T) {
	// previousPublicOnly 保存全局出站策略，测试结束后恢复。
	previousPublicOnly := netguard.DefaultPublicOnly()
	netguard.SetDefaultPublicOnly(false)
	t.Cleanup(func() { netguard.SetDefaultPublicOnly(previousPublicOnly) })
	// srv 返回超过 10 MiB 的图片。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(make([]byte, 10<<20+10))
	}))
	t.Cleanup(srv.Close)
	if _, _, err := downloadImageURL(context.Background(), srv.URL); err == nil {
		// 出站客户端自身限制响应体大小时同样以错误返回，业务上都必须拒绝超限图片。
		t.Errorf("oversize should fail, err=%v", err)
	}
	// badURL 表示非 http 协议或缺少主机的地址。
	for _, badURL := range []string{"ftp://x/a.png", "http:///a.png", "://bad"} {
		if _, _, err := downloadImageURL(context.Background(), badURL); err == nil {
			t.Errorf("%q should be invalid", badURL)
		}
	}
	if got := pathBaseFromURL("https://x.test/"); got == "" {
		t.Error("pathBaseFromURL fallback empty")
	}
}
