package server

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestParseMoneyCents(t *testing.T) {
	cases := map[string]int64{"1": 100, "1.2": 120, "¥12.34": 1234, "￥0.01": 1, "-0.50": -50, "+2.05": 205, "": 0}
	for raw, want := range cases {
		got, err := parseMoneyCents(raw)
		if err != nil || got != want {
			t.Errorf("parseMoneyCents(%q) = %d, %v; want %d", raw, got, err, want)
		}
	}
	if _, err := parseMoneyCents("1.2.3"); err == nil {
		t.Fatal("invalid money should fail")
	}
}

func TestReadLimitedXLSXXMLRejectsOversizedPart(t *testing.T) {
	_, err := readLimitedXLSXXML(io.LimitReader(endlessByteReader{}, maxXLSXXMLPartBytes+1))
	if err == nil {
		t.Fatal("oversized xlsx XML should fail")
	}
}

type endlessByteReader struct{}

// Read 读取当前值。
func (endlessByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestPublishBatchPathAndZipSafety(t *testing.T) {
	for _, raw := range []string{"../secret.png", "/etc/passwd", `..\\secret.png`, ""} {
		if _, err := safeZipPath(raw); err == nil {
			t.Errorf("safeZipPath(%q) should fail", raw)
		}
	}
	if got, err := safeZipPath("images/a.png"); err != nil || got != filepath.Join("images", "a.png") {
		t.Fatalf("safe path = %q, %v", got, err)
	}

	dest := t.TempDir()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("images/a.png")
	_, _ = f.Write([]byte("not-an-image"))
	_ = zw.Close()
	if err := extractPublishImagesZip(buf.Bytes(), dest); err != nil {
		t.Fatalf("extract non-image: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "images", "a.png")); !os.IsNotExist(err) {
		t.Fatal("non-image must not be extracted")
	}

	buf.Reset()
	zw = zip.NewWriter(&buf)
	f, _ = zw.Create("../escape.png")
	_, _ = f.Write([]byte("x"))
	_ = zw.Close()
	if err := extractPublishImagesZip(buf.Bytes(), dest); err == nil {
		t.Fatal("zip traversal should fail")
	}
}

// TestNormalizePublishCardHeader 验证批量发布卡密字段的历史表头兼容映射。
func TestNormalizePublishCardHeader(t *testing.T) {
	// got 表示历史中文表头归一化后的字段名。
	if got := normalizePublishHeader("付款后发送的卡密"); got != "paid_delivery_contents" {
		t.Fatalf("normalizePublishHeader=%q", got)
	}
}

func TestNormalizePublishHeaderCategoryFallbackLabels(t *testing.T) {
	cases := map[string]string{
		"类目ID":        "category_id",
		"类目名称":        "category_name",
		"频道类目ID":      "channel_category_id",
		"淘宝类目ID":      "tb_category_id",
		"category_id": "category_id",
	}
	for input, want := range cases {
		if got := normalizePublishHeader(input); got != want {
			t.Fatalf("normalizePublishHeader(%q)=%q want %q", input, got, want)
		}
	}
}
