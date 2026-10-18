package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// maxResponseBytes 限制客户端读取的单个响应大小。
const maxResponseBytes = 16 << 20

// codeUpstream 是 stdio 桥接无法把请求转发到 HTTP 端点时回写的 JSON-RPC 错误码。
const codeUpstream = -32000

// Client 通过 HTTP 调用本服务的 /mcp 端点，供 SSH 登录本机后的命令行和 stdio 桥接使用。
type Client struct {
	// URL 是 /mcp 端点地址，例如 http://127.0.0.1:59188/mcp。
	URL string
	// Token 是 Bearer API Token。
	Token string
	// HTTP 发送请求；超时应略长于服务端 2 分钟的写超时，sync_items 可能持续数十秒。
	HTTP *http.Client
}

// ToolInfo 是 tools/list 返回的单个工具描述。
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	} `json:"inputSchema"`
	Annotations struct {
		ReadOnlyHint bool `json:"readOnlyHint"`
	} `json:"annotations"`
}

// post 把一条或一批 JSON-RPC 消息原样发到端点，返回响应正文；全部是通知时返回 nil。
// 非 200/202 状态返回包含服务端错误信息的 error。
func (c *Client) post(ctx context.Context, message []byte) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(message))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer "+c.Token)
	response, err := c.HTTP.Do(request)
	if err != nil {
		return nil, fmt.Errorf("连接 %s 失败（服务是否在运行？）: %w", c.URL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	switch response.StatusCode {
	case http.StatusAccepted:
		return nil, nil
	case http.StatusOK:
		return bytes.TrimSpace(body), nil
	}
	var failure rpcResponse
	if json.Unmarshal(body, &failure) == nil && failure.Error != nil {
		return nil, fmt.Errorf("HTTP %d：%s", response.StatusCode, failure.Error.Message)
	}
	return nil, fmt.Errorf("HTTP %d：%s", response.StatusCode, preview(string(body), 200))
}

// call 发送一次 JSON-RPC 请求并把 result 解码到 out。
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	message, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	body, err := c.post(ctx, message)
	if err != nil {
		return err
	}
	// Result 预置为 out，解码时直接写入调用方的结构，不再二次解析。
	response := rpcResponse{Result: out}
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("解析 %s 响应失败: %w", method, err)
	}
	if response.Error != nil {
		return fmt.Errorf("%s 失败：%s", method, response.Error.Message)
	}
	return nil
}

// ListTools 返回当前 Token 可用的工具。
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var result struct {
		Tools []ToolInfo `json:"tools"`
	}
	return result.Tools, c.call(ctx, "tools/list", map[string]any{}, &result)
}

// CallTool 调用工具并返回文本结果；arguments 为空时按无参数调用。isError 表示业务失败（例如参数不合法、库存不足）。
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (text string, isError bool, err error) {
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments}, &result); err != nil {
		return "", false, err
	}
	parts := make([]string, 0, len(result.Content))
	for _, content := range result.Content {
		parts = append(parts, content.Text)
	}
	return strings.Join(parts, "\n"), result.IsError, nil
}

// ServeStdio 实现 MCP stdio 传输：从 in 逐行读取 JSON-RPC 消息转发到端点，把响应逐行写到 out。
// 每条消息独立转发，慢工具（如 sync_items）不会阻塞后续请求；端点无会话状态，响应顺序不影响客户端按 ID 匹配。
// 转发失败时对带 ID 的请求回写 JSON-RPC 错误，通知则丢弃。in 结束后等待在途请求完成再返回。
func (c *Client) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	// mu 保护对 out 的写入与 writeErr，保证每条响应整行输出。
	var mu sync.Mutex
	var writeErr error
	var pending sync.WaitGroup
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), maxRequestBytes+1)
	for scanner.Scan() {
		line := bytes.Clone(bytes.TrimSpace(scanner.Bytes()))
		if len(line) == 0 {
			continue
		}
		pending.Go(func() {
			reply := c.forward(ctx, line)
			if len(reply) == 0 {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if writeErr == nil {
				_, writeErr = out.Write(append(reply, '\n'))
			}
		})
	}
	pending.Wait()
	if writeErr != nil {
		return writeErr
	}
	return scanner.Err()
}

// forward 转发一行消息并返回要回写的响应；通知或转发失败的通知返回 nil。
func (c *Client) forward(ctx context.Context, line []byte) []byte {
	reply, err := c.post(ctx, line)
	if err == nil {
		return reply
	}
	var message struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(line, &message) != nil || len(message.ID) == 0 || string(message.ID) == "null" {
		return nil
	}
	// ID 来自合法 JSON，其余字段是固定类型，编码不会失败。
	reply, _ = json.Marshal(rpcResponse{JSONRPC: "2.0", ID: message.ID, Error: &rpcError{Code: codeUpstream, Message: err.Error()}})
	return reply
}
