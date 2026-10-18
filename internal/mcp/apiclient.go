package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// apiClient 以已认证用户身份在进程内调用管理接口，不经过网络。
type apiClient struct {
	// handler 是完整的管理接口路由。
	handler http.Handler
	// ctx 携带可信会话，并沿用 MCP 请求的取消信号。
	ctx context.Context
}

// responseRecorder 是收集内部调用响应的最小 http.ResponseWriter。
type responseRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

// Header 返回响应头集合。
func (r *responseRecorder) Header() http.Header { return r.header }

// Write 收集响应正文；未显式写状态时按 200 处理。
func (r *responseRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(data)
}

// WriteHeader 记录第一次写入的状态码。
func (r *responseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

// do 调用 method path；body 非 nil 时编码为 JSON 请求体，out 非 nil 时解码 2xx 响应。
// 非 2xx 响应转换为包含接口错误信息的 error，供工具结果直接展示给模型。
func (c apiClient) do(method, path string, query url.Values, body, out any) error {
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader = http.NoBody
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(c.ctx, method, "http://mcp.internal"+target, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := &responseRecorder{header: http.Header{}}
	c.handler.ServeHTTP(recorder, request)
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	if recorder.status < 200 || recorder.status >= 300 {
		return fmt.Errorf("%s %s 失败（HTTP %d）：%s", method, path, recorder.status, apiErrorMessage(recorder.body.Bytes()))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(recorder.body.Bytes(), out); err != nil {
		return fmt.Errorf("解析 %s %s 响应失败: %w", method, path, err)
	}
	return nil
}

// apiErrorMessage 从统一错误信封中提取可读信息；无法解析时返回截断后的原文。
func apiErrorMessage(body []byte) string {
	var envelope struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		for _, text := range []string{envelope.Message, envelope.Detail, envelope.Error} {
			if strings.TrimSpace(text) != "" {
				return text
			}
		}
	}
	text := strings.TrimSpace(string(body))
	if len([]rune(text)) > 200 {
		text = string([]rune(text)[:200]) + "…"
	}
	return text
}
