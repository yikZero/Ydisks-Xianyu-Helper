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

	orderapp "xianyu-go/internal/application/orders"
)

// refreshGapPort 是订单刷新任务端口桩。
type refreshGapPort struct {
	// startErr 是 CreateAndStart 的错误。
	startErr error
	// job、jobErr 是 GetJob 的返回。
	job    *orderapp.RefreshJob
	jobErr error
	// cancel、cancelErr 是 CancelForUser 的返回。
	cancel    orderapp.RefreshJobCancelResult
	cancelErr error
}

// CreateAndStart 返回预设错误或固定任务。
func (p *refreshGapPort) CreateAndStart(context.Context, int64, string, string) (orderapp.RefreshJobStartResult, error) {
	return orderapp.RefreshJobStartResult{Job: &orderapp.RefreshJob{ID: "j1"}}, p.startErr
}

// GetJob 返回预设任务。
func (p *refreshGapPort) GetJob(context.Context, int64, string) (*orderapp.RefreshJob, error) {
	return p.job, p.jobErr
}

// CancelForUser 返回预设取消结果。
func (p *refreshGapPort) CancelForUser(context.Context, int64, string) (orderapp.RefreshJobCancelResult, error) {
	return p.cancel, p.cancelErr
}

// TestOrderRefreshJobHandlerGapBranches 覆盖订单刷新任务接口的错误映射与结果解析。
func TestOrderRefreshJobHandlerGapBranches(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	srv.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// port 是可变任务端口桩。
	port := &refreshGapPort{}
	srv.applications.orderRefreshJobs = port
	handler := srv.Router()
	cookie := loginHelper(t, handler)

	port.startErr = orderapp.ErrForbidden
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/api/v1/orders/refresh", `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("start forbidden=%d", rec.Code)
	}
	port.startErr = errors.New("db")
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/api/v1/orders/refresh", `{}`); rec.Code != http.StatusInternalServerError {
		t.Errorf("start failure=%d", rec.Code)
	}

	// getCases 描述任务读取场景。
	getCases := []struct {
		job  *orderapp.RefreshJob
		err  error
		want int
	}{
		{nil, orderapp.ErrRefreshJobNotFound, 404},
		{nil, errors.New("db"), 500},
		{&orderapp.RefreshJob{ID: "j1", Status: "succeeded", ResultJSON: "{"}, nil, 500},
		{&orderapp.RefreshJob{ID: "j1", Status: "succeeded", ResultJSON: `{"total":1}`}, nil, 200},
	}
	// tc 表示当前读取场景。
	for _, tc := range getCases {
		port.job, port.jobErr = tc.job, tc.err
		if rec := serveChatCoverageRequest(handler, cookie, "GET", "/api/v1/orders/refresh/j1", ""); rec.Code != tc.want {
			t.Errorf("get %v/%v => %d want %d", tc.job, tc.err, rec.Code, tc.want)
		}
	}

	// cancelCases 描述任务取消场景。
	cancelCases := []struct {
		result orderapp.RefreshJobCancelResult
		err    error
		want   int
	}{
		{orderapp.RefreshJobCancelResult{}, orderapp.ErrRefreshJobNotFound, 404},
		{orderapp.RefreshJobCancelResult{}, errors.New("db"), 500},
		{orderapp.RefreshJobCancelResult{Job: &orderapp.RefreshJob{Status: "succeeded"}}, nil, 409},
		{orderapp.RefreshJobCancelResult{Cancelled: true}, nil, 200},
	}
	// tc 表示当前取消场景。
	for _, tc := range cancelCases {
		port.cancel, port.cancelErr = tc.result, tc.err
		if rec := serveChatCoverageRequest(handler, cookie, "DELETE", "/api/v1/orders/refresh/j1", ""); rec.Code != tc.want {
			t.Errorf("cancel %v/%v => %d want %d", tc.result, tc.err, rec.Code, tc.want)
		}
	}
}

// TestParseOrderRefreshRequestGapBranches 覆盖订单刷新请求的媒体类型、表单与超限解析分支。
func TestParseOrderRefreshRequestGapBranches(t *testing.T) {
	// parse 以给定类型和正文调用解析器。
	parse := func(contentType, body string) (orderRefreshRequest, error) {
		// req 是待解析请求。
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		return parseOrderRefreshRequest(httptest.NewRecorder(), req)
	}
	if _, err := parse("", "data"); err == nil {
		t.Error("untyped body should fail")
	}
	if _, err := parse("", ""); err != nil {
		t.Errorf("empty request: %v", err)
	}
	if _, err := parse("not a media type;;;", "x"); err == nil {
		t.Error("bad media type should fail")
	}
	if _, err := parse("text/plain", "x"); err == nil {
		t.Error("unsupported media type should fail")
	}
	// err、got 分别保存本步骤的返回值：err为本步骤返回的错误，用于断言失败或成功路径；got为被测函数的实际返回值，用于与期望值比较
	if got, err := parse("application/x-www-form-urlencoded", "cookie_id=+a+&status=2"); err != nil || got.CookieID != "a" || got.Status != "2" {
		t.Errorf("urlencoded = %+v %v", got, err)
	}
	if _, err := parse("application/x-www-form-urlencoded", "a=%zz"); err == nil {
		t.Error("broken urlencoded should fail")
	}
	if _, err := parse("multipart/form-data", "garbage"); err == nil {
		t.Error("broken multipart should fail")
	}
	// bigBody 是超过 1 MiB 的 multipart 表单。
	var bigBody bytes.Buffer
	// writer 生成 multipart。
	writer := multipart.NewWriter(&bigBody)
	_ = writer.WriteField("cookie_id", strings.Repeat("a", maxOrderRefreshRequestBytes+10))
	_ = writer.Close()
	if _, err := parse(writer.FormDataContentType(), bigBody.String()); err == nil {
		t.Error("oversize multipart should fail")
	}
}
