package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	itemapp "xianyu-go/internal/application/items"
)

// batchGapManagement 是批量发布管理端口的可控桩，每个方法返回字段中预设的结果或错误。
type batchGapManagement struct {
	// startErr 是 StartBatch 返回的错误。
	startErr error
	// listErr 是 ListBatches 返回的错误。
	listErr error
	// getErr 是 GetBatch 返回的错误。
	getErr error
	// getRows 是 GetBatch 成功时返回的明细。
	getRows []itemapp.BatchRow
	// cancelErr 是 CancelBatch 返回的错误。
	cancelErr error
	// deleteErr 是 DeleteBatch 返回的错误。
	deleteErr error
	// retryErr 是 RetryFailedBatch 返回的错误。
	retryErr error
	// cleanupErr 是 CleanupExpiredUploads 返回的错误。
	cleanupErr error
}

// StartBatch 返回预设错误或固定批次标识。
func (m *batchGapManagement) StartBatch(context.Context, int64, string, time.Duration) (string, error) {
	return "b1", m.startErr
}

// ListBatches 返回预设错误或一个批次摘要。
func (m *batchGapManagement) ListBatches(context.Context, int64, int) ([]itemapp.BatchInfo, error) {
	return []itemapp.BatchInfo{{ID: "b1", Status: "completed"}}, m.listErr
}

// GetBatch 返回预设错误或批次详情。
func (m *batchGapManagement) GetBatch(context.Context, int64, string) (itemapp.BatchDetails, error) {
	return itemapp.BatchDetails{Batch: itemapp.BatchInfo{ID: "b1"}, Rows: m.getRows}, m.getErr
}

// CancelBatch 返回预设错误或取消状态。
func (m *batchGapManagement) CancelBatch(context.Context, int64, string) (string, error) {
	return "cancelled", m.cancelErr
}

// DeleteBatch 返回预设错误。
func (m *batchGapManagement) DeleteBatch(context.Context, int64, string) error { return m.deleteErr }

// CleanupExpiredUploads 返回预设清理错误。
func (m *batchGapManagement) CleanupExpiredUploads(context.Context, time.Time, int) error {
	return m.cleanupErr
}

// RetryFailedBatch 返回预设错误或批次标识。
func (m *batchGapManagement) RetryFailedBatch(context.Context, int64, string, time.Duration) (string, error) {
	return "b1", m.retryErr
}

// batchGapPreview 是预检端口的可控桩。
type batchGapPreview struct {
	// ownedErr 是 CookieOwned 的基础设施错误。
	ownedErr error
	// owned 是默认账号归属结果。
	owned bool
	// previewErr 是 Preview 返回的错误。
	previewErr error
	// rows 是 Preview 成功时返回的预检行。
	rows []itemapp.BatchPreviewRow
}

// CookieOwned 返回预设归属结果。
func (p *batchGapPreview) CookieOwned(context.Context, int64, string) (bool, error) {
	return p.owned, p.ownedErr
}

// Preview 返回预设预检行或错误。
func (p *batchGapPreview) Preview(context.Context, itemapp.BatchPreviewInput) ([]itemapp.BatchPreviewRow, error) {
	return p.rows, p.previewErr
}

// batchGapPersistence 是预检持久化端口的可控桩。
type batchGapPersistence struct {
	err error
	// result 是 Persist 成功时返回的结果。
	result itemapp.BatchPreviewPersistenceResult
}

// Persist 返回预设结果或错误。
func (p *batchGapPersistence) Persist(context.Context, itemapp.BatchPreviewPersistenceBatch, []itemapp.BatchPreviewRow) (itemapp.BatchPreviewPersistenceResult, error) {
	return p.result, p.err
}

// TestPublishBatchManagementHandlersMapErrors 覆盖管理类接口把应用层错误映射为 HTTP 状态码的全部分支。
func TestPublishBatchManagementHandlersMapErrors(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	// management 是注入的批量管理桩。
	management := &batchGapManagement{}
	srv.applications.itemBatchManagement = management
	// handler 是注入桩后的完整路由。
	handler := srv.Router()
	cookie := loginHelper(t, handler)

	// cases 描述每个接口在不同应用错误下期望的状态码。
	cases := []struct {
		// name 是子场景名称。
		name string
		// method、path、body 描述请求。
		method, path, body string
		// set 在请求前配置桩错误。
		set func(err error)
		// err 是注入的应用错误。
		err  error
		want int
	}{
		{"start conflict", "POST", "/items/publish-batches", `{"preview_id":"b1"}`, func(e error) { management.startErr = e }, itemapp.ErrBatchConflict, http.StatusConflict},
		{"start invalid state", "POST", "/items/publish-batches", `{"batch_id":"b1"}`, func(e error) { management.startErr = e }, itemapp.ErrBatchInvalidState, http.StatusBadRequest},
		{"start no rows", "POST", "/items/publish-batches", `{"preview_id":"b1"}`, func(e error) { management.startErr = e }, itemapp.ErrBatchNoRows, http.StatusBadRequest},
		{"start other", "POST", "/items/publish-batches", `{"preview_id":"b1"}`, func(e error) { management.startErr = e }, errors.New("boom"), http.StatusInternalServerError},
		{"list error", "GET", "/items/publish-batches", ``, func(e error) { management.listErr = e }, errors.New("boom"), http.StatusInternalServerError},
		{"get other", "GET", "/items/publish-batches/b1", ``, func(e error) { management.getErr = e }, errors.New("boom"), http.StatusInternalServerError},
		{"cancel conflict", "POST", "/items/publish-batches/b1/cancel", ``, func(e error) { management.cancelErr = e }, itemapp.ErrBatchConflict, http.StatusConflict},
		{"cancel other", "POST", "/items/publish-batches/b1/cancel", ``, func(e error) { management.cancelErr = e }, errors.New("boom"), http.StatusInternalServerError},
		{"delete conflict", "DELETE", "/items/publish-batches/b1", ``, func(e error) { management.deleteErr = e }, itemapp.ErrBatchConflict, http.StatusConflict},
		{"delete other", "DELETE", "/items/publish-batches/b1", ``, func(e error) { management.deleteErr = e }, errors.New("boom"), http.StatusInternalServerError},
		{"retry no rows", "POST", "/items/publish-batches/b1/retry-failed", ``, func(e error) { management.retryErr = e }, itemapp.ErrBatchNoRows, http.StatusBadRequest},
		{"retry other", "POST", "/items/publish-batches/b1/retry-failed", ``, func(e error) { management.retryErr = e }, errors.New("boom"), http.StatusInternalServerError},
		{"result other", "GET", "/items/publish-batches/b1/result.csv", ``, func(e error) { management.getErr = e }, errors.New("boom"), http.StatusInternalServerError},
	}
	// tc 表示当前错误映射场景。
	for _, tc := range cases {
		*management = batchGapManagement{}
		tc.set(tc.err)
		// recorder 保存响应。
		recorder := serveChatCoverageRequest(handler, cookie, tc.method, tc.path, tc.body)
		if recorder.Code != tc.want {
			t.Errorf("%s: status=%d want %d body=%s", tc.name, recorder.Code, tc.want, recorder.Body.String())
		}
	}
}

// TestPreviewItemPublishBatchRejectsInvalidInputs 覆盖预检接口在上传、表单和应用层各阶段的失败响应。
func TestPreviewItemPublishBatchRejectsInvalidInputs(t *testing.T) {
	// uploadRoot 是本测试专用的上传根目录，避免写入仓库 data 目录。
	t.Setenv("XIANYU_UPLOAD_DIR", t.TempDir())
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	srv.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// management 的清理故意失败，覆盖仅记录警告的分支。
	management := &batchGapManagement{cleanupErr: errors.New("cleanup failed")}
	srv.applications.itemBatchManagement = management
	// preview 是预检桩。
	preview := &batchGapPreview{owned: true}
	srv.applications.itemBatchPreview = preview
	// persistence 是持久化桩。
	persistence := &batchGapPersistence{}
	srv.applications.itemBatchPreviewPersistence = persistence
	// handler 是注入桩后的完整路由。
	handler := srv.Router()
	cookie := loginHelper(t, handler)

	// send 以 multipart 提交表单字段和可选文件，返回响应状态与正文。
	send := func(fields map[string]string, fileName, fileData, zipName, zipData string) (int, string) {
		// body 保存 multipart 请求体。
		var body bytes.Buffer
		// writer 生成表单。
		writer := multipart.NewWriter(&body)
		// key、value 表示当前表单字段。
		for key, value := range fields {
			_ = writer.WriteField(key, value)
		}
		if fileName != "" {
			// part 是表格文件字段。
			part, _ := writer.CreateFormFile("file", fileName)
			_, _ = part.Write([]byte(fileData))
		}
		if zipName != "" {
			// zipPart 是图片压缩包字段。
			zipPart, _ := writer.CreateFormFile("images_zip", zipName)
			_, _ = zipPart.Write([]byte(zipData))
		}
		_ = writer.Close()
		// request 是携带会话的预检请求。
		request := httptest.NewRequest(http.MethodPost, "/items/publish-batches/preview", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.AddCookie(cookie)
		// recorder 保存响应。
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code, recorder.Body.String()
	}
	// base 是通过账号校验的最小表单。
	base := func() map[string]string { return map[string]string{"default_cookie_id": "acc1"} }
	// csvData 是含表头和一行数据的合法 CSV。
	const csvData = "标题,价格\n商品,1\n"

	preview.ownedErr = errors.New("db down")
	if code, _ := send(base(), "a.csv", csvData, "", ""); code != http.StatusInternalServerError {
		t.Errorf("ownership error status=%d", code)
	}
	preview.ownedErr = nil

	// badFields 列出各类非法表单字段及期望状态。
	badFields := []struct {
		// name 是子场景名称。
		name string
		// mutate 修改基础表单。
		mutate func(map[string]string)
	}{
		{"bad interval", func(f map[string]string) { f["publish_interval_seconds"] = "abc" }},
		{"bad location", func(f map[string]string) { f["location"] = "{" }},
		{"partial category", func(f map[string]string) { f["fallback_category_id"] = "1" }},
	}
	// bad 表示当前非法字段场景。
	for _, bad := range badFields {
		// fields 是应用了非法修改的表单。
		fields := base()
		bad.mutate(fields)
		// body、code 分别保存本步骤的返回值：body为本次请求返回的响应正文，用于诊断失败；code为本次请求返回的 HTTP 状态码
		if code, body := send(fields, "a.csv", csvData, "", ""); code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s", bad.name, code, body)
		}
	}

	if code, _ := send(base(), "a.csv", strings.Repeat("a", 21<<20), "", ""); code != http.StatusBadRequest {
		t.Errorf("oversize sheet status=%d", code)
	}
	if code, _ := send(base(), "a.csv", csvData, "i.zip", "not a zip"); code != http.StatusBadRequest {
		t.Errorf("bad zip status=%d", code)
	}
	// 文件名被净化为空时回落到默认名称，随后因 zip 非法返回 400。
	if code, _ := send(base(), "/", csvData, "/", "not a zip"); code != http.StatusBadRequest {
		t.Errorf("blank names status=%d", code)
	}
	// 文件名为 .. 时无法在目录内创建同名文件，返回保存失败。
	if code, _ := send(base(), "..", csvData, "", ""); code != http.StatusInternalServerError {
		t.Errorf("dotdot name status=%d", code)
	}
	if code, _ := send(base(), "a.csv", "标题\n", "", ""); code != http.StatusBadRequest {
		t.Errorf("header-only sheet status=%d", code)
	}

	preview.previewErr = itemapp.ErrBatchPreviewNoRows
	if code, _ := send(base(), "a.csv", csvData, "", ""); code != http.StatusBadRequest {
		t.Errorf("preview no rows status=%d", code)
	}
	preview.previewErr = errors.New("preview failed")
	if code, _ := send(base(), "a.csv", csvData, "", ""); code != http.StatusBadRequest {
		t.Errorf("preview error status=%d", code)
	}
	preview.previewErr = nil

	persistence.err = itemapp.ErrBatchPreviewNoRows
	if code, _ := send(base(), "a.csv", csvData, "", ""); code != http.StatusBadRequest {
		t.Errorf("persist no rows status=%d", code)
	}
	persistence.err = errors.New("persist failed")
	if code, _ := send(base(), "a.csv", csvData, "", ""); code != http.StatusInternalServerError {
		t.Errorf("persist error status=%d", code)
	}
	persistence.err = nil

	// 成功路径同时覆盖预检行里付款发货与评价赠品动作的映射。
	preview.rows = []itemapp.BatchPreviewRow{{RowNo: 2, Title: "商品", Automation: itemapp.BatchPreviewAutomation{
		PaidDelivery: itemapp.BatchPreviewCardAutomation{Enabled: true, Actions: []itemapp.BatchPreviewCardAction{{CardID: 1, DeliveryCount: 1}}},
		ReviewGift:   itemapp.BatchPreviewCardAutomation{Enabled: true, Actions: []itemapp.BatchPreviewCardAction{{CardID: 2, DeliveryCount: 1}}},
	}}}
	persistence.result = itemapp.BatchPreviewPersistenceResult{Success: true, PreviewID: "p1", Total: 1, Valid: 1, Rows: preview.rows}
	// body、code 分别保存本步骤的返回值：body为本次请求返回的响应正文，用于诊断失败；code为本次请求返回的 HTTP 状态码
	if code, body := send(base(), "a.csv", csvData, "", ""); code != http.StatusOK || !strings.Contains(body, "p1") {
		t.Errorf("success status=%d body=%s", code, body)
	}

	// 默认账号不属于当前用户。
	preview.owned = false
	if code, _ := send(base(), "a.csv", csvData, "", ""); code != http.StatusForbidden {
		t.Errorf("foreign cookie status=%d", code)
	}
}

// TestPublishBatchSmallHelpers 覆盖失败类型、最终状态以及账号归属和 Cookie 读取的辅助分支。
func TestPublishBatchSmallHelpers(t *testing.T) {
	if got := finalPublishBatchStatus(nil); got != "failed" {
		t.Errorf("nil batch => %q", got)
	}
	if got := finalPublishBatchStatus(&itemapp.BatchInfo{FailedCount: 1}); got != "failed" {
		t.Errorf("all failed => %q", got)
	}
	if got := finalPublishBatchStatus(&itemapp.BatchInfo{FailedCount: 1, SuccessCount: 1}); got != "partially_failed" {
		t.Errorf("partial => %q", got)
	}
	// srv 是零值 Server，没有任何应用端口。
	srv := &Server{}
	if srv.cookieOwnedByUser(context.Background(), 1, "c") {
		t.Error("nil summary port must not own")
	}
	if _, err := srv.cookieValueForUser(context.Background(), 1, "c"); err == nil {
		t.Error("nil credential port should fail")
	}
}
