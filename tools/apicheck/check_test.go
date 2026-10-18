package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
)

// writeSpec 在临时仓库根目录写入 api/openapi.yaml 并返回根目录，用作 check 的夹具。
func writeSpec(t *testing.T, content string) string {
	t.Helper()
	// root 是临时仓库根目录。
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "api", "openapi.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// compliantSpec 是满足 operationId、成功响应、统一错误响应与鉴权元数据全部约束的最小文档。
const compliantSpec = `openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /api/v1/ping:
    get:
      operationId: ping
      security: [{}]
      responses:
        "200": {description: ok}
        "400": {description: bad}
        "401": {description: unauthorized}
`

// TestCheckAcceptsCompliantDocument 验证合规文档没有违规，并驱动 main 的通过分支。
func TestCheckAcceptsCompliantDocument(t *testing.T) {
	// root 是合规文档所在仓库根。
	root := writeSpec(t, compliantSpec)
	// violations、err 是检查结果。
	violations, err := check(root)
	if err != nil || len(violations) != 0 {
		t.Fatalf("violations=%v err=%v", violations, err)
	}
	// original 是替换前的命令行参数。
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	os.Args = []string{"apicheck", root}
	main()
}

// TestCheckReportsEveryMissingContractPiece 验证缺少 operationId、成功响应、统一错误响应、鉴权元数据都会被逐项报告。
func TestCheckReportsEveryMissingContractPiece(t *testing.T) {
	// root 的文档中 operation 只有一个 500 响应且没有 operationId 与 security。
	root := writeSpec(t, `openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /api/v1/bad:
    get:
      responses:
        "500": {description: boom}
`)
	// violations、err 是检查结果。
	violations, err := check(root)
	if err != nil {
		t.Fatal(err)
	}
	// joined 是全部违规拼接文本，便于逐项断言。
	joined := strings.Join(violations, "\n")
	// want 是每个必须出现的违规片段。
	for _, want := range []string{"缺少 operationId", "缺少成功响应", "缺少统一错误响应", "缺少鉴权元数据"} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少违规 %q: %s", want, joined)
		}
	}
}

// TestCheckReportsExceptionViolations 验证特殊成功校验类别非法时违规带上方法与路径。
func TestCheckReportsExceptionViolations(t *testing.T) {
	// root 的文档登记了不受支持的特殊类别。
	root := writeSpec(t, `openapi: 3.0.3
info: {title: t, version: "1"}
paths:
  /api/v1/x:
    get:
      operationId: x
      security: [{}]
      x-contract-success-exception: {kind: other}
      responses:
        "400": {description: bad}
        "401": {description: unauthorized}
`)
	// violations、err 是检查结果。
	violations, err := check(root)
	if err != nil || len(violations) != 1 || !strings.Contains(violations[0], "GET /api/v1/x") {
		t.Fatalf("violations=%v err=%v", violations, err)
	}
}

// TestCheckLoadAndValidateFailures 验证文件缺失与规范无效分别返回加载、校验错误。
func TestCheckLoadAndValidateFailures(t *testing.T) {
	if _, err := check(t.TempDir()); err == nil || !strings.Contains(err.Error(), "加载 OpenAPI 失败") {
		t.Fatalf("err=%v", err)
	}
	// root 的文档缺少必需的 info.title，语义校验失败。
	root := writeSpec(t, "openapi: 3.0.3\ninfo: {version: \"1\"}\npaths: {}\n")
	if _, err := check(root); err == nil || !strings.Contains(err.Error(), "规范无效") {
		t.Fatalf("err=%v", err)
	}
}

// TestContractSuccessExceptionKindBranches 覆盖 nil operation、无扩展、kind 非字符串与正常读取。
func TestContractSuccessExceptionKindBranches(t *testing.T) {
	if contractSuccessExceptionKind(nil) != "" || contractSuccessExceptionKind(&openapi3.Operation{}) != "" {
		t.Fatal("空 operation 应返回空类别")
	}
	// noKey 有扩展 map 但没有登记特殊校验。
	noKey := &openapi3.Operation{Extensions: map[string]any{"x-other": 1}}
	if contractSuccessExceptionKind(noKey) != "" {
		t.Fatal("未登记应返回空类别")
	}
	// badKind 的 kind 不是字符串。
	badKind := &openapi3.Operation{Extensions: map[string]any{"x-contract-success-exception": map[string]any{"kind": 3}}}
	if contractSuccessExceptionKind(badKind) != "" {
		t.Fatal("非字符串 kind 应返回空类别")
	}
	// violations 是下方语句引入的测试值，承载被测调用的结果或夹具，仅在当前测试内使用。
	if violations := validateContractSuccessException(badKind); len(violations) != 1 || !strings.Contains(violations[0], "必须是字符串") {
		t.Fatalf("violations=%v", violations)
	}
}

// TestValidateContractSuccessExceptionNilAndMissing 覆盖 nil operation、无扩展与未登记扩展时无违规。
func TestValidateContractSuccessExceptionNilAndMissing(t *testing.T) {
	if validateContractSuccessException(nil) != nil || validateContractSuccessException(&openapi3.Operation{}) != nil {
		t.Fatal("空 operation 不应有违规")
	}
	if validateContractSuccessException(&openapi3.Operation{Extensions: map[string]any{"x-other": 1}}) != nil {
		t.Fatal("未登记扩展不应有违规")
	}
}

// TestValidateContractSuccessExceptionNilResponses 覆盖各类别在 Responses 为 nil 时的拒绝。
func TestValidateContractSuccessExceptionNilResponses(t *testing.T) {
	// kind 是被测试的特殊校验类别。
	for _, kind := range []string{"websocket", "binary", "permanently_disabled"} {
		// operation 没有任何响应定义。
		operation := &openapi3.Operation{Extensions: map[string]any{"x-contract-success-exception": map[string]any{"kind": kind}}}
		if len(validateContractSuccessException(operation)) == 0 {
			t.Errorf("%s 缺少响应应被拒绝", kind)
		}
	}
}

// TestHasBinarySuccessResponseBranches 覆盖 nil、nil Value、JSON 类型的 binary 与缺失 schema 情形。
func TestHasBinarySuccessResponseBranches(t *testing.T) {
	if hasBinarySuccessResponse(nil) || hasSuccessResponse(nil) {
		t.Fatal("nil 响应集合不应判定为成功")
	}
	// jsonBinary 的 binary schema 声明在 application/json 下，不能算二进制下载。
	jsonBinary := openapi3.NewResponses(openapi3.WithStatus(200, &openapi3.ResponseRef{Value: openapi3.NewResponse().WithContent(openapi3.Content{
		"application/json": &openapi3.MediaType{Schema: &openapi3.SchemaRef{Value: &openapi3.Schema{Format: "binary"}}},
	})}))
	if hasBinarySuccessResponse(jsonBinary) {
		t.Fatal("application/json 不应算二进制")
	}
	// noSchema 的媒体类型没有 schema，也不是二进制。
	noSchema := openapi3.NewResponses(openapi3.WithStatus(200, &openapi3.ResponseRef{Value: openapi3.NewResponse().WithContent(openapi3.Content{"text/csv": &openapi3.MediaType{}})}))
	if hasBinarySuccessResponse(noSchema) {
		t.Fatal("缺少 schema 不应算二进制")
	}
	// nilValue 的响应引用没有 Value。
	nilValue := openapi3.NewResponses(openapi3.WithStatus(200, &openapi3.ResponseRef{}))
	if hasBinarySuccessResponse(nilValue) {
		t.Fatal("nil Value 不应算二进制")
	}
	if hasSuccessResponse(openapi3.NewResponses(openapi3.WithStatus(500, &openapi3.ResponseRef{Value: openapi3.NewResponse()}))) {
		t.Fatal("仅 500 不应算成功")
	}
	if !hasSuccessResponse(openapi3.NewResponses(openapi3.WithStatus(101, &openapi3.ResponseRef{Value: openapi3.NewResponse()}))) {
		t.Fatal("101 应算成功")
	}
}
