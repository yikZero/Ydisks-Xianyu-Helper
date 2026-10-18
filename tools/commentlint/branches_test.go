package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// runMainWith 用独立的 FlagSet 和给定参数运行 main，结束后恢复全局 flag 与 os.Args，只用于不会 os.Exit 的路径。
func runMainWith(t *testing.T, args ...string) {
	t.Helper()
	// originalArgs、originalFlags 是测试前的命令行与全局 FlagSet。
	originalArgs, originalFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args, flag.CommandLine = originalArgs, originalFlags })
	flag.CommandLine = flag.NewFlagSet("commentlint", flag.ContinueOnError)
	os.Args = append([]string{"commentlint"}, args...)
	main()
}

// cleanFixtureTree 创建一棵导出声明均带文档注释的夹具树并返回根目录。
func cleanFixtureTree(t *testing.T) string {
	t.Helper()
	// root 是夹具根目录。
	root := t.TempDir()
	writeFixture(t, root, "pkg/ok.go", "// Package pkg 是夹具包。\npackage pkg\n\n// Answer 是夹具常量。\nconst Answer = 42\n")
	return root
}

// TestMainCheckModePassesOnCleanTree 验证干净夹具树在 check 模式下通过且不退出进程。
func TestMainCheckModePassesOnCleanTree(t *testing.T) {
	runMainWith(t, "-mode", "check", "-root", cleanFixtureTree(t))
}

// TestMainTemplateAuditModeReportsFindings 验证 template-audit 模式输出模板化注释计数并正常返回。
func TestMainTemplateAuditModeReportsFindings(t *testing.T) {
	// root 的文件含一条泛化的错误说明模板注释。
	root := t.TempDir()
	writeFixture(t, root, "pkg/tpl.go", "package pkg\n\n// err 表示错误\nvar V = 1\n")
	runMainWith(t, "-mode", "template-audit", "-root", root)
}

// TestCollectFunctionsFailOnMissingRoot 验证根目录不存在时两个收集函数都返回遍历错误。
func TestCollectFunctionsFailOnMissingRoot(t *testing.T) {
	// missing 是不存在的根目录。
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := collectFindings(missing); err == nil {
		t.Fatal("collectFindings 应失败")
	}
	if _, err := collectTemplateCommentFindings(missing); err == nil {
		t.Fatal("collectTemplateCommentFindings 应失败")
	}
}

// TestCollectFunctionsFailOnUnparsableGoFile 验证无法解析的 Go 文件使收集函数返回解析错误。
func TestCollectFunctionsFailOnUnparsableGoFile(t *testing.T) {
	// root 含一个语法错误文件。
	root := t.TempDir()
	writeFixture(t, root, "broken.go", "package broken\nfunc (")
	if _, err := collectFindings(root); err == nil {
		t.Fatal("collectFindings 应报告解析错误")
	}
	if _, err := collectTemplateCommentFindings(root); err == nil {
		t.Fatal("collectTemplateCommentFindings 应报告解析错误")
	}
	if _, err := templateFindingsInGoFile(root, filepath.Join(root, "broken.go")); err == nil {
		t.Fatal("templateFindingsInGoFile 应报告解析错误")
	}
	if _, err := inspectGoFile(root, filepath.Join(root, "broken.go")); err == nil {
		t.Fatal("inspectGoFile 应报告解析错误")
	}
}

// TestCollectTemplateSkipsFrozenFiles 验证模板化注释审计会跳过冻结的 CAPTCHA 文件。
func TestCollectTemplateSkipsFrozenFiles(t *testing.T) {
	// root 的冻结文件含模板注释，但不得被报告。
	root := t.TempDir()
	writeFixture(t, root, "internal/browser/slider.go", "package browser\n\n// err 表示错误\nvar V = 1\n")
	// findings、err 是审计结果。
	findings, err := collectTemplateCommentFindings(root)
	if err != nil || len(findings) != 0 {
		t.Fatalf("findings=%v err=%v", findings, err)
	}
}

// TestPathHelpersTolerateRelativeRootMismatch 验证相对根与绝对路径混用导致 filepath.Rel 失败时各函数的降级行为。
func TestPathHelpersTolerateRelativeRootMismatch(t *testing.T) {
	// root 是真实夹具目录；relativeRoot 是与绝对路径无法求相对关系的根。
	root := t.TempDir()
	writeFixture(t, root, "a.go", "package a\n")
	// file 是夹具文件绝对路径。
	file := filepath.Join(root, "a.go")
	if shouldSkipDirectory(file, "relative-root") {
		t.Fatal("Rel 失败时不应跳过目录")
	}
	if _, err := inspectGoFile("relative-root", file); err == nil {
		t.Fatal("Rel 失败应返回错误")
	}
	if _, err := templateFindingsInGoFile("relative-root", file); err == nil {
		t.Fatal("Rel 失败应返回错误")
	}
}
