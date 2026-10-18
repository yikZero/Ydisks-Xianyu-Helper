package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"

	"xianyu-go/internal/httpapi"
)

// apiClient 以已认证用户身份在进程内调用管理接口，不经过网络。
type apiClient struct {
	// handler 是完整的管理接口路由。
	handler http.Handler
	// ctx 携带可信会话，并沿用 MCP 请求的取消信号。
	ctx context.Context
	// imageDir 是 publish_item 可读取本地图片的绝对目录；为空表示只接受图片 URL。
	imageDir string
}

// formFile 是 multipart 请求中的一个文件字段。
type formFile struct {
	field       string
	filename    string
	contentType string
	data        []byte
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
	if body == nil {
		return c.send(method, target, "", http.NoBody, out)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.send(method, target, "application/json", bytes.NewReader(encoded), out)
}

// postForm 以 multipart/form-data 调用 POST path，用于需要上传图片的接口。
func (c apiClient) postForm(path string, fields map[string]string, files []formFile, out any) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			return err
		}
	}
	for _, file := range files {
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": file.field, "filename": file.filename}))
		header.Set("Content-Type", file.contentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			return err
		}
		if _, err := part.Write(file.data); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return c.send(http.MethodPost, path, writer.FormDataContentType(), &body, out)
}

// send 在进程内执行请求并处理响应状态与解码。
func (c apiClient) send(method, target, contentType string, body io.Reader, out any) error {
	request, err := http.NewRequestWithContext(c.ctx, method, "http://mcp.internal"+target, body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := &responseRecorder{header: http.Header{}}
	c.handler.ServeHTTP(recorder, request)
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	path, _, _ := strings.Cut(target, "?")
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
	var envelope httpapi.ErrorResponse
	if json.Unmarshal(body, &envelope) == nil && strings.TrimSpace(envelope.Message) != "" {
		if len(envelope.Details) == 0 {
			return envelope.Message
		}
		// Details 携带恢复所需的事实（例如已发布商品的 ID），一并展示，避免模型重复操作。
		details, _ := json.Marshal(envelope.Details)
		return envelope.Message + " " + string(details)
	}
	return preview(string(body), 200)
}
