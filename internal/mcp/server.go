// Package mcp 实现 Model Context Protocol（MCP）的 Streamable HTTP 端点，供 AI 客户端用 API Token
// 管理账号商品、卡密库存和自动发货规则。
//
// 工具不直接访问数据库：每次调用都以 Token 所属用户身份在进程内转发到现有 /api/v1 管理接口，
// 复用其参数校验、归属检查和事务边界。服务端无状态，只支持 POST 的 JSON 响应模式。
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// latestProtocolVersion 是本服务实现的 MCP 协议版本；客户端请求其他受支持版本时原样协商。
const latestProtocolVersion = "2025-06-18"

// supportedProtocolVersions 是可协商的协议版本；本服务只用到三个版本共有的工具能力。
var supportedProtocolVersions = map[string]bool{"2025-06-18": true, "2025-03-26": true, "2024-11-05": true}

// maxRequestBytes 限制单个 MCP 请求体大小，足够一次追加数千条卡密。
const maxRequestBytes = 2 << 20

// JSON-RPC 错误码。
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeUnauthorized   = -32001
)

// Principal 是 API Token 认证后的调用者身份。
type Principal struct {
	// TokenID 用于审计日志定位是哪个 Token 发起的调用。
	TokenID int64
	// UserID 是 Token 所属用户，所有工具都以该用户身份访问数据。
	UserID int64
	// Write 为 false 时只允许调用只读工具。
	Write bool
}

// Handler 是 MCP 的 HTTP 端点。所有字段都是必需依赖，由服务端组装时提供。
type Handler struct {
	// Authenticate 校验 Bearer Token，返回调用者身份和携带该身份的 Context；API 收到该 Context 的请求时视为该用户已登录。
	// 失败时返回非 nil 错误。
	Authenticate func(ctx context.Context, token string) (context.Context, Principal, error)
	// API 是完整的管理接口路由，工具调用在进程内转发到这里。
	API http.Handler
	// Logger 记录工具调用审计日志，不记录参数内容（可能包含卡密）。
	Logger *slog.Logger
	// Version 是服务端版本，出现在 initialize 结果中。
	Version string
}

// rpcMessage 是单条 JSON-RPC 消息；ID 缺失表示通知，不需要响应。
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse 是 JSON-RPC 响应；Result 与 Error 二选一。
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError 是 JSON-RPC 错误对象。
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ServeHTTP 处理 MCP Streamable HTTP 请求：只接受带 Bearer Token 的 POST。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "MCP 端点只支持 POST", http.StatusMethodNotAllowed)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		writeUnauthorized(w, "缺少 Authorization: Bearer <API Token>")
		return
	}
	ctx, principal, err := h.Authenticate(r.Context(), strings.TrimSpace(token))
	if err != nil {
		writeUnauthorized(w, "API Token 无效或已吊销")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil || len(body) > maxRequestBytes {
		writeRPCError(w, http.StatusRequestEntityTooLarge, codeInvalidRequest, "请求体过大或读取失败")
		return
	}
	body = bytes.TrimSpace(body)
	batch := len(body) > 0 && body[0] == '['
	var messages []rpcMessage
	if batch {
		err = json.Unmarshal(body, &messages)
	} else {
		messages = make([]rpcMessage, 1)
		err = json.Unmarshal(body, &messages[0])
	}
	if err != nil || len(messages) == 0 {
		writeRPCError(w, http.StatusBadRequest, codeParseError, "JSON-RPC 请求格式错误")
		return
	}
	responses := make([]rpcResponse, 0, len(messages))
	for _, message := range messages {
		if response, ok := h.handleMessage(ctx, principal, message); ok {
			responses = append(responses, response)
		}
	}
	switch {
	case len(responses) == 0:
		w.WriteHeader(http.StatusAccepted)
	case batch:
		writeJSON(w, http.StatusOK, responses)
	default:
		writeJSON(w, http.StatusOK, responses[0])
	}
}

// handleMessage 处理单条消息；返回 false 表示这是通知或客户端响应，不需要回复。
func (h *Handler) handleMessage(ctx context.Context, principal Principal, message rpcMessage) (rpcResponse, bool) {
	if len(message.ID) == 0 || string(message.ID) == "null" {
		return rpcResponse{}, false
	}
	response := rpcResponse{JSONRPC: "2.0", ID: message.ID}
	if message.JSONRPC != "2.0" || message.Method == "" {
		response.Error = &rpcError{Code: codeInvalidRequest, Message: "不是合法的 JSON-RPC 2.0 请求"}
		return response, true
	}
	switch message.Method {
	case "initialize":
		response.Result = h.initializeResult(message.Params)
	case "ping":
		response.Result = struct{}{}
	case "tools/list":
		response.Result = map[string]any{"tools": toolDescriptors(principal.Write)}
	case "tools/call":
		result, err := h.callTool(ctx, principal, message.Params)
		if err != nil {
			response.Error = err
		} else {
			response.Result = result
		}
	default:
		response.Error = &rpcError{Code: codeMethodNotFound, Message: "不支持的方法：" + message.Method}
	}
	return response, true
}

// initializeResult 协商协议版本并声明只提供工具能力。
func (h *Handler) initializeResult(params json.RawMessage) map[string]any {
	var request struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &request)
	version := latestProtocolVersion
	if supportedProtocolVersions[request.ProtocolVersion] {
		version = request.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": "ydisks-xianyu-helper", "version": h.Version},
		"instructions":    serverInstructions,
	}
}

// callTool 执行 tools/call；ctx 携带 Authenticate 返回的可信身份。参数或工具名错误返回协议错误；业务失败以 isError 结果返回给模型。
func (h *Handler) callTool(ctx context.Context, principal Principal, params json.RawMessage) (map[string]any, *rpcError) {
	var request struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "tools/call 参数格式错误"}
	}
	definition, ok := toolsByName[request.Name]
	if !ok {
		return nil, &rpcError{Code: codeInvalidParams, Message: "未知工具：" + request.Name}
	}
	started := time.Now()
	var result any
	var err error
	if definition.write && !principal.Write {
		err = errors.New("当前 API Token 只有只读权限，不能调用会修改数据的工具；请使用 -api-token-scope write 创建的 Token")
	} else {
		arguments := request.Arguments
		if len(arguments) == 0 || string(arguments) == "null" {
			arguments = json.RawMessage("{}")
		}
		result, err = definition.run(apiClient{handler: h.API, ctx: ctx}, arguments)
	}
	h.Logger.Info("MCP 工具调用", "tool", request.Name, "mcp_client_id", principal.TokenID, "user_id", principal.UserID,
		"ok", err == nil, "duration", time.Since(started).Round(time.Millisecond))
	if err != nil {
		return toolText(err.Error(), true), nil
	}
	encoded, encodeErr := json.MarshalIndent(result, "", "  ")
	if encodeErr != nil {
		return toolText("结果编码失败："+encodeErr.Error(), true), nil
	}
	return toolText(string(encoded), false), nil
}

// toolText 构造只含一段文本的工具结果。
func toolText(text string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isError}
}

// writeUnauthorized 返回 401，并按 MCP 授权约定附带 WWW-Authenticate。
func writeUnauthorized(w http.ResponseWriter, message string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="ydisks-xianyu-helper"`)
	writeRPCError(w, http.StatusUnauthorized, codeUnauthorized, message)
}

// writeRPCError 写一个无请求 ID 的 JSON-RPC 错误响应，用于无法定位到具体消息的传输层失败。
func writeRPCError(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: code, Message: message}})
}

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
