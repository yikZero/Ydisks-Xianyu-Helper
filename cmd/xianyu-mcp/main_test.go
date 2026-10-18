package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEndpoint 模拟 /mcp：只接受 Token "secret-token"，记录最后一次 tools/call 的参数。
type fakeEndpoint struct {
	lastArguments string
}

// ServeHTTP 按 JSON-RPC 方法返回固定结果。
func (f *fakeEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer secret-token" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32001,"message":"API Token 无效或已吊销"}}`)
		return
	}
	var message struct {
		Method string `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&message)
	switch message.Method {
	case "tools/list":
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"list_items","description":"列商品",
			"inputSchema":{"type":"object","properties":{"keyword":{"type":"string","description":"关键词"},"account_id":{"type":"string","description":"账号"}},"required":["account_id"]},
			"annotations":{"readOnlyHint":true}}]}}`)
	case "tools/call":
		f.lastArguments = string(message.Params.Arguments)
		isError := message.Params.Name == "bind_item_delivery"
		result, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "text", "text": "结果:" + message.Params.Name}}, "isError": isError})
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(result) + `}`))
	}
}

// TestRunSubcommands 覆盖 tools 输出、call 读取 stdin 参数、业务错误退出码、Token 文件与环境变量优先级以及用法错误。
func TestRunSubcommands(t *testing.T) {
	endpoint := &fakeEndpoint{}
	server := httptest.NewServer(endpoint)
	defer server.Close()
	home := t.TempDir()
	tokenPath := filepath.Join(home, ".config", "ydisks-xianyu-helper", "api-token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"HOME": home, "XIANYU_MCP_URL": server.URL}
	getenv := func(key string) string { return env[key] }
	invoke := func(stdin string, args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, getenv, strings.NewReader(stdin), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	code, out, _ := invoke("", "tools")
	if code != 0 || !strings.Contains(out, "list_items [只读]") || strings.Index(out, "account_id (string, 必填)") > strings.Index(out, "keyword (string, 可选)") {
		t.Fatalf("tools code=%d out=%q", code, out)
	}
	if code, out, _ = invoke(`{"keyword":"夸克"}`, "call", "list_items", "-"); code != 0 || out != "结果:list_items\n" || endpoint.lastArguments != `{"keyword":"夸克"}` {
		t.Fatalf("call code=%d out=%q args=%s", code, out, endpoint.lastArguments)
	}
	if code, _, _ = invoke("", "call", "list_cards"); code != 0 || endpoint.lastArguments != `{}` {
		t.Fatalf("省略参数应发送 {}，code=%d args=%s", code, endpoint.lastArguments)
	}
	if code, _, _ = invoke("", "call", "bind_item_delivery", `{}`); code != 1 {
		t.Fatalf("工具业务错误应退出 1，got %d", code)
	}
	if code, _, errOut := invoke("", "call", "list_items", `{bad`); code != 1 || !strings.Contains(errOut, "JSON") {
		t.Fatalf("非法 JSON code=%d err=%q", code, errOut)
	}
	env["XIANYU_API_TOKEN"] = "wrong-token"
	if code, _, errOut := invoke("", "tools"); code != 1 || !strings.Contains(errOut, "HTTP 401") || strings.Contains(errOut, "wrong-token") {
		t.Fatalf("环境变量 Token 应优先且错误不回显 Token: code=%d err=%q", code, errOut)
	}
	delete(env, "XIANYU_API_TOKEN")
	if code, _, errOut := invoke("", "-token-file", filepath.Join(home, "missing"), "tools"); code != 1 || !strings.Contains(errOut, "api-token-create") {
		t.Fatalf("缺少 Token 文件 code=%d err=%q", code, errOut)
	}
	for _, args := range [][]string{{}, {"call"}, {"unknown"}} {
		if code, _, errOut := invoke("", args...); code != 2 || !strings.Contains(errOut, "用法") {
			t.Fatalf("%v 应输出用法并退出 2: code=%d err=%q", args, code, errOut)
		}
	}
}
