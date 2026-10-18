package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cardsapp "xianyu-go/internal/application/cards"
)

// bareRequest 构造不经过认证中间件的请求，因此上下文里没有管理会话，用于覆盖会话缺失的防御分支。
func bareRequest(method, body string) *http.Request {
	return httptest.NewRequest(method, "/x", strings.NewReader(body))
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
