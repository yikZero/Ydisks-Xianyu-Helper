package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	cardsapp "xianyu-go/internal/application/cards"
	"xianyu-go/internal/db"
)

// editorAPITester 捕获临时测试实际收到的配置，不访问外部服务。
type editorAPITester struct {
	// config 保存最近一次通过权限与配置校验的执行配置。
	config string
	// calls 记录调用次数，证明越权与错误草稿不会发送到上游。
	calls int
}

// Test 记录 input 并返回固定诊断；ctx 与真实测试请求共用取消机制。
func (tester *editorAPITester) Test(ctx context.Context, input cardsapp.APIRequestTestInput) (cardsapp.APIRequestTestResult, error) {
	tester.config = input.Config
	tester.calls++
	return cardsapp.APIRequestTestResult{Status: "success", StatusCode: 200, ResponseContentType: "application/json", ResponseFields: []string{"data"}}, ctx.Err()
}

// TestCardAPIEditorContract 验证完整回显仅属于当前用户，编辑测试合并旧模板且不写回数据库。
func TestCardAPIEditorContract(t *testing.T) {
	srv, store, cleanup := newTestServer(t)
	defer cleanup()
	// ctx 控制夹具操作。
	ctx := context.Background()
	// owner、err 是当前管理员用户。
	owner, err := store.Users.GetAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// saved 是包含测试凭证和幂等键的已保存配置；不是线上密钥。
	saved := `{"url":"https://example.com/claim","method":"POST","timeout_seconds":10,"headers":{"Authorization":"Bearer editor-fixture-secret","Idempotency-Key":"{idempotency_key}"},"params":{"order_id":"{order_id}"},"body":{"trigger_type":"{trigger_type}"},"content_type":"application/json","response_path":"data.content","retry_enabled":true}`
	// cardID、err 是归属当前用户的 API 卡密。
	cardID, err := store.Cards.Create(ctx, &db.CardFull{Name: "编辑回显测试", Type: "api", APIConfig: saved, Enabled: true, UserID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	// tester 截获实际测试配置，避免向真实服务发货。
	tester := &editorAPITester{}
	srv.applications.apiRequestTester = tester
	handler := srv.Router()
	// cookie 是当前管理员的登录会话。
	cookie := loginHelper(t, handler)
	// perform 构造已登录请求并检查真实响应契约。
	perform := func(method, path, body string, status int) *httptest.ResponseRecorder {
		// request 是当前编辑或测试请求。
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.AddCookie(cookie)
		// response 捕获接口的状态与正文。
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatalf("%s %s: status=%d body=%s", method, path, response.Code, response.Body.String())
		}
		if status == http.StatusOK {
			assertOpenAPISuccessResponse(t, request, response)
		} else {
			assertOpenAPIResponse(t, request, response)
		}
		return response
	}
	// path 是当前卡密的完整编辑入口。
	path := "/api/v1/cards/" + strconv.FormatInt(cardID, 10) + "/api-editor"
	// response 是拥有者主动打开编辑的结果。
	response := perform(http.MethodGet, path, "", http.StatusOK)
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("完整模板不得缓存")
	}
	// editor 保存专用 HTTP 编辑模型。
	var editor cardAPIEditorResponse
	// err 表示专用编辑响应未能解码为契约模型。
	if err := json.Unmarshal(response.Body.Bytes(), &editor); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(editor.Headers, "editor-fixture-secret") || !strings.Contains(editor.Headers, "{idempotency_key}") || !strings.Contains(editor.Body, "{trigger_type}") {
		t.Fatal("编辑模板未完整返回")
	}
	// summaryPath 分别验证列表和普通详情仍不返回敏感模板。
	for _, summaryPath := range []string{"/api/v1/cards", "/api/v1/cards/" + strconv.FormatInt(cardID, 10) + "/details"} {
		if strings.Contains(perform(http.MethodGet, summaryPath, "", 200).Body.String(), "editor-fixture-secret") {
			t.Fatal("普通查询泄露凭证")
		}
	}
	// payload 是修改超时后的草稿，未重填已保存的密钥与正文。
	payload := `{"card_id":` + strconv.FormatInt(cardID, 10) + `,"api_config":{"url":"https://example.com/claim","method":"POST","timeout_seconds":15,"headers_action":"retain","params_action":"retain","retry_enabled":true}}`
	perform(http.MethodPost, "/api/v1/cards/test-api", payload, 200)
	// merged、err 是测试器真正收到的完整配置。
	merged, err := cardsapp.ParseAPIConfig(tester.config)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Timeout != 15 || merged.Headers["Authorization"] != "Bearer editor-fixture-secret" || merged.Headers["Idempotency-Key"] != "{idempotency_key}" || merged.Body["trigger_type"] != "{trigger_type}" {
		t.Fatal("测试未正确合并已保存配置")
	}
	// unchanged、err 验证测试没有修改保存的配置。
	unchanged, err := store.Cards.GetForDelivery(ctx, cardID)
	if err != nil || unchanged.APIConfig != saved {
		t.Fatal("测试不应写回卡密配置")
	}
	// invalid 去掉必要幂等键，应该在访问上游前拒绝。
	invalid := strings.Replace(payload, `"headers_action":"retain"`, `"headers_action":"clear"`, 1)
	perform(http.MethodPost, "/api/v1/cards/test-api", invalid, 400)
	if tester.calls != 1 {
		t.Fatal("无效草稿不应调用上游")
	}
	// created、err 创建另一个拥有者，验证管理员也不能读取他人的凭证。
	created, err := store.Users.Create(ctx, "editor-other", "editor-other@example.com", "pw")
	if err != nil || !created {
		t.Fatal("创建隔离用户失败", err)
	}
	// other、err 是另一位用户。
	other, err := store.Users.GetByUsername(ctx, "editor-other")
	if err != nil {
		t.Fatal(err)
	}
	// otherID、err 是另一位用户拥有的 API 卡密。
	otherID, err := store.Cards.Create(ctx, &db.CardFull{Name: "他人API", Type: "api", APIConfig: saved, Enabled: true, UserID: other.ID})
	if err != nil {
		t.Fatal(err)
	}
	perform(http.MethodGet, "/api/v1/cards/"+strconv.FormatInt(otherID, 10)+"/api-editor", "", 403)
	perform(http.MethodPost, "/api/v1/cards/test-api", strings.Replace(payload, `"card_id":`+strconv.FormatInt(cardID, 10), `"card_id":`+strconv.FormatInt(otherID, 10), 1), 403)
	perform(http.MethodGet, "/api/v1/cards/99999/api-editor", "", 404)
	perform(http.MethodGet, "/api/v1/cards/invalid/api-editor", "", 400)
	if tester.calls != 1 {
		t.Fatal("越权请求不应调用上游")
	}
	// anonymous 是没有登录会话的直接访问。
	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, path, nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatal("未登录访问未被拒绝")
	}
}
