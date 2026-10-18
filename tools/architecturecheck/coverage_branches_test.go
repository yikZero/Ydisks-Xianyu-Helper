package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// archWrite 在临时仓库 root 下写入 relative 文件（自动创建父目录），用于搭建架构门禁夹具。
func archWrite(t *testing.T, root, relative, content string) {
	t.Helper()
	// full 是夹具文件绝对路径。
	full := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// archParse 把源码解析成 AST 并返回其位置集合，供直接调用扫描函数。
func archParse(t *testing.T, name, source string) (*ast.File, *token.FileSet) {
	t.Helper()
	// fset 是该夹具的位置集合。
	fset := token.NewFileSet()
	// syntax、err 是解析结果与错误。
	syntax, err := parser.ParseFile(fset, name, source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	return syntax, fset
}

// archBrokenLink 在 relative 处创建指向不存在目标的符号链接，使读取该文件必然失败（对 root 用户同样有效）。
func archBrokenLink(t *testing.T, root, relative string) {
	t.Helper()
	// full 是符号链接绝对路径。
	full := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), full); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
}

// archMessages 把违规列表拼成单个字符串，便于断言包含关键语义。
func archMessages(violations []violation) string {
	// parts 收集每条违规的文件与消息。
	parts := make([]string, 0, len(violations))
	// item 是当前违规。
	for _, item := range violations {
		parts = append(parts, item.file+": "+item.message)
	}
	return strings.Join(parts, "\n")
}

// archCleanRepo 搭建在指定阶段下零违规的最小仓库：总计划、生命周期清单、兼容矩阵、server.go、composition 与 cmd 入口、tsconfig。
func archCleanRepo(t *testing.T, planRow string) string {
	t.Helper()
	// root 是临时仓库根。
	root := t.TempDir()
	archWrite(t, root, "docs/architecture/refactoring-master-plan.md", "| 阶段 | 状态 |\n| --- | --- |\n"+planRow+"\n")
	archWrite(t, root, "docs/architecture/lifecycle-inventory.md", "所有者 Context 来源 停止/关闭 等待/观测 Wait/Join 锁顺序\n")
	archWrite(t, root, "docs/architecture/api-compatibility-matrix.md", "settingsResponse notificationBindingListResponse automationRulePageResponse v2.0\n")
	archWrite(t, root, "internal/server/server.go", "package server\n\nconst legacyAPISuccessorLink = \"</api/v1>; rel=\\\"successor-version\\\"; title=\\\"v2.0\\\"\"\nconst legacyAPISunsetDate = \"x\"\n\nfunc mark(w interface{ Header() map[string][]string }) {\n\tw.Header().Set(\"Deprecation\", \"true\")\n\tw.Header().Set(\"Sunset\", legacyAPISunsetDate)\n}\n")
	archWrite(t, root, "internal/composition/composition.go", "package composition\n")
	archWrite(t, root, "cmd/server/main.go", "package main\n\nimport _ \"xianyu-go/internal/composition/runtime\"\n\nfunc main() {}\n")
	archWrite(t, root, "frontend/tsconfig.json", `{"compilerOptions":{"noUnusedLocals":true,"noUnusedParameters":true}}`)
	return root
}

// TestCheckRepositoryCleanTreeAndMain 验证零违规仓库返回空结果，并驱动 main 的通过分支（含显式根目录参数）。
func TestCheckRepositoryCleanTreeAndMain(t *testing.T) {
	// root 是阶段六已完成的零违规仓库。
	root := archCleanRepo(t, "| 6. 收口 | 已完成 |")
	// violations、err 是整仓检查结果。
	violations, err := checkRepository(root)
	if err != nil || len(violations) != 0 {
		t.Fatalf("violations=%s err=%v", archMessages(violations), err)
	}
	// original 是测试前的命令行参数。
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	os.Args = []string{"architecturecheck", root}
	main()
}

// TestCheckRepositoryErrorBranches 覆盖总计划缺失、不可解析 Go 文件和不可读 Go 文件的错误返回。
func TestCheckRepositoryErrorBranches(t *testing.T) {
	if _, err := checkRepository(t.TempDir()); err == nil {
		t.Fatal("缺少总计划应失败")
	}
	// parseRoot 含语法错误的生产文件。
	parseRoot := archCleanRepo(t, "| 1. 基线 | 当前阶段 |")
	archWrite(t, parseRoot, "pkg/broken.go", "package broken\nfunc (")
	if _, err := checkRepository(parseRoot); err == nil {
		t.Fatal("语法错误应返回错误")
	}
	// readRoot 含一个断开的 .go 符号链接。
	readRoot := archCleanRepo(t, "| 1. 基线 | 当前阶段 |")
	archBrokenLink(t, readRoot, "pkg/link.go")
	if _, err := checkRepository(readRoot); err == nil {
		t.Fatal("不可读文件应返回错误")
	}
}

// TestCheckGoFileReportsEveryImportViolation 覆盖低层反向依赖、应用层基础设施依赖、隐藏依赖、Server 低层依赖与裸 BeginTx 五类违规。
func TestCheckGoFileReportsEveryImportViolation(t *testing.T) {
	// root 是承载违规源码的临时仓库。
	root := t.TempDir()
	archWrite(t, root, "internal/db/bad.go", "package db\n\nimport _ \"xianyu-go/internal/server\"\n")
	archWrite(t, root, "internal/application/items/bad.go", "package items\n\nimport _ \"database/sql\"\n")
	archWrite(t, root, "internal/application/orders/hidden.go", "package orders\n\nimport _ \"plugin\"\n")
	archWrite(t, root, "internal/server/bad.go", "package server\n\nimport _ \"xianyu-go/internal/xianyu\"\n\nfunc f(s struct{ DB interface{ BeginTx() } }) { s.DB.BeginTx() }\n")
	// cases 描述相对路径与期望的违规片段。
	cases := map[string]string{
		"internal/db/bad.go":                    "低层包禁止依赖上层应用包",
		"internal/application/items/bad.go":     "应用层禁止依赖基础设施",
		"internal/application/orders/hidden.go": "反射、插件或动态依赖",
		"internal/server/bad.go":                "Server 新增低层依赖",
	}
	// relative、want 是当前用例路径与期望片段。
	for relative, want := range cases {
		// violations、err 是单文件检查结果。
		violations, err := checkGoFile(root, relative, token.NewFileSet(), 1)
		if err != nil || !strings.Contains(archMessages(violations), want) {
			t.Errorf("%s: want %q got %s err=%v", relative, want, archMessages(violations), err)
		}
	}
	// serverViolations 是 Server 文件的完整结果，应额外包含事务违规。
	serverViolations, _ := checkGoFile(root, "internal/server/bad.go", token.NewFileSet(), 1)
	if !strings.Contains(archMessages(serverViolations), "禁止直接创建数据库事务") {
		t.Fatalf("缺少事务违规: %s", archMessages(serverViolations))
	}
	if _, err := checkGoFile(root, "missing.go", token.NewFileSet(), 1); err == nil {
		t.Fatal("文件缺失应失败")
	}
	archWrite(t, root, "pkg/broken.go", "package x\nfunc (")
	if _, err := checkGoFile(root, "pkg/broken.go", token.NewFileSet(), 1); err == nil {
		t.Fatal("语法错误应失败")
	}
}

// TestCheckStageTwoCompositionRootBranches 覆盖 composition 目录缺失、仅有测试文件、入口无法解析、入口未导入组合层。
func TestCheckStageTwoCompositionRootBranches(t *testing.T) {
	// missing 没有 composition 目录。
	if got := archMessages(checkStageTwoCompositionRoot(t.TempDir())); !strings.Contains(got, "独立 composition 包") {
		t.Fatalf("got=%s", got)
	}
	// onlyTests 的 composition 只含测试文件。
	onlyTests := t.TempDir()
	archWrite(t, onlyTests, "internal/composition/a_test.go", "package composition\n")
	if got := archMessages(checkStageTwoCompositionRoot(onlyTests)); !strings.Contains(got, "必须包含生产装配代码") {
		t.Fatalf("got=%s", got)
	}
	// noMain 有生产文件但缺少 cmd/server/main.go。
	noMain := t.TempDir()
	archWrite(t, noMain, "internal/composition/a.go", "package composition\n")
	if got := archMessages(checkStageTwoCompositionRoot(noMain)); !strings.Contains(got, "无法解析 cmd/server") {
		t.Fatalf("got=%s", got)
	}
	// noImport 的入口没有导入组合层。
	noImport := t.TempDir()
	archWrite(t, noImport, "internal/composition/a.go", "package composition\n")
	archWrite(t, noImport, "cmd/server/main.go", "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println() }\n")
	if got := archMessages(checkStageTwoCompositionRoot(noImport)); !strings.Contains(got, "显式调用 internal/composition") {
		t.Fatalf("got=%s", got)
	}
}

// TestReadActiveArchitectureStageMissingFile 验证总计划文件不存在时返回读取错误。
func TestReadActiveArchitectureStageMissingFile(t *testing.T) {
	if _, err := readActiveArchitectureStage(t.TempDir()); err == nil {
		t.Fatal("缺少总计划应失败")
	}
}

// TestOpenAPIContractClosureWalkAndTransportImport 覆盖不可读前端文件的扫描失败与对 transport 模块的静态引用。
func TestOpenAPIContractClosureWalkAndTransportImport(t *testing.T) {
	// root 含一个引用 ./transport 的 feature 文件。
	root := t.TempDir()
	archWrite(t, root, "frontend/app/features/a/page.ts", "import { X } from './transport';\n")
	if got := archMessages(checkOpenAPIContractClosure(root)); !strings.Contains(got, "禁止引用手写 transport DTO") {
		t.Fatalf("got=%s", got)
	}
	// broken 含一个不可读的 .ts 生产文件。
	broken := t.TempDir()
	archBrokenLink(t, broken, "frontend/app/a.ts")
	if got := archMessages(checkOpenAPIContractClosure(broken)); !strings.Contains(got, "扫描 OpenAPI 契约闭环失败") {
		t.Fatalf("got=%s", got)
	}
	// skipped 的 node_modules 与 dist 内容必须被忽略。
	skipped := t.TempDir()
	archWrite(t, skipped, "frontend/node_modules/x/a.ts", "import { X } from '../transport';\n")
	archWrite(t, skipped, "frontend/dist/a.ts", "import { X } from '../transport';\n")
	if got := checkOpenAPIContractClosure(skipped); len(got) != 0 {
		t.Fatalf("got=%v", got)
	}
}

// TestLifecycleArchitectureGateBranches 覆盖受保护冻结文件、不可解析后台源码、缺失生命周期清单与缺少必填术语。
func TestLifecycleArchitectureGateBranches(t *testing.T) {
	// root 含冻结文件（应被跳过）、语法错误的后台文件、非后台文件。
	root := t.TempDir()
	archWrite(t, root, "internal/browser/slider.go", "package browser\n\nimport \"context\"\n\nvar _ = context.Background()\n")
	archWrite(t, root, "internal/engine/broken.go", "package engine\nfunc (")
	archWrite(t, root, "internal/other/a.go", "package other\n\nimport \"context\"\n\nvar _ = context.Background()\n")
	// got 是缺少清单时的违规文本。
	got := archMessages(checkLifecycleArchitecture(root))
	if !strings.Contains(got, "无法解析生产源码") || !strings.Contains(got, "无法读取生命周期清单") {
		t.Fatalf("got=%s", got)
	}
	if strings.Contains(got, "slider.go") || strings.Contains(got, "other/a.go") {
		t.Fatalf("冻结或非后台文件不应被报告: %s", got)
	}
	archWrite(t, root, "docs/architecture/lifecycle-inventory.md", "只有所有者\n")
	if got := archMessages(checkLifecycleArchitecture(root)); !strings.Contains(got, "缺少必填边界") {
		t.Fatalf("got=%s", got)
	}
}

// TestReactArchitectureGateBranches 覆盖集中入口残留、旧 HTTP 导入、直接网络请求、动态 import、barrel 与跨 feature 导入、tsconfig 缺失/非法/未启用选项。
func TestReactArchitectureGateBranches(t *testing.T) {
	// root 同时包含各类前端违规。
	root := t.TempDir()
	archWrite(t, root, "frontend/services/amapLocation.ts", "export {}\n")
	archWrite(t, root, "frontend/app/features/a/page.tsx", "import { get, type ApiError } from '../../../shared/http/client';\nimport x from '../../../shared/api-contract';\nimport y from '../b/internal';\nconst z = import('./lazy');\nfetch('/x');\n")
	archWrite(t, root, "frontend/app/features/a/ui.tsx", "import { t } from '../../../shared/api-contract/items';\n")
	archWrite(t, root, "frontend/node_modules/m/a.ts", "fetch('/x')\n")
	// got 是缺少 tsconfig 时的全部违规。
	got := archMessages(checkReactArchitecture(root))
	// want 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	for _, want := range []string{"根级服务", "旧 HTTP get", "直接请求网络", "动态 import", "barrel", "transport DTO", "禁止导入 feature b", "无法读取 TypeScript 配置"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q: %s", want, got)
		}
	}
	archWrite(t, root, "frontend/tsconfig.json", "{not json")
	if got := archMessages(checkReactArchitecture(root)); !strings.Contains(got, "无法解析 TypeScript 配置") {
		t.Fatalf("got=%s", got)
	}
	archWrite(t, root, "frontend/tsconfig.json", `{"compilerOptions":{"noUnusedLocals":true}}`)
	if got := archMessages(checkReactArchitecture(root)); !strings.Contains(got, "compilerOptions.noUnusedParameters") {
		t.Fatalf("got=%s", got)
	}
	// broken 含不可读的前端源码。
	broken := t.TempDir()
	archBrokenLink(t, broken, "frontend/app/a.ts")
	if got := archMessages(checkReactArchitecture(broken)); !strings.Contains(got, "扫描前端架构失败") {
		t.Fatalf("got=%s", got)
	}
}

// TestCheckCrossFeatureImportBranches 验证非 feature 源码、非相对导入、同 feature 导入放行，跨 feature 才报告。
func TestCheckCrossFeatureImportBranches(t *testing.T) {
	if checkCrossFeatureImport("shared/a.ts", "../app/features/x/y") != nil {
		t.Fatal("非 feature 源码不应检查")
	}
	if checkCrossFeatureImport("app/features/a/p.ts", "react") != nil {
		t.Fatal("非相对导入不应检查")
	}
	if checkCrossFeatureImport("app/features/a/p.ts", "./inner") != nil {
		t.Fatal("同 feature 导入应放行")
	}
	if len(checkCrossFeatureImport("app/features/a/p.ts", "../b/inner")) != 1 {
		t.Fatal("跨 feature 导入应报告")
	}
}

// TestDatabaseArchitectureGateBranches 覆盖不可解析的上层源码、database/sql 导入、Store.DB 访问和事务调用。
func TestDatabaseArchitectureGateBranches(t *testing.T) {
	// root 含上层违规源码和一个非上层文件。
	root := t.TempDir()
	archWrite(t, root, "internal/server/broken.go", "package server\nfunc (")
	archWrite(t, root, "internal/account/a.go", "package account\n\nimport \"database/sql\"\n\nvar _ sql.DB\n\nfunc f(s struct{ DB struct{ Begin func() } }) { s.DB.Begin() }\n")
	archWrite(t, root, "internal/db/ok.go", "package db\n\nimport \"database/sql\"\n\nvar _ sql.DB\n")
	// got 是数据库门禁结果。
	got := archMessages(checkDatabaseArchitecture(root))
	// want 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	for _, want := range []string{"无法解析生产源码", "database/sql", "Store.DB", "数据库事务"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q: %s", want, got)
		}
	}
	if strings.Contains(got, "internal/db/ok.go") {
		t.Fatalf("基础设施层不应被报告: %s", got)
	}
}

// TestQualityArchitectureGateBranches 覆盖冻结与生成文件豁免、解析失败、超长函数与高分支复杂度。
func TestQualityArchitectureGateBranches(t *testing.T) {
	// root 含各类质量夹具。
	root := t.TempDir()
	archWrite(t, root, "internal/browser/slider.go", "package browser\n"+strings.Repeat("// x\n", 900))
	archWrite(t, root, "pkg/gen.go", "// Code generated by x. DO NOT EDIT.\npackage pkg\n"+strings.Repeat("// x\n", 900))
	archWrite(t, root, "pkg/broken.go", "package pkg\nfunc (")
	// longBody 是超过 180 行的函数体。
	longBody := "package pkg\n\nfunc Long() {\n" + strings.Repeat("\t_ = 1\n", 200) + "}\n"
	archWrite(t, root, "pkg/long.go", longBody)
	// branchy 含 45 个 if 分支。
	branchy := "package pkg\n\nfunc Branchy(a bool) {\n" + strings.Repeat("\tif a {\n\t}\n", 45) + "}\n\nfunc Declared()\n"
	archWrite(t, root, "pkg/branchy.go", branchy)
	// got 是质量门禁结果。
	got := archMessages(checkQualityArchitecture(root))
	// want 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	for _, want := range []string{"复杂度扫描解析失败", "函数 Long 过大", "函数 Branchy 过大"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q: %s", want, got)
		}
	}
	if strings.Contains(got, "slider.go") || strings.Contains(got, "gen.go") {
		t.Fatalf("冻结或生成文件不应被报告: %s", got)
	}
}

// TestScanGoProductionFilesWalkAndReadFailures 覆盖读取失败时的汇总违规，以及 .git/static/测试文件的跳过。
func TestScanGoProductionFilesWalkAndReadFailures(t *testing.T) {
	// root 含应跳过的目录与测试文件，以及一个不可读的 Go 文件。
	root := t.TempDir()
	archWrite(t, root, ".git/a.go", "package a\n")
	archWrite(t, root, "internal/webui/static/a.go", "package a\n")
	archWrite(t, root, "pkg/a_test.go", "package a\n")
	archWrite(t, root, "pkg/ok.go", "package a\n")
	archBrokenLink(t, root, "pkg/zlink.go")
	// seen 记录被检查的相对路径。
	var seen []string
	// violations 是扫描汇总结果。
	violations := scanGoProductionFiles(root, func(relativePath string, _ []byte) []violation {
		seen = append(seen, relativePath)
		return nil
	})
	if len(seen) != 1 || seen[0] != "pkg/ok.go" || !strings.Contains(archMessages(violations), "遍历生产 Go 源码失败") {
		t.Fatalf("seen=%v violations=%s", seen, archMessages(violations))
	}
}

// TestSourceLineAtAndGeneratedSource 覆盖偏移越界回退、负偏移与生成标记长度截断。
func TestSourceLineAtAndGeneratedSource(t *testing.T) {
	// source 是三行文本。
	source := []byte("a\nb\nc")
	if sourceLineAt(source, -1) != 1 || sourceLineAt(source, 0) != 1 || sourceLineAt(source, 3) != 2 || sourceLineAt(source, 999) != 3 {
		t.Fatal("sourceLineAt 行号错误")
	}
	// late 的生成标记超出前 512 字节，不应视为生成文件。
	late := strings.Repeat("x", 600) + "Code generated DO NOT EDIT"
	if isGeneratedSource([]byte(late)) || !isGeneratedSource([]byte("// Code generated by x; DO NOT EDIT.")) {
		t.Fatal("isGeneratedSource 判断错误")
	}
	if isFrontendProductionPath("a.js") || isFrontendProductionPath("a.test.ts") || isFrontendProductionPath("a.spec.tsx") || !isFrontendProductionPath("a.tsx") {
		t.Fatal("isFrontendProductionPath 判断错误")
	}
}

// TestCompatibilityGovernanceBranches 覆盖矩阵缺失、server.go 缺失、登记缺漏、Sunset 版本缺失与遥测头缺失。
func TestCompatibilityGovernanceBranches(t *testing.T) {
	// noMatrix 没有兼容矩阵。
	if got := archMessages(checkCompatibilityGovernance(t.TempDir())); !strings.Contains(got, "无法读取兼容矩阵") {
		t.Fatalf("got=%s", got)
	}
	// noServer 有矩阵但没有 server.go。
	noServer := t.TempDir()
	archWrite(t, noServer, "docs/architecture/api-compatibility-matrix.md", "x")
	if got := archMessages(checkCompatibilityGovernance(noServer)); !strings.Contains(got, "无法读取历史 API 遥测实现") {
		t.Fatalf("got=%s", got)
	}
	// drift 的矩阵缺少登记名与版本，server.go 缺少 Link 与遥测头。
	drift := t.TempDir()
	archWrite(t, drift, "docs/architecture/api-compatibility-matrix.md", "nothing here")
	archWrite(t, drift, "internal/server/server.go", "package server\n")
	// got 是漂移结果。
	got := archMessages(checkCompatibilityGovernance(drift))
	// want 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	for _, want := range []string{"未登记在兼容矩阵", "缺少 Sunset 版本", "successor Link", "Deprecation 与 Sunset"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少 %q: %s", want, got)
		}
	}
}

// TestDynamicMapTypeHelpers 覆盖 any/空接口/非 any 值类型、数组、指针、结构体嵌套与 nil 字段判断。
func TestDynamicMapTypeHelpers(t *testing.T) {
	// parseType 解析单个类型表达式。
	parseType := func(source string) ast.Expr {
		// err、expr 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
		expr, err := parser.ParseExpr(source)
		if err != nil {
			t.Fatal(err)
		}
		return expr
	}
	// cases 把类型文本映射到是否含动态 map。
	cases := map[string]bool{
		"map[string]any":                    true,
		"map[string]interface{}":            true,
		"map[string]string":                 false,
		"[]map[string]any":                  true,
		"*map[string]any":                   true,
		"struct{ A map[string]any }":        true,
		"struct{ A string }":                false,
		"string":                            false,
		"map[string]interface{ M() }":       false,
		"[]*struct{ B []map[string]any }":   true,
		"map[string]map[string]interface{}": false,
	}
	// text、want 是类型文本与期望。
	for text, want := range cases {
		if got := containsDynamicMapType(parseType(text)); got != want {
			t.Errorf("%s got=%v want=%v", text, got, want)
		}
	}
	if isAnyType(parseType("string")) || !isAnyType(parseType("any")) || isAnyType(parseType("[]int")) {
		t.Fatal("isAnyType 判断错误")
	}
}

// TestLowLevelImportAndFirstLine 覆盖 browser 低层包反向依赖、同包放行、非低层文件放行与 firstLineContaining 的未命中。
func TestLowLevelImportAndFirstLine(t *testing.T) {
	if !isForbiddenLowLevelImport("internal/browser/a.go", "internal/engine") || isForbiddenLowLevelImport("internal/browser/a.go", "internal/browser/sub") || isForbiddenLowLevelImport("internal/other/a.go", "internal/engine") {
		t.Fatal("isForbiddenLowLevelImport 判断错误")
	}
	if !isForbiddenLowLevelImport("internal/xianyu/a.go", "internal/auth") || isForbiddenLowLevelImport("internal/db/a.go", "internal/logging") {
		t.Fatal("isForbiddenLowLevelImport 判断错误")
	}
	if firstLineContaining("a\nb", "zz") != 0 || firstLineContaining("a\nb", "b") != 2 {
		t.Fatal("firstLineContaining 判断错误")
	}
}

// TestStageTwoScannerBranches 覆盖 application_services.go 禁令、Port 容器的非结构体/集合形态、composition 核心反向依赖、非结构体 Server 与 HTTP 请求变量的非匿名/非 req 形态。
func TestStageTwoScannerBranches(t *testing.T) {
	// appSyntax、appFset 是 application_services.go 夹具。
	appSyntax, appFset := archParse(t, "application_services.go", "package server\n")
	if got := archMessages(checkStageTwoTransportBoundary("internal/server/application_services.go", appSyntax, appFset)); !strings.Contains(got, "application_services.go") {
		t.Fatalf("got=%s", got)
	}
	// portSyntax、portFset 的 ApplicationPorts 含 map、切片、嵌套指针与非应用字段。
	portSyntax, portFset := archParse(t, "application_ports.go", `package server

import (
	items "xianyu-go/internal/application/items"
	_ "xianyu-go/internal/application/orders"
	. "xianyu-go/internal/application/cards"
)

type ApplicationPortsInput interface{}

type ApplicationPorts struct {
	A map[string]*items.ItemService
	B []*items.ItemServiceSet
	C **items.ItemService
	D string
	E *items.Options
	F map[string]string
}
`)
	if got := checkStageTwoApplicationPortDeclarations("internal/server/application_ports.go", portSyntax, portFset); len(got) != 3 {
		t.Fatalf("got=%s", archMessages(got))
	}
	// compSyntax、compFset 是反向依赖 Server 的 composition 核心文件。
	compSyntax, compFset := archParse(t, "c.go", "package composition\n\nimport _ \"xianyu-go/internal/server\"\n")
	if got := checkStageTwoCompositionProjection("internal/composition/c.go", compSyntax, compFset); len(got) != 1 {
		t.Fatalf("got=%v", got)
	}
	if got := checkStageTwoCompositionProjection("internal/composition/runtime/c.go", compSyntax, compFset); got != nil {
		t.Fatalf("runtime 子层应放行: %v", got)
	}
	// ifaceSyntax、ifaceFset 的 Server 是接口而非结构体。
	ifaceSyntax, ifaceFset := archParse(t, "s.go", "package server\n\ntype Other struct{}\n\ntype Server interface{}\n")
	if got := checkServerInfrastructureFields("internal/server/s.go", ifaceSyntax, ifaceFset); len(got) != 0 {
		t.Fatalf("got=%v", got)
	}
	// reqSyntax、reqFset 的 handler 含多种 var 形态。
	reqSyntax, reqFset := archParse(t, "x_handlers.go", `package server

func handler() {
	var plain string
	var other struct{ A int }
	var req struct{ A int }
	const c = 1
	_, _, _ = plain, other, req
}
`)
	if got := checkHTTPRequestContracts("internal/server/x_handlers.go", reqSyntax, reqFset); len(got) != 1 {
		t.Fatalf("got=%v", got)
	}
}

// TestCompositionProjectionSkipsUnrelatedImports 验证 composition 核心文件的无关导入不触发违规，只有 Server 导入才触发。
func TestCompositionProjectionSkipsUnrelatedImports(t *testing.T) {
	// syntax、fset 的文件先导入标准库再导入 Server。
	syntax, fset := archParse(t, "c.go", "package composition\n\nimport (\n\t\"fmt\"\n\t_ \"xianyu-go/internal/server\"\n)\n\nvar _ = fmt.Sprint\n")
	if got := checkStageTwoCompositionProjection("internal/composition/c.go", syntax, fset); len(got) != 1 {
		t.Fatalf("got=%v", got)
	}
	// cleanSyntax、cleanFset 的文件没有 Server 导入。
	cleanSyntax, cleanFset := archParse(t, "d.go", "package composition\n\nimport \"fmt\"\n\nvar _ = fmt.Sprint\n")
	if got := checkStageTwoCompositionProjection("internal/composition/d.go", cleanSyntax, cleanFset); got != nil {
		t.Fatalf("got=%v", got)
	}
}
