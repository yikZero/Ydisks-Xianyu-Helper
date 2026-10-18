package main

import (
	"go/ast"
	"os"
	"path/filepath"
	"testing"
)

// writeFixture 在临时根目录下创建夹具文件，必要时创建父目录。
func writeFixture(t *testing.T, root, relative, content string) {
	t.Helper()
	// full 是夹具文件的绝对路径。
	full := filepath.Join(root, filepath.FromSlash(relative))
	// mkdirErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if mkdirErr := os.MkdirAll(filepath.Dir(full), 0o755); mkdirErr != nil {
		t.Fatal(mkdirErr)
	}
	// writeErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if writeErr := os.WriteFile(full, []byte(content), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
}

// TestInspectGoFileReportsExportedAPIOnly 验证只有导出函数、导出类型上的导出方法和导出类型需要文档，
// 未导出声明、局部变量、字段和标准接口方法不再强制注释。
func TestInspectGoFileReportsExportedAPIOnly(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "bad.go", `package bad

var plain int
type Thing struct {
	Named string
}
type hidden struct{}
func Missing() {
	local := 1
	_ = local
}
func (Thing) Method() {}
func (Thing) Error() string { return "" }
func (hidden) Exported() {}
func unexported() {}
type (
	Grouped int
	// Documented 有文档。
	Documented int
)
`)
	writeFixture(t, root, "good.go", `package good

// Thing 是带说明的类型。
type Thing struct {
	Named string
}

// Run is documented in English, which is also accepted.
func Run() {}

// Method 是带说明的方法。
func (Thing) Method() {}
`)
	findings, inspectErr := inspectGoFile(root, filepath.Join(root, "bad.go"))
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	names := map[string]string{}
	for _, finding := range findings {
		names[finding.Name] = finding.Kind
	}
	want := map[string]string{"Thing": "type", "Missing": "function", "Method": "function", "Grouped": "type"}
	if len(names) != len(want) {
		t.Fatalf("findings=%+v", findings)
	}
	for name, kind := range want {
		if names[name] != kind {
			t.Fatalf("缺少 %s(%s): %+v", name, kind, findings)
		}
	}
	goodFindings, goodErr := inspectGoFile(root, filepath.Join(root, "good.go"))
	if goodErr != nil || len(goodFindings) != 0 {
		t.Fatalf("good findings=%+v err=%v", goodFindings, goodErr)
	}
}

// TestReceiverTypeNameHandlesGenericReceivers 验证泛型和指针接收者能解析出基础类型名。
func TestReceiverTypeNameHandlesGenericReceivers(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "generic.go", `package generic

type Box[T any] struct{ v T }
type Pair[K comparable, V any] struct{}

func (b *Box[T]) Get() T { return b.v }
func (Pair[K, V]) Keys() {}
`)
	findings, err := inspectGoFile(root, filepath.Join(root, "generic.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 4 {
		t.Fatalf("findings=%+v", findings)
	}
	if receiverTypeName(&ast.SelectorExpr{}) != "" {
		t.Fatal("未知接收者表达式应返回空名称")
	}
}

// TestCollectFindingsWalksTreeAndSkipsExcluded 覆盖递归扫描、生成文件/忽略目录/冻结文件的跳过、排序和语法错误传播。
func TestCollectFindingsWalksTreeAndSkipsExcluded(t *testing.T) {
	// root 是夹具根目录。
	root := t.TempDir()
	// missing 是缺少注释的最小源码。
	const missing = "package p\nfunc X() {}\n"
	writeFixture(t, root, "b.go", missing)
	writeFixture(t, root, "a.go", missing)
	writeFixture(t, root, "gen.gen.go", missing)
	writeFixture(t, root, "notes.txt", "ignored")
	writeFixture(t, root, "node_modules/dep/x.go", missing)
	writeFixture(t, root, "internal/webui/static/x.go", missing)
	writeFixture(t, root, "internal/browser/slider.go", missing)
	writeFixture(t, root, "c_test.go", missing)
	// findings、collectErr 是整棵树的扫描结果。
	findings, collectErr := collectFindings(root)
	if collectErr != nil {
		t.Fatal(collectErr)
	}
	sortFindings(findings)
	if len(findings) != 2 || findings[0].File != "a.go" || findings[1].File != "b.go" {
		t.Fatalf("findings=%+v", findings)
	}
	writeFixture(t, root, "broken.go", "package p\nfunc (")
	// brokenErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if _, brokenErr := collectFindings(root); brokenErr == nil {
		t.Fatal("语法错误应向上传播")
	}
	// templateErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if _, templateErr := collectTemplateCommentFindings(root); templateErr == nil {
		t.Fatal("模板审计遇到语法错误应失败")
	}
	// missingErr 保存该步骤返回的错误，用于断言预期的成功或失败路径。
	if _, missingErr := collectFindings(filepath.Join(root, "absent")); missingErr == nil {
		t.Fatal("不存在的根目录应失败")
	}
}

// TestCollectTemplateCommentFindingsSortsResults 覆盖整棵树的模板化注释扫描与排序。
func TestCollectTemplateCommentFindingsSortsResults(t *testing.T) {
	// root 是夹具根目录。
	root := t.TempDir()
	writeFixture(t, root, "z.go", "package p\n\n// err 表示错误\nvar err error\n")
	writeFixture(t, root, "a.go", "package p\n\n// count 表示数量\nvar count int\n")
	writeFixture(t, root, "internal/browser/token_captcha.go", "package p\n\n// err 表示错误\nvar err error\n")
	// findings、scanErr 是扫描结果。
	findings, scanErr := collectTemplateCommentFindings(root)
	if scanErr != nil || len(findings) != 2 || findings[0].File != "a.go" {
		t.Fatalf("findings=%+v err=%v", findings, scanErr)
	}
}

// TestShouldSkipDirectoryAndFrozenFiles 覆盖目录忽略规则与冻结文件清单。
func TestShouldSkipDirectoryAndFrozenFiles(t *testing.T) {
	// root 是扫描根目录。
	root := t.TempDir()
	// cases 保存相对目录到是否跳过的期望。
	cases := map[string]bool{".git": true, "data": true, "data/sub": true, "vendor/x": true, "internal/webui/static": true, "internal/server": false, "database": false}
	// relative、want 分别是相对目录与期望结果。
	for relative, want := range cases {
		// got 保存本次调用的返回值，用于与期望结果比较。
		if got := shouldSkipDirectory(filepath.Join(root, filepath.FromSlash(relative)), root); got != want {
			t.Fatalf("%s got %v want %v", relative, got, want)
		}
	}
	if !isFrozenGoFile("internal/browser/slider.go") || isFrozenGoFile("internal/browser/other.go") {
		t.Fatal("冻结文件清单判断错误")
	}
}

// TestHelpersCoverNamesAndSorting 覆盖匿名函数名和稳定排序的全部比较分支。
func TestHelpersCoverNamesAndSorting(t *testing.T) {
	if funcName(nil) != "anonymous" || funcName(&ast.Ident{Name: ""}) != "anonymous" || funcName(&ast.Ident{Name: "named"}) != "named" {
		t.Fatal("funcName 判断错误")
	}
	// findings 覆盖按文件、行号、类别、名称依次比较的排序。
	findings := []Finding{
		{File: "b", Line: 1, Kind: "k", Name: "n"},
		{File: "a", Line: 2, Kind: "k", Name: "n"},
		{File: "a", Line: 1, Kind: "z", Name: "n"},
		{File: "a", Line: 1, Kind: "k", Name: "z"},
		{File: "a", Line: 1, Kind: "k", Name: "a"},
	}
	sortFindings(findings)
	// want 是按文件、行号、类别、名称依次比较后的期望顺序。
	want := []Finding{
		{File: "a", Line: 1, Kind: "k", Name: "a"},
		{File: "a", Line: 1, Kind: "k", Name: "z"},
		{File: "a", Line: 1, Kind: "z", Name: "n"},
		{File: "a", Line: 2, Kind: "k", Name: "n"},
		{File: "b", Line: 1, Kind: "k", Name: "n"},
	}
	// index 是当前遍历到的元素，用于逐项断言或处理。
	for index := range want {
		if findings[index] != want[index] {
			t.Fatalf("index %d got %+v want %+v", index, findings[index], want[index])
		}
	}
}
