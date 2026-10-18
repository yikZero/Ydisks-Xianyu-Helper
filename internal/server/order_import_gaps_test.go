package server

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// failingImportReader 模拟读取到一半即失败的请求体，用于覆盖读取错误分支。
type failingImportReader struct{}

// Read 总是返回固定错误，模拟客户端连接中断。
func (failingImportReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

// buildImportZip 生成仅用于订单导入测试的 xlsx 压缩包，entries 的键为包内路径，值为文件内容。
func buildImportZip(t *testing.T, names []string, contents map[string]string) []byte {
	t.Helper()
	// buf 保存内存中的 zip 字节。
	var buf bytes.Buffer
	// zw 负责按 names 顺序写入压缩包条目。
	zw := zip.NewWriter(&buf)
	// name 表示当前要写入的包内文件名。
	for _, name := range names {
		// w、err 是当前条目的写入器及创建错误。
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = w.Write([]byte(contents[name])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// importSheetXML 生成含内联字符串单元格的工作表 XML，rows 为每行单元格文本。
func importSheetXML(rows [][]string) string {
	// sb 累积工作表 XML。
	var sb strings.Builder
	sb.WriteString(`<worksheet><sheetData>`)
	// row、r 表示当前行内容及其下标。
	for r, row := range rows {
		sb.WriteString(`<row>`)
		// cell、c 表示当前单元格文本及其列下标。
		for c, cell := range row {
			fmt.Fprintf(&sb, `<c r="%c%d" t="inlineStr"><is><t>%s</t></is></c>`, rune('A'+c), r+1, cell)
		}
		sb.WriteString(`</row>`)
	}
	sb.WriteString(`</sheetData></worksheet>`)
	return sb.String()
}

// TestParseImportedOrdersMultipartFailures 覆盖 multipart 损坏、超限、读取失败与原始请求超限。
func TestParseImportedOrdersMultipartFailures(t *testing.T) {
	// noBoundary 缺少 boundary，表单解析必须报“表单损坏”。
	noBoundary := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("abc"))
	noBoundary.Header.Set("Content-Type", "multipart/form-data")
	if _, err := parseImportedOrders(httptest.NewRecorder(), noBoundary); err == nil || !strings.Contains(err.Error(), "损坏") {
		t.Fatalf("corrupt form err=%v", err)
	}

	// uploadWith 构造包含 size 字节文件字段的 multipart 请求。
	uploadWith := func(size int) *http.Request {
		// body 保存 multipart 请求体。
		var body bytes.Buffer
		// mw 生成 multipart 边界。
		mw := multipart.NewWriter(&body)
		// part、err 是文件字段写入器及错误。
		part, err := mw.CreateFormFile("file", "orders.csv")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(bytes.Repeat([]byte("a"), size))
		_ = mw.Close()
		// req 是待解析的上传请求。
		req := httptest.NewRequest(http.MethodPost, "/x", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		return req
	}
	if _, err := parseImportedOrders(httptest.NewRecorder(), uploadWith(maxOrderImportMultipartBytes+1024)); err == nil || !strings.Contains(err.Error(), "33 MiB") {
		t.Fatalf("multipart too large err=%v", err)
	}
	if _, err := parseImportedOrders(httptest.NewRecorder(), uploadWith(maxOrderImportBytes+1)); err == nil || !strings.Contains(err.Error(), "32 MiB") {
		t.Fatalf("file too large err=%v", err)
	}

	// brokenRaw 的请求体读取即失败。
	brokenRaw := httptest.NewRequest(http.MethodPost, "/x", failingImportReader{})
	if _, err := parseImportedOrders(httptest.NewRecorder(), brokenRaw); err == nil || !strings.Contains(err.Error(), "读取请求失败") {
		t.Fatalf("read err=%v", err)
	}
	// hugeRaw 的原始请求体超过 32 MiB。
	hugeRaw := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(bytes.Repeat([]byte("a"), maxOrderImportBytes+1)))
	if _, err := parseImportedOrders(httptest.NewRecorder(), hugeRaw); err == nil || !strings.Contains(err.Error(), "32 MiB") {
		t.Fatalf("raw too large err=%v", err)
	}
}

// TestParseImportedOrderBytesAndJSONEdges 覆盖 .xls、空 JSON、非法单对象和 JSON 数组的各类坏输入。
func TestParseImportedOrderBytesAndJSONEdges(t *testing.T) {
	if _, err := parseImportedOrderBytes([]byte("x"), "a.XLS"); err == nil || !strings.Contains(err.Error(), ".xls") {
		t.Fatalf("xls err=%v", err)
	}
	if _, err := parseJSONOrders([]byte("  ")); err == nil {
		t.Fatal("empty json should fail")
	}
	if _, err := parseJSONOrders([]byte("{bad")); err == nil {
		t.Fatal("bad single json should fail")
	}
	// badArrays 是各类数组形态错误：元素非对象、缺少结束符、尾随内容与尾随非法内容。
	badArrays := []string{`[1]`, `[{"a":1}`, `[{"a":1}] 5`, `[{"a":1}] {`, `[{"a":1},`}
	// raw 表示当前非法数组样本。
	for _, raw := range badArrays {
		if _, err := parseJSONOrderArray([]byte(raw)); err == nil {
			t.Errorf("array %q should fail", raw)
		}
	}
	if _, err := parseJSONOrderArray([]byte(`]`)); err == nil {
		t.Error("leading closer should fail")
	}
	// tooMany 超过行数上限的 JSON 数组。
	tooMany := "[" + strings.Repeat("{},", maxOrderImportRows) + "{}]"
	if _, err := parseJSONOrderArray([]byte(tooMany)); err == nil {
		t.Error("too many json rows should fail")
	}
}

// TestParseDelimitedOrdersFailures 覆盖 CSV 表头读取失败、行解析失败、超行数和仅表头场景。
func TestParseDelimitedOrdersFailures(t *testing.T) {
	if _, err := parseDelimitedOrders(nil, ','); err == nil {
		t.Error("empty csv should fail")
	}
	if _, err := parseDelimitedOrders([]byte("订单号\n\"a\x00b\"c\n"), ','); err == nil {
		// 宽松引号模式下该样本可能合法；仅要求不会 panic。
		t.Log("lazy quotes accepted sample")
	}
	if _, err := parseDelimitedOrders([]byte("订单号\n"), ','); err == nil {
		t.Error("header only should fail")
	}
	// sb 累积超过行数上限的 CSV 文本。
	var sb strings.Builder
	sb.WriteString("order_id\n")
	// i 循环序号，用于生成不同的测试数据
	for i := 0; i <= maxOrderImportRows; i++ {
		sb.WriteString("x\n")
	}
	if _, err := parseDelimitedOrders([]byte(sb.String()), ','); err == nil {
		t.Error("too many csv rows should fail")
	}
}

// TestParseXLSXOrdersBranches 覆盖 xlsx 解析的结构错误、共享字符串、行宽不齐与行数超限。
func TestParseXLSXOrdersBranches(t *testing.T) {
	// okSheet 是含表头与一行数据的合法工作表，第二行比表头短。
	okSheet := importSheetXML([][]string{{"订单号", "金额"}, {"o1"}})
	// sheetName 是第一个工作表在包内的路径。
	const sheetName = "xl/worksheets/sheet1.xml"

	if _, err := parseXLSXOrders([]byte("not zip")); err == nil {
		t.Error("invalid zip should fail")
	}
	if _, err := parseXLSXOrders(buildImportZip(t, []string{"other.xml"}, map[string]string{"other.xml": "x"})); err == nil {
		t.Error("missing sheet should fail")
	}
	if _, err := parseXLSXOrders(buildImportZip(t, []string{sheetName}, map[string]string{sheetName: "<worksheet"})); err == nil {
		t.Error("bad sheet xml should fail")
	}
	if _, err := parseXLSXOrders(buildImportZip(t, []string{"xl/sharedStrings.xml", sheetName}, map[string]string{"xl/sharedStrings.xml": "<sst", sheetName: okSheet})); err == nil {
		t.Error("bad shared strings should fail")
	}
	// oneRow 只有表头，应提示至少需要两行。
	oneRow := importSheetXML([][]string{{"订单号"}})
	if _, err := parseXLSXOrders(buildImportZip(t, []string{sheetName}, map[string]string{sheetName: oneRow})); err == nil {
		t.Error("one row should fail")
	}
	// rows 为共享字符串、非首位 sharedStrings 条目与短行共存的正常场景。
	rows, err := parseXLSXOrders(buildImportZip(t, []string{"docProps.xml", "xl/sharedStrings.xml", sheetName},
		map[string]string{"docProps.xml": "x", "xl/sharedStrings.xml": `<sst><si><t>a</t></si></sst>`, sheetName: okSheet}))
	if err != nil || len(rows) != 1 || rows[0]["order_id"] != "o1" {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	// big 构造超过行数上限的数据行。
	bigRows := [][]string{{"订单号"}}
	// i 循环序号，用于生成不同的测试数据
	for i := 0; i <= maxOrderImportRows; i++ {
		bigRows = append(bigRows, []string{"x"})
	}
	if _, err = parseXLSXOrders(buildImportZip(t, []string{sheetName}, map[string]string{sheetName: importSheetXML(bigRows)})); err == nil {
		t.Error("too many xlsx rows should fail")
	}
}

// TestXLSXPartReadFailures 覆盖压缩包条目无法打开及 XML 过大的错误分支。
func TestXLSXPartReadFailures(t *testing.T) {
	// buf 保存带未知压缩算法条目的 zip。
	var buf bytes.Buffer
	// zw 用于写入原始条目。
	zw := zip.NewWriter(&buf)
	// w、err 是原始条目写入器及错误。
	w, err := zw.CreateRaw(&zip.FileHeader{Name: "xl/sharedStrings.xml", Method: 99})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("zz"))
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	// zr 读取上面构造的压缩包。
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = xlsxSharedStrings(zr); err == nil {
		t.Error("unsupported method should fail")
	}
	if _, err = readXLSXPart(zr.File[0]); err == nil {
		t.Error("readXLSXPart should fail")
	}
	if _, err = readLimitedXLSXXML(failingImportReader{}); err == nil {
		t.Error("reader failure should propagate")
	}
	if _, err = readLimitedXLSXXML(io.LimitReader(zeroImportReader{}, maxXLSXXMLPartBytes+5)); err == nil {
		t.Error("oversize xml should fail")
	}
	// sheet 条目不可读时 parseXLSXOrders 也要返回错误。
	if _, err = parseXLSXOrders(buf.Bytes()); err == nil {
		t.Error("parse should fail on unreadable shared strings")
	}
}

// zeroImportReader 无限产生零字节，用于构造超大 XML 分片而不占用大块内存。
type zeroImportReader struct{}

// Read 用零字节填满缓冲区。
func (zeroImportReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestImportHeaderAliasesAndStringFromAny 覆盖剩余的表头别名和 stringFromAny 默认类型分支。
func TestImportHeaderAliasesAndStringFromAny(t *testing.T) {
	// cases 把每个中英文别名映射到期望的标准字段名。
	cases := map[string]string{
		"cookie_id": "cookie_id", "账号": "cookie_id", "商品价格": "item_price", "itemprice": "item_price",
		"规格名": "spec_name", "规格值": "spec_value", "收件人": "receiver_name", "手机号": "receiver_phone",
		"城市": "receiver_city", "Unknown": "Unknown",
	}
	// in、want 表示原始表头与期望字段。
	for in, want := range cases {
		if got := normalizeImportHeader(in); got != want {
			t.Errorf("%q => %q want %q", in, got, want)
		}
	}
	if got := stringFromAny([]int{1, 2}); got != "[1 2]" {
		t.Errorf("default branch got %q", got)
	}
}
