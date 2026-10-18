package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// buildMultipartFileRequest 构造指定文件字节数的真实 multipart 请求，用于验证文件上限与请求包装开销分离。
func buildMultipartFileRequest(t *testing.T, fieldName, filename string, size int64) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	// body 保存 multipart writer 编码出的完整请求字节。
	var body bytes.Buffer
	// writer 负责生成真实 boundary、文件头和结束分隔符。
	writer := multipart.NewWriter(&body)
	// fileWriter、createErr 保存目标文件字段写入器及其创建错误。
	fileWriter, createErr := writer.CreateFormFile(fieldName, filename)
	if createErr != nil {
		t.Fatalf("create multipart file: %v", createErr)
	}
	// content 保存精确指定大小的文件内容；重复字节避免测试数据额外引入编码差异。
	content := bytes.Repeat([]byte{'x'}, int(size))
	// writeErr 保存向 multipart 文件字段写入精确大小测试字节时的错误。
	if _, writeErr := fileWriter.Write(content); writeErr != nil {
		t.Fatalf("write multipart file: %v", writeErr)
	}
	// closeErr 保存写出 multipart 结束 boundary 时的错误。
	if closeErr := writer.Close(); closeErr != nil {
		t.Fatalf("close multipart file: %v", closeErr)
	}
	// request 是带完整 Content-Type boundary 的 HTTP 上传请求。
	request := httptest.NewRequest(http.MethodPost, "/upload", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	// recorder 保存共享 multipart 解析器分类错误时的响应。
	recorder := httptest.NewRecorder()
	return request, recorder
}

// TestMultipartParseErrorsRemainClassified 验证格式错误、boundary 损坏和总请求超限返回不同错误，避免统一提示掩盖根因。
func TestMultipartParseErrorsRemainClassified(t *testing.T) {
	// wrongMediaRequest、wrongMediaRecorder 保存非 multipart 上传请求及其分类错误响应。
	wrongMediaRequest := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(`{}`))
	wrongMediaRequest.Header.Set("Content-Type", "application/json")
	// wrongMediaRecorder 保存非 multipart 请求被拒绝后的统一错误响应。
	wrongMediaRecorder := httptest.NewRecorder()
	if parseMultipartRequest(wrongMediaRecorder, wrongMediaRequest, maxChatImageMultipartBytes, maxChatImageMultipartBytes, "图片上传内容不能超过 11 MiB") {
		t.Fatal("non-multipart request unexpectedly parsed")
	}
	if !strings.Contains(wrongMediaRecorder.Body.String(), "请求格式错误") {
		t.Fatalf("wrong media response=%s", wrongMediaRecorder.Body.String())
	}

	// brokenRequest、brokenRecorder 保存声明错误 boundary 的 multipart 请求及其分类响应。
	brokenRequest := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader("--different\r\n"))
	brokenRequest.Header.Set("Content-Type", "multipart/form-data; boundary=expected")
	// brokenRecorder 保存 boundary 与请求体不一致时的统一错误响应。
	brokenRecorder := httptest.NewRecorder()
	if parseMultipartRequest(brokenRecorder, brokenRequest, maxChatImageMultipartBytes, maxChatImageMultipartBytes, "图片上传内容不能超过 11 MiB") {
		t.Fatal("broken boundary request unexpectedly parsed")
	}
	if !strings.Contains(brokenRecorder.Body.String(), "上传表单损坏") {
		t.Fatalf("broken boundary response=%s", brokenRecorder.Body.String())
	}

	// oversizedRequest、oversizedRecorder 保存有效 multipart 但总体超过聊天上传总配额的请求及其分类响应。
	oversizedRequest, oversizedRecorder := buildMultipartFileRequest(t, "image", "too-large.png", maxChatImageMultipartBytes)
	if parseMultipartRequest(oversizedRecorder, oversizedRequest, maxChatImageMultipartBytes, maxChatImageMultipartBytes, "图片上传内容不能超过 11 MiB") {
		t.Fatal("oversized multipart request unexpectedly parsed")
	}
	if !strings.Contains(oversizedRecorder.Body.String(), "图片上传内容不能超过 11 MiB") {
		t.Fatalf("oversized response=%s", oversizedRecorder.Body.String())
	}
}
