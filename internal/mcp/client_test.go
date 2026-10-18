package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestEndpoint 启动只认 Token "good"（只读）的 MCP 端点，管理接口只提供卡密列表。
func newTestEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/cards" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, []cardRow{{ID: 7, Name: "夸克合集", Type: "text", TextContent: "链接", Enabled: true}})
	})
	server := httptest.NewServer(&Handler{
		Authenticate: func(ctx context.Context, token string) (context.Context, Principal, error) {
			if token != "good" {
				return nil, Principal{}, errors.New("bad token")
			}
			return ctx, Principal{TokenID: 1, UserID: 1}, nil
		},
		API:    api,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t.Cleanup(server.Close)
	return server
}

// TestClientListsAndCallsTools 覆盖客户端列工具、调用只读工具、写工具被拒和 Token 无效。
func TestClientListsAndCallsTools(t *testing.T) {
	server := newTestEndpoint(t)
	client := &Client{URL: server.URL, Token: "good", HTTP: server.Client()}
	ctx := context.Background()

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if !tool.Annotations.ReadOnlyHint {
			t.Fatalf("只读 Token 不应看到写工具 %s", tool.Name)
		}
	}
	if len(tools) == 0 || tools[0].Name != "list_accounts" {
		t.Fatalf("tools=%+v", tools)
	}

	text, isError, err := client.CallTool(ctx, "list_cards", nil)
	if err != nil || isError || !strings.Contains(text, "夸克合集") {
		t.Fatalf("text=%q isError=%v err=%v", text, isError, err)
	}
	if text, isError, err = client.CallTool(ctx, "create_card", json.RawMessage(`{}`)); err != nil || !isError || !strings.Contains(text, "只读") {
		t.Fatalf("只读 Token 调用写工具应返回业务错误: text=%q isError=%v err=%v", text, isError, err)
	}
	if _, _, err = client.CallTool(ctx, "no_such_tool", nil); err == nil || !strings.Contains(err.Error(), "未知工具") {
		t.Fatalf("未知工具应返回协议错误: %v", err)
	}

	bad := &Client{URL: server.URL, Token: "wrong", HTTP: server.Client()}
	if _, err := bad.ListTools(ctx); err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("无效 Token 应返回 401 且不回显 Token: %v", err)
	}
}

// TestServeStdioForwardsLines 验证 stdio 桥接逐行转发、通知不回写，转发失败时按请求 ID 回写错误。
func TestServeStdioForwardsLines(t *testing.T) {
	server := newTestEndpoint(t)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		``,
		`{"jsonrpc":"2.0","id":"b","method":"ping"}`,
	}, "\n")
	var out bytes.Buffer
	client := &Client{URL: server.URL, Token: "good", HTTP: server.Client()}
	if err := client.ServeStdio(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	replies := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var response rpcResponse
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("每行应是一条完整响应: %q", line)
		}
		replies[string(response.ID)] = line
	}
	if len(replies) != 2 || !strings.Contains(replies["1"], `"protocolVersion":"2025-03-26"`) || replies[`"b"`] == "" {
		t.Fatalf("stdio 输出=%q", out.String())
	}

	out.Reset()
	bad := &Client{URL: server.URL, Token: "wrong", HTTP: server.Client()}
	input = `{"jsonrpc":"2.0","id":9,"method":"ping"}` + "\n" + `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	if err := bad.ServeStdio(context.Background(), strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	var response rpcResponse
	if err := json.Unmarshal(out.Bytes(), &response); err != nil || string(response.ID) != "9" || response.Error == nil || response.Error.Code != codeUpstream {
		t.Fatalf("转发失败应按请求 ID 回写错误且通知不回写: %q err=%v", out.String(), err)
	}
}

// pingWatcher 在写出 ID 为 2 的响应时关闭 seen，用来确认慢请求未完成前后续请求已得到响应。
type pingWatcher struct {
	bytes.Buffer
	seen chan struct{}
}

// Write 记录输出并检测 ping 响应。
func (w *pingWatcher) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte(`"id":2`)) {
		close(w.seen)
	}
	return w.Buffer.Write(data)
}

// TestServeStdioDoesNotBlockOnSlowTool 验证慢工具调用期间，后续请求仍能先得到响应。
func TestServeStdioDoesNotBlockOnSlowTool(t *testing.T) {
	out := &pingWatcher{seen: make(chan struct{})}
	server := httptest.NewServer(&Handler{
		Authenticate: func(ctx context.Context, _ string) (context.Context, Principal, error) { return ctx, Principal{}, nil },
		API: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			select {
			case <-out.seen:
				writeJSON(w, http.StatusOK, []cardRow{})
			case <-time.After(5 * time.Second):
				http.Error(w, "ping 被慢请求阻塞", http.StatusGatewayTimeout)
			}
		}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer server.Close()
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_cards"}}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"
	client := &Client{URL: server.URL, Token: "any", HTTP: server.Client()}
	if err := client.ServeStdio(context.Background(), strings.NewReader(input), out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "阻塞") || strings.Count(out.String(), "\n") != 2 {
		t.Fatalf("输出=%q", out.String())
	}
}
