package server

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	automationapp "xianyu-go/internal/application/automation"
	cardsapp "xianyu-go/internal/application/cards"
)

// cardGapPort 包装卡券端口，让追加库存返回预设错误。
type cardGapPort struct {
	// CardsPort 提供其余卡券能力的真实实现。
	CardsPort
	// appendErr 是 AppendData 的错误。
	appendErr error
}

// AppendData 返回预设错误。
func (p cardGapPort) AppendData(context.Context, int64, int64, string) (int, error) {
	return 0, p.appendErr
}

// issuesGapPort 是自动化异常端口桩。
type issuesGapPort struct {
	// listErr 是 ListIssues 的错误。
	listErr error
	// runErr、deferredErr 是两类处理方法的错误。
	runErr, deferredErr error
}

// ListIssues 返回预设错误或一条延期异常。
func (p issuesGapPort) ListIssues(context.Context, int64) ([]automationapp.RunIssue, []automationapp.DeferredIssue, error) {
	return nil, []automationapp.DeferredIssue{{ID: 1, CookieID: "acc1"}}, p.listErr
}

// ResolveRunIssue 返回预设错误。
func (p issuesGapPort) ResolveRunIssue(context.Context, int64, int64, string) error { return p.runErr }

// ResolveDeferredIssue 返回预设错误。
func (p issuesGapPort) ResolveDeferredIssue(context.Context, int64, int64, string) error {
	return p.deferredErr
}

// TestCardBatchAndAppendGapBranches 覆盖卡密批量创建的逐行校验、上传失败与追加库存错误映射。
func TestCardBatchAndAppendGapBranches(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	handler := srv.Router()
	cookie := loginHelper(t, handler)

	// upload 以 multipart 上传卡密表格，fileName 为空时不带文件。
	upload := func(fileName, data string) *httptest.ResponseRecorder {
		// body 保存 multipart 请求体。
		var body bytes.Buffer
		// writer 生成表单。
		writer := multipart.NewWriter(&body)
		if fileName != "" {
			// part 是文件字段写入器。
			part, _ := writer.CreateFormFile("file", fileName)
			_, _ = part.Write([]byte(data))
		}
		_ = writer.Close()
		// request 是批量创建请求。
		request := httptest.NewRequest(http.MethodPost, "/cards/batch", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.AddCookie(cookie)
		// recorder 保存响应。
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	if rec := upload("", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("missing file=%d", rec.Code)
	}
	if rec := upload("cards.csv", strings.Repeat("a", 5<<20+1)); rec.Code != http.StatusBadRequest {
		t.Errorf("oversize=%d", rec.Code)
	}
	if rec := upload("cards.xls", "x"); rec.Code != http.StatusBadRequest {
		t.Errorf("xls=%d", rec.Code)
	}
	if rec := upload("/", "name,type,content\n"); rec.Code != http.StatusBadRequest {
		t.Errorf("header only with default name=%d", rec.Code)
	}
	// rows 覆盖缺名称、坏类型、缺内容、图片类型成功和延时越界。
	rows := "name,type,content,delay_seconds\n,text,x,0\nbad,weird,x,0\nempty,text,,0\nimg,image,https://example.test/a.png,0\nslow,text,x,9999\n"
	rec := upload("cards.csv", rows)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"failed":4`) {
		t.Errorf("rows status=%d body=%s", rec.Code, rec.Body.String())
	}
	// manyRows 超过单次最多卡密组数。
	var many strings.Builder
	many.WriteString("name,type,content\n")
	// i 循环序号，用于生成不同的测试数据
	for i := 0; i <= maxCardBatchRows; i++ {
		many.WriteString("n,text,c\n")
	}
	if rec = upload("cards.csv", many.String()); rec.Code != http.StatusBadRequest {
		t.Errorf("too many rows=%d", rec.Code)
	}

	// 追加库存错误映射。
	real := srv.applications.cards
	if rec = serveChatCoverageRequest(handler, cookie, "POST", "/cards/x/append-data", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id=%d", rec.Code)
	}
	if rec = serveChatCoverageRequest(handler, cookie, "POST", "/cards/1/append-data", `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body=%d", rec.Code)
	}
	if rec = serveChatCoverageRequest(handler, cookie, "POST", "/cards/1/append-data", `{"content":" "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty content=%d", rec.Code)
	}
	// appendCases 描述追加库存错误与期望状态。
	appendCases := []struct {
		err  error
		want int
	}{{cardsapp.ErrNotFound, 404}, {cardsapp.ErrForbidden, 403}, {cardsapp.ErrNotDataType, 400}, {errors.New("db"), 500}}
	// tc 表示当前追加场景。
	for _, tc := range appendCases {
		srv.applications.cards = cardGapPort{CardsPort: real, appendErr: tc.err}
		if rec = serveChatCoverageRequest(handler, cookie, "POST", "/cards/1/append-data", `{"content":"a"}`); rec.Code != tc.want {
			t.Errorf("append %v => %d want %d", tc.err, rec.Code, tc.want)
		}
	}
}

// TestAutomationIssueHandlersGapBranches 覆盖自动化异常列表与处理接口的错误映射。
func TestAutomationIssueHandlersGapBranches(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	// port 是可变异常端口桩。
	port := &issuesGapPort{}
	srv.applications.automationIssues = port
	handler := srv.Router()
	cookie := loginHelper(t, handler)

	if rec := serveChatCoverageRequest(handler, cookie, "GET", "/automation-issues", ""); rec.Code != http.StatusOK {
		t.Errorf("list=%d", rec.Code)
	}
	port.listErr = errors.New("db")
	if rec := serveChatCoverageRequest(handler, cookie, "GET", "/automation-issues", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure=%d", rec.Code)
	}
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/automation-runs/x/resolve", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad run id=%d", rec.Code)
	}
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/automation-runs/1/resolve", `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad run body=%d", rec.Code)
	}
	port.runErr = errors.New("invalid")
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/automation-runs/1/resolve", `{"resolution":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("run invalid=%d", rec.Code)
	}
	port.runErr = automationapp.ErrNotFound
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/automation-runs/1/resolve", `{"resolution":"x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("run missing=%d", rec.Code)
	}
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/automation-pending-tasks/1/resolve", `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("task bad body=%d", rec.Code)
	}
	port.deferredErr = automationapp.ErrInvalidDeferredResolution
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/automation-pending-tasks/1/resolve", `{"resolution":"x"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("task invalid=%d", rec.Code)
	}
	port.deferredErr = errors.New("gone")
	if rec := serveChatCoverageRequest(handler, cookie, "POST", "/automation-pending-tasks/1/resolve", `{"resolution":"retry"}`); rec.Code != http.StatusNotFound {
		t.Errorf("task missing=%d", rec.Code)
	}
}
