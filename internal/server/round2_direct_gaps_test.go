package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accountapp "xianyu-go/internal/application/account"
	cardsapp "xianyu-go/internal/application/cards"
	itemapp "xianyu-go/internal/application/items"
)

// bareRequest 构造不经过认证中间件的请求，因此上下文里没有管理会话，用于覆盖会话缺失的防御分支。
func bareRequest(method, body string) *http.Request {
	return httptest.NewRequest(method, "/x", strings.NewReader(body))
}

// ownerGapSummary 包装账号摘要端口，使归属查询返回另一个用户的账号。
type ownerGapSummary struct {
	// AccountSummaryPort 提供其余摘要能力的真实实现。
	AccountSummaryPort
}

// GetOwnedSummary 返回属于其他用户的账号摘要。
func (ownerGapSummary) GetOwnedSummary(context.Context, int64, string) (accountapp.AccountSummary, error) {
	return accountapp.AccountSummary{ID: "acc1", UserID: 424242}, nil
}

// passwordGapPort 是密码登录端口桩，所有操作返回同一个错误。
type passwordGapPort struct {
	err error
}

// Start 返回预设错误。
func (p passwordGapPort) Start(context.Context, accountapp.PasswordLoginStartInput) error {
	return p.err
}

// Check 返回预设错误。
func (p passwordGapPort) Check(context.Context, accountapp.PasswordLoginSessionInput) error {
	return p.err
}

// Cancel 返回预设错误。
func (p passwordGapPort) Cancel(context.Context, accountapp.PasswordLoginSessionInput) error {
	return p.err
}

// localPublishGapPort 是批量本地收口端口桩。
type localPublishGapPort struct {
	err error
}

// Complete 不会被调用。
func (localPublishGapPort) Complete(context.Context, int64, itemapp.BatchRow, string, *itemapp.BatchPublishResult) error {
	return nil
}

// EnsureAutomationRules 返回预设错误。
func (p localPublishGapPort) EnsureAutomationRules(context.Context, int64, itemapp.BatchRow, *itemapp.BatchPublishResult) error {
	return p.err
}

// testerGapPort 是卡券 API 测试端口桩，仅用于让 handler 越过服务可用性检查。
type testerGapPort struct{}

// Test 不会被调用，会话缺失时 handler 已提前返回。
func (testerGapPort) Test(context.Context, cardsapp.APIRequestTestInput) (cardsapp.APIRequestTestResult, error) {
	return cardsapp.APIRequestTestResult{}, nil
}

// TestSessionlessGuardsReturnUnauthorized 通过直接调用 handler 覆盖会话缺失时的防御性响应。
func TestSessionlessGuardsReturnUnauthorized(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	srv.applications.apiRequestTester = testerGapPort{}
	// cases 描述被直接调用的 handler、请求体与期望状态码。
	cases := []struct {
		// name 是子场景名称。
		name string
		// call 直接执行 handler。
		call func(http.ResponseWriter, *http.Request)
		// body 是请求体。
		body string
		want int
	}{
		{"qr generate", srv.generateQRLogin, ``, 401},
		{"qr check without id", srv.checkQRLoginStatus, ``, 400},
		{"qr status without id", srv.checkQRLoginStatusAndPersist, ``, 400},
		{"qr complete without id", srv.completeQRVerification, ``, 400},
		{"settings batch", srv.setSettings, `{"theme":"x"}`, 500},
		{"settings all", srv.allSettings, ``, 500},
		{"ai models", srv.listAIModels, `{}`, 500},
		{"ai test", srv.testAIConnection, `{}`, 500},
		{"ai reply get", srv.getAIReply, ``, 401},
		{"ai reply set", srv.setAIReply, `{}`, 401},
		{"default reply get", srv.getDefaultReply, ``, 401},
		{"default reply set", srv.setDefaultReply, `{}`, 401},
		{"default reply delete", srv.deleteDefaultReply, ``, 401},
		{"default reply clear", srv.clearDefaultReplyRecords, ``, 401},
		{"default reply list", srv.listDefaultReplies, ``, 401},
		{"default reply map", srv.listDefaultRepliesMap, ``, 401},
		{"card api test", srv.testCardAPI, `{"card_id":1}`, 401},
	}
	// tc 表示当前直接调用场景。
	for _, tc := range cases {
		// recorder 保存响应。
		recorder := httptest.NewRecorder()
		tc.call(recorder, bareRequest("POST", tc.body))
		if recorder.Code != tc.want {
			t.Errorf("%s => %d want %d body=%s", tc.name, recorder.Code, tc.want, recorder.Body.String())
		}
	}
	// recorder 保存 requireCookieOwner 与 requireQRSessionOwner 的响应。
	recorder := httptest.NewRecorder()
	// ok 表示账号归属校验是否通过；会话缺失时必须为 false
	if _, ok := srv.requireCookieOwner(recorder, bareRequest("GET", ""), "acc1"); ok || recorder.Code != http.StatusUnauthorized {
		t.Errorf("cookie owner without session => %d", recorder.Code)
	}
	// qrRecorder 保存二维码会话归属校验的响应。
	qrRecorder := httptest.NewRecorder()
	if srv.requireQRSessionOwner(qrRecorder, bareRequest("GET", ""), "s1") || qrRecorder.Code != http.StatusUnauthorized {
		t.Errorf("qr owner without session => %d", qrRecorder.Code)
	}
	if srv.ownsAccount(bareRequest("GET", ""), "") {
		t.Error("empty account id must not be owned")
	}
}

// TestOwnershipAndAccountTaskGuards 覆盖账号归属错配、摘要服务缺失及账号任务的权限与格式错误。
func TestOwnershipAndAccountTaskGuards(t *testing.T) {
	// srv、cleanup 是启用聊天应用的测试服务及资源释放函数。
	srv, _, cleanup := newTestServerWithChat(t)
	defer cleanup()
	handler := srv.Router()
	cookie := loginHelper(t, handler)
	// real 是真实账号摘要端口。
	real := srv.applications.accountSummaries

	srv.applications.accountSummaries = ownerGapSummary{AccountSummaryPort: real}
	// recorder 是调用 "PUT" "/cookies/acc1/status" 得到的响应记录，其状态码用于断言该场景的错误映射
	if recorder := serveChatCoverageRequest(handler, cookie, "PUT", "/cookies/acc1/status", `{"enabled":true}`); recorder.Code != http.StatusForbidden {
		t.Errorf("foreign owner => %d", recorder.Code)
	}
	srv.applications.accountSummaries = nil
	// recorder 是调用 "GET" "/cookies/acc1/auto-confirm" 得到的响应记录，其状态码用于断言该场景的错误映射
	if recorder := serveChatCoverageRequest(handler, cookie, "GET", "/cookies/acc1/auto-confirm", ""); recorder.Code != http.StatusNotFound {
		t.Errorf("missing summary service => %d", recorder.Code)
	}
	srv.applications.accountSummaries = real

	// recorder 是调用 "PUT" "/api/account-tasks/ghost" 得到的响应记录，其状态码用于断言该场景的错误映射
	if recorder := serveChatCoverageRequest(handler, cookie, "PUT", "/api/account-tasks/ghost", `{}`); recorder.Code != http.StatusForbidden {
		t.Errorf("task update foreign => %d", recorder.Code)
	}
	// recorder 是调用 "POST" "/api/account-tasks/ghost/run" 得到的响应记录，其状态码用于断言该场景的错误映射
	if recorder := serveChatCoverageRequest(handler, cookie, "POST", "/api/account-tasks/ghost/run", `{}`); recorder.Code != http.StatusForbidden {
		t.Errorf("task run foreign => %d", recorder.Code)
	}
	// recorder 是调用 "PUT" "/api/account-tasks/acc1" 得到的响应记录，其状态码用于断言该场景的错误映射
	if recorder := serveChatCoverageRequest(handler, cookie, "PUT", "/api/account-tasks/acc1", `{`); recorder.Code != http.StatusBadRequest {
		t.Errorf("task update bad body => %d", recorder.Code)
	}
	// recorder 是调用 "GET" "/api/v1/admin/tasks?limit=0" 得到的响应记录，其状态码用于断言该场景的错误映射
	if recorder := serveChatCoverageRequest(handler, cookie, "GET", "/api/v1/admin/tasks?limit=0", ""); recorder.Code != http.StatusBadRequest {
		t.Errorf("tasks bad limit => %d", recorder.Code)
	}
	// 后台任务数量超过 limit 时只返回前 limit 条。
	srv.startBackgroundTaskContext("a", context.Background(), func() {})
	srv.startBackgroundTaskContext("b", context.Background(), func() {})
	srv.WaitForBackground()
	// recorder 是调用 "GET" "/api/v1/admin/tasks?limit=1" 得到的响应记录，其状态码用于断言该场景的错误映射
	if recorder := serveChatCoverageRequest(handler, cookie, "GET", "/api/v1/admin/tasks?limit=1", ""); recorder.Code != http.StatusOK {
		t.Errorf("tasks limited => %d", recorder.Code)
	}
	// 空任务在有日志器时记录警告而不执行。
	srv.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	srv.startBackgroundTaskResult("nil-task", context.Background(), nil)
	srv.WaitForBackground()
}

// TestPasswordLoginHelpersAndLocalPublishGaps 覆盖密码登录关闭策略的异常分支、JSON 解码边界与本地收口服务缺失。
func TestPasswordLoginHelpersAndLocalPublishGaps(t *testing.T) {
	// serve 以指定端口桩和方法调用密码登录处理器并返回状态码。
	serve := func(port PasswordLoginPort, method string) int {
		// srv 是只携带密码登录端口的零值 Server。
		srv := &Server{applications: &ApplicationPorts{passwordLogin: port}}
		// recorder 保存响应。
		recorder := httptest.NewRecorder()
		srv.passwordLoginDisabled(recorder, httptest.NewRequest(method, "/x", nil))
		return recorder.Code
	}
	// code 是密码登录处理器在该端口桩与 HTTP 方法组合下返回的状态码
	if code := serve(nil, "POST"); code != http.StatusInternalServerError {
		t.Errorf("nil service => %d", code)
	}
	// code 是密码登录处理器在该端口桩与 HTTP 方法组合下返回的状态码
	if code := serve(passwordGapPort{}, "PATCH"); code != http.StatusMethodNotAllowed {
		t.Errorf("unsupported method => %d", code)
	}
	// code 是密码登录处理器在该端口桩与 HTTP 方法组合下返回的状态码
	if code := serve(passwordGapPort{err: errors.New("db")}, "POST"); code != http.StatusInternalServerError {
		t.Errorf("operation failure => %d", code)
	}
	// code 是密码登录处理器在该端口桩与 HTTP 方法组合下返回的状态码
	if code := serve(passwordGapPort{err: accountapp.ErrPasswordLoginDisabled}, "GET"); code != http.StatusNotImplemented {
		t.Errorf("disabled => %d", code)
	}

	// decoded 接收 decodeJSON 的解码结果。
	var decoded map[string]any
	// err 是decodeJSON 对该畸形请求体返回的解码错误，必须非空
	if err := decodeJSON(httptest.NewRequest("POST", "/x", failingImportReader{}), &decoded); err == nil {
		t.Error("read failure should propagate")
	}
	// err 是decodeJSON 对该畸形请求体返回的解码错误，必须非空
	if err := decodeJSON(bareRequest("POST", `{} {`), &decoded); err == nil {
		t.Error("garbage after first value should fail")
	}
	// err 是decodeJSON 对该畸形请求体返回的解码错误，必须非空
	if err := decodeJSON(bareRequest("POST", `{} {}`), &decoded); err == nil {
		t.Error("second value should fail")
	}

	// srv 没有装配本地收口端口。
	srv := &Server{}
	if _, err := srv.newBatchLocalPublishService(); !errors.Is(err, itemapp.ErrBatchLocalPublishUnavailable) {
		t.Errorf("unwired service err = %v", err)
	}
	if err := srv.createPublishAutomationRules(context.Background(), 1, itemapp.BatchRow{}, nil); err == nil {
		t.Error("unwired service should fail")
	}
	srv.applications = &ApplicationPorts{itemBatchLocalPublish: localPublishGapPort{err: errors.New("rules failed")}}
	if err := srv.createPublishAutomationRules(context.Background(), 1, itemapp.BatchRow{}, nil); err == nil {
		t.Error("port error should propagate")
	}
}

// TestItemHandlerRound2Gaps 覆盖商品接口的账号归属失败、凭证服务缺失和规格图片校验。
func TestItemHandlerRound2Gaps(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	handler := srv.Router()
	cookie := loginHelper(t, handler)
	// ghostRoutes 是账号不存在时应提前返回的商品接口。
	ghostRoutes := []struct{ method, path string }{
		{"GET", "/items/ghost/i1"}, {"PUT", "/items/ghost/i1"}, {"DELETE", "/items/ghost/i1"},
	}
	// route 表示当前账号不存在场景。
	for _, route := range ghostRoutes {
		// recorder 是调用 route.method route.path 得到的响应记录，其状态码用于断言该场景的错误映射
		if recorder := serveChatCoverageRequest(handler, cookie, route.method, route.path, `{}`); recorder.Code < 400 {
			t.Errorf("%s %s => %d", route.method, route.path, recorder.Code)
		}
	}

	// png 是最小 PNG 魔数字节。
	png := []byte("\x89PNG\r\n\x1a\n")
	// okImages 是合法主图。
	okImages := []publishImagePart{{field: "images", contentType: "image/png", data: png}}
	// baseFields 是通过基础校验的字段。
	baseFields := []publishFormField{{"cookie_id", "acc1"}, {"title", "t"}, {"price", "10"}, {"quantity", "1"}}
	// badSpec 是 spec_images 中的非图片部件。
	badSpec := append(append([]publishImagePart{}, okImages...), publishImagePart{field: "spec_images", contentType: "text/plain", data: png})
	// body 是发布接口返回的响应正文，仅用于失败时输出诊断；code 是发布接口返回的 HTTP 状态码
	if code, body := postPublishForm(handler, cookie, baseFields, badSpec); code != http.StatusBadRequest {
		t.Errorf("non-image spec image => %d %s", code, body)
	}
	// bigSpec 是超过 10 MiB 的规格图片。
	bigSpec := append(append([]publishImagePart{}, okImages...), publishImagePart{field: "spec_images", contentType: "image/png", data: append(append([]byte{}, png...), make([]byte, 10<<20)...)})
	// body 是发布接口返回的响应正文，仅用于失败时输出诊断；code 是发布接口返回的 HTTP 状态码
	if code, body := postPublishForm(handler, cookie, baseFields, bigSpec); code != http.StatusBadRequest {
		t.Errorf("oversize spec image => %d %s", code, body)
	}

	srv.applications.platformCredentials = nil
	if code, _ := postPublishForm(handler, cookie, baseFields, okImages); code != http.StatusInternalServerError {
		t.Errorf("missing credential service => %d", code)
	}
}

// TestImportAndSheetRound2Gaps 覆盖 xlsx 分流、工作表读取失败和空白行/空表头列的跳过。
func TestImportAndSheetRound2Gaps(t *testing.T) {
	// sheetName 是 xlsx 内工作表路径。
	const sheetName = "xl/worksheets/sheet1.xml"
	// okZip 是合法的订单 xlsx。
	okZip := buildImportZip(t, []string{sheetName}, map[string]string{sheetName: importSheetXML([][]string{{"订单号"}, {"o1"}})})
	// err 是xlsx 订单解析的错误，合法文件应为 nil；rows 从 xlsx 解析出的订单行
	if rows, err := parseImportedOrderBytes(okZip, "orders.xlsx"); err != nil || len(rows) != 1 {
		t.Errorf("xlsx dispatch rows=%v err=%v", rows, err)
	}
	// brokenSheet 的工作表条目使用未知压缩算法，打开必然失败。
	var buf bytes.Buffer
	// writer 写入原始条目。
	writer := zip.NewWriter(&buf)
	// entry 是未知压缩方法的工作表条目。
	entry, err := writer.CreateRaw(&zip.FileHeader{Name: sheetName, Method: 99})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("zz"))
	_ = writer.Close()
	if _, err = parseXLSXOrders(buf.Bytes()); err == nil {
		t.Error("unreadable order sheet should fail")
	}
	if _, err = parseXLSXPublishSheetWithLimit(buf.Bytes(), 0); err == nil {
		t.Error("unreadable publish sheet should fail")
	}
	// 全空数据行与空表头列在铺货表格中被跳过。
	if rows, parseErr := parsePublishSheetBytes([]byte("标题,,价格\na,x,1\n,,\n"), "a.csv"); parseErr != nil || len(rows) != 1 {
		t.Errorf("blank row skipped rows=%v err=%v", rows, parseErr)
	}
}

// TestPublishBatchFailureAndCredentialHelpers 覆盖后置处理失败分类、凭证读取错误映射和数字文本回退。
func TestPublishBatchFailureAndCredentialHelpers(t *testing.T) {
	// kind 是失败分类；两种后置处理错误类型都应归为 post_publish。
	for _, cause := range []error{&postPublishError{err: errors.New("rules failed")}, &itemapp.PostPublishError{Err: errors.New("rules failed")}} {
		// kind 是publishBatchFailure 给出的失败分类，后置处理错误必须是 post_publish
		if _, kind := publishBatchFailure(cause, "running"); kind != "post_publish" {
			t.Errorf("%T classified as %q", cause, kind)
		}
	}
	// credErrs 描述凭证端口错误与 cookieValueForUser 的映射结果是否应为统一文案。
	credErrs := []struct {
		err error
		// value 是凭证端口返回的 Cookie 占位值。
		value string
		// unified 表示期望返回统一的“账号不存在或 Cookie 为空”。
		unified bool
	}{
		{accountapp.ErrCredentialNotFound, "", true}, {accountapp.ErrForbidden, "", true},
		{accountapp.ErrCredentialEmpty, "", true}, {nil, "  ", true}, {errors.New("db"), "", false},
	}
	// tc 表示当前凭证读取场景。
	for _, tc := range credErrs {
		// srv 只携带凭证端口桩。
		srv := &Server{applications: &ApplicationPorts{platformCredentials: itemGapCredentials{value: tc.value, err: tc.err}}}
		// err 是 cookieValueForUser 的返回错误。
		_, err := srv.cookieValueForUser(context.Background(), 1, "acc1")
		if err == nil || (err.Error() == "账号不存在或 Cookie 为空") != tc.unified {
			t.Errorf("%v/%q => %v", tc.err, tc.value, err)
		}
	}
	// got 是被测函数返回的实际值，用于与期望比较
	if got := atoiPublishDefault("not-a-number", 7); got != 7 {
		t.Errorf("fallback = %d", got)
	}
}
