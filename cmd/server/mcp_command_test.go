package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xianyu-go/internal/mcp"
)

// TestRunMCPCommand 对真实 MCP 端点覆盖 tools 输出、call 读取 stdin 参数、业务错误退出码、Token 来源优先级和用法错误。
func TestRunMCPCommand(t *testing.T) {
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/items":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"cookie_id": "a", "item_id": "1", "item_title": "夸克网盘合集"}, {"cookie_id": "a", "item_id": "2", "item_title": "其他"}})
		case "/api/v1/cards":
			_, _ = io.WriteString(w, "[]")
		default:
			http.NotFound(w, r)
		}
	})
	server := httptest.NewServer(&mcp.Handler{
		Authenticate: func(ctx context.Context, token string) (context.Context, mcp.Principal, error) {
			if token != "secret-token" {
				return nil, mcp.Principal{}, errors.New("bad token")
			}
			return ctx, mcp.Principal{TokenID: 1, UserID: 1}, nil
		},
		API:    api,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "api-token")
	if err := os.WriteFile(tokenFile, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XIANYU_SERVICE_URL", server.URL+"/")
	t.Setenv("XIANYU_API_TOKEN", "")
	invoke := func(stdin string, args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := runMCPCommand(context.Background(), append([]string{"-token-file", tokenFile}, args...), strings.NewReader(stdin), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	code, out, _ := invoke("", "tools")
	required, optional := strings.Index(out, "account_id (string, 必填)"), strings.Index(out, "quantity (integer, 可选)")
	if code != 0 || !strings.Contains(out, "list_items [只读]") || strings.Contains(out, "[写]") || required < 0 || optional < required {
		t.Fatalf("tools code=%d out=%q", code, out)
	}
	if code, out, _ = invoke(`{"keyword":"夸克"}`, "call", "list_items", "-"); code != 0 || !strings.Contains(out, "夸克网盘合集") || strings.Contains(out, "其他") {
		t.Fatalf("call code=%d out=%q", code, out)
	}
	if code, out, _ = invoke("", "call", "list_cards"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("无参数调用 code=%d out=%q", code, out)
	}
	if code, out, _ = invoke("", "call", "create_card", `{}`); code != 1 || !strings.Contains(out, "只读") {
		t.Fatalf("工具业务错误应退出 1: code=%d out=%q", code, out)
	}
	if code, _, errOut := invoke("", "call", "list_items", `{bad`); code != 1 || !strings.Contains(errOut, "JSON") {
		t.Fatalf("非法 JSON code=%d err=%q", code, errOut)
	}
	t.Setenv("XIANYU_API_TOKEN", "wrong-token")
	if code, _, errOut := invoke("", "tools"); code != 1 || !strings.Contains(errOut, "HTTP 401") || strings.Contains(errOut, "wrong-token") {
		t.Fatalf("环境变量 Token 应优先且错误不回显 Token: code=%d err=%q", code, errOut)
	}
	t.Setenv("XIANYU_API_TOKEN", "")
	var stderr bytes.Buffer
	if code := runMCPCommand(context.Background(), []string{"-token-file", filepath.Join(t.TempDir(), "missing"), "tools"}, nil, io.Discard, &stderr); code != 1 || !strings.Contains(stderr.String(), "-api-token-save") {
		t.Fatalf("缺少 Token 文件 code=%d err=%q", code, stderr.String())
	}
	for _, args := range [][]string{{}, {"call"}, {"unknown"}} {
		if code, _, errOut := invoke("", args...); code != 2 || !strings.Contains(errOut, "用法") {
			t.Fatalf("%v 应输出用法并退出 2: code=%d err=%q", args, code, errOut)
		}
	}
}
