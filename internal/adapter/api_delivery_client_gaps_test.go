package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	cardsapp "xianyu-go/internal/application/cards"
	"xianyu-go/internal/automation"
	"xianyu-go/internal/db"
)

// TestAPIDeliveryConstructors 覆盖发货客户端与测试器的构造函数（含空日志器默认值）。
func TestAPIDeliveryConstructors(t *testing.T) {
	if newAPIDeliveryClient(nil, nil) == nil || NewAPICardTester(nil, nil) == nil {
		t.Fatal("构造函数不应返回空值")
	}
	// nilClient 是空指针客户端，httpClient 应回退到全局策略客户端。
	var nilClient *apiDeliveryClient
	if nilClient.httpClient() == nil {
		t.Fatal("空客户端应回退默认 HTTP 客户端")
	}
	// zero 是未设置共享客户端的零值适配器。
	zero := &apiDeliveryClient{}
	if zero.httpClient() == nil {
		t.Fatal("零值客户端应惰性创建")
	}
}

// TestTruncateAPITestPreview 覆盖预览截断。
func TestTruncateAPITestPreview(t *testing.T) {
	if truncateAPITestPreview("short") != "short" {
		t.Fatal("短文本应原样返回")
	}
	// long 是超过预览上限的文本。
	long := strings.Repeat("a", apiTestPreviewLimit+5)
	if got := truncateAPITestPreview(long); len(got) != apiTestPreviewLimit+3 || !strings.HasSuffix(got, "...") {
		t.Fatalf("截断结果错误 len=%d", len(got))
	}
}

// TestAPICardTesterPOSTAndFailureBranches 覆盖测试请求的 POST、非对象 JSON、错误配置、取消与超大响应路径。
func TestAPICardTesterPOSTAndFailureBranches(t *testing.T) {
	ctx := context.Background()
	// client 是零值测试适配器。
	client := &apiDeliveryClient{}
	if _, err := client.Test(ctx, cardsapp.APIRequestTestInput{Config: "{"}); err == nil {
		t.Fatal("非法配置应失败")
	}
	// mode 决定本地服务本次的响应形状。
	mode := "array"
	// server 是按 mode 回复的本地端点，同时校验 POST 请求头与正文。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "array":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") == "" {
				t.Errorf("POST 请求头缺失: %s %q", r.Method, r.Header.Get("Content-Type"))
			}
			_, _ = w.Write([]byte(`[1,2]`))
		case "big":
			_, _ = w.Write([]byte(strings.Repeat("x", int(apiDeliveryResponseLimit)+1)))
		}
	}))
	defer server.Close()
	// postConfig 是带请求体与默认提取（无 response_path）的 POST 配置。
	postConfig := fmt.Sprintf(`{"url":%q,"method":"POST","timeout_seconds":5,"body":{"k":"{order_id}"},"headers":{"X-A":"b"}}`, server.URL)
	// result、err 是数组响应的诊断结果。
	result, err := client.Test(ctx, cardsapp.APIRequestTestInput{Config: postConfig})
	if err != nil || result.Status != "success" || result.ExtractedValue != "[1,2]" || len(result.ResponseFields) != 0 {
		t.Fatalf("数组响应诊断错误: %+v %v", result, err)
	}
	mode = "big"
	// bigResult、bigErr 是超大响应的失败诊断。
	bigResult, bigErr := client.Test(ctx, cardsapp.APIRequestTestInput{Config: postConfig})
	if bigErr == nil || bigResult.Status != "failed" {
		t.Fatalf("超大响应应失败: %+v %v", bigResult, bigErr)
	}
	// cancelled 是已取消上下文，应原样返回取消错误。
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := client.Test(cancelled, cardsapp.APIRequestTestInput{Config: postConfig}); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消错误: %v", err)
	}
	// closed 是已关闭的端点，应返回不含地址的通用错误。
	closed := httptest.NewServer(http.NotFoundHandler())
	// closedURL 已关闭的本地测试端点地址，用于模拟网络不可达。
	closedURL := closed.URL
	closed.Close()
	if _, err := client.Test(ctx, cardsapp.APIRequestTestInput{Config: fmt.Sprintf(`{"url":%q,"method":"GET","timeout_seconds":5}`, closedURL)}); err == nil || strings.Contains(err.Error(), closedURL) {
		t.Fatalf("网络失败应返回通用错误: %v", err)
	}
	// 非 JSON 成功响应只返回预览，无提取值。
	mode = "plain"
	// plainResult 是非 JSON 响应的诊断。
	plainResult, err := client.Test(ctx, cardsapp.APIRequestTestInput{Config: postConfig})
	if err != nil || plainResult.ExtractedValue != "" {
		t.Fatalf("非 JSON 响应诊断: %+v %v", plainResult, err)
	}
}

// TestAPIDeliveryFetchErrorBranches 覆盖 Fetch 的商品详情、配置、网络、重试耗尽和取消分支。
func TestAPIDeliveryFetchErrorBranches(t *testing.T) {
	ctx := context.Background()
	// oldGaps 保存全局退避配置，测试结束后恢复。
	oldGaps := apiDeliveryRetryGaps
	apiDeliveryRetryGaps = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { apiDeliveryRetryGaps = oldGaps }()
	if _, err := (&apiDeliveryClient{}).Fetch(ctx, automation.APICardRequest{Config: "{"}); err == nil {
		t.Fatal("非法配置应失败")
	}
	// store、cleanup 是承载商品详情的临时库。
	store, cleanup := newAdapterTestStore(t)
	defer cleanup()
	// seen 记录服务端收到的商品详情替换值。
	var seen string
	// server 回显 detail 查询参数后返回内容。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("d")
		_, _ = w.Write([]byte("CODE"))
	}))
	defer server.Close()
	// config 把商品详情放进查询参数。
	config := fmt.Sprintf(`{"url":%q,"method":"GET","timeout_seconds":5,"params":{"d":"{item_detail}"}}`, server.URL)
	// client 持有真实 SQLite 商品库。
	client := &apiDeliveryClient{store: store}
	if err := store.Items.Upsert(ctx, &db.ItemInfoRow{CookieID: "cid", ItemID: "i1", ItemTitle: "t", ItemDetail: "DETAIL"}); err != nil {
		t.Fatalf("写入商品: %v", err)
	}
	// err、res 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存被测函数返回的结果对象，用于断言其内容。
	if res, err := client.Fetch(ctx, automation.APICardRequest{Config: config, AccountID: "cid", ItemID: "i1"}); err != nil || res.Content != "CODE" || seen != "DETAIL" {
		t.Fatalf("商品详情替换: %+v %v seen=%q", res, err, seen)
	}
	// 商品不存在时详情保持为空且不报错。
	if _, err := client.Fetch(ctx, automation.APICardRequest{Config: config, AccountID: "cid", ItemID: "missing"}); err != nil || seen != "" {
		t.Fatalf("缺失商品: %v seen=%q", err, seen)
	}
	// 非法 URL（控制字符）使 url.Parse 失败。
	if _, err := client.Fetch(ctx, automation.APICardRequest{Config: `{"url":"http://a\u007f.com","method":"GET","timeout_seconds":5}`}); err == nil {
		t.Fatal("非法 URL 应失败")
	}
	// 数据库关闭后商品详情查询出现非 NotFound 错误。
	cleanup()
	if _, err := client.Fetch(ctx, automation.APICardRequest{Config: config, AccountID: "cid", ItemID: "i1"}); err == nil || !strings.Contains(err.Error(), "读取商品详情") {
		t.Fatalf("数据库错误应上抛: %v", err)
	}
}

// TestAPIDeliveryFetchRetryExhaustionAndCancel 覆盖重试耗尽、退避期间取消与网络错误重试。
func TestAPIDeliveryFetchRetryExhaustionAndCancel(t *testing.T) {
	// oldGaps 保存全局退避配置，测试结束后恢复。
	oldGaps := apiDeliveryRetryGaps
	defer func() { apiDeliveryRetryGaps = oldGaps }()
	apiDeliveryRetryGaps = []time.Duration{time.Millisecond, time.Millisecond}
	// calls 统计服务端收到的请求次数。
	calls := 0
	// server 始终返回 503，触发重试直至耗尽。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	// config 在头部携带幂等键以启用重试。
	config := fmt.Sprintf(`{"url":%q,"method":"GET","timeout_seconds":5,"retry_enabled":true,"headers":{"X-Key":"{idempotency_key}"}}`, server.URL)
	// result、err 是耗尽重试后的结果。
	result, err := (&apiDeliveryClient{}).Fetch(context.Background(), automation.APICardRequest{Config: config})
	if err == nil || calls != apiDeliveryRetryLimit || !result.Dispatched {
		t.Fatalf("重试耗尽: calls=%d res=%+v err=%v", calls, result, err)
	}
	// 退避期间取消：使用较长退避并在服务端首次响应后取消上下文。
	apiDeliveryRetryGaps = []time.Duration{time.Hour, time.Hour}
	// ctx、cancel 在首次请求后取消。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// cancelServer 在响应前取消调用方上下文。
	cancelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		cancel()
	}))
	defer cancelServer.Close()
	// cancelConfig 与 config 相同但指向取消服务。
	cancelConfig := strings.Replace(config, server.URL, cancelServer.URL, 1)
	if _, err := (&apiDeliveryClient{}).Fetch(ctx, automation.APICardRequest{Config: cancelConfig}); !errors.Is(err, context.Canceled) {
		t.Fatalf("退避期间取消: %v", err)
	}
	// 网络错误：服务已关闭，仍然按网络类别重试后返回可分类错误。
	apiDeliveryRetryGaps = []time.Duration{time.Millisecond, time.Millisecond}
	// dead 是关闭后的端点地址。
	dead := httptest.NewServer(http.NotFoundHandler())
	// deadURL 已关闭的本地测试端点地址，用于模拟网络不可达。
	deadURL := dead.URL
	dead.Close()
	// netResult、netErr 是网络失败的最终结果。
	netResult, netErr := (&apiDeliveryClient{}).Fetch(context.Background(), automation.APICardRequest{Config: strings.Replace(config, server.URL, deadURL, 1)})
	if netErr == nil || !netResult.Dispatched {
		t.Fatalf("网络失败应标记已发出: %+v %v", netResult, netErr)
	}
	// 取消上下文直接调用：doAPIRequest 返回取消错误且未发出。
	// done 是已取消上下文。
	done, doneCancel := context.WithCancel(context.Background())
	doneCancel()
	// err、res 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存被测函数返回的结果对象，用于断言其内容。
	if res, err := (&apiDeliveryClient{}).Fetch(done, automation.APICardRequest{Config: config}); !errors.Is(err, context.Canceled) || res.Dispatched {
		t.Fatalf("预取消: %+v %v", res, err)
	}
}

// TestAPIDeliveryDoRequestResponseBranches 覆盖 doAPIRequest 的空响应、读取失败和 POST 默认类型分支。
func TestAPIDeliveryDoRequestResponseBranches(t *testing.T) {
	// status/body 决定服务端响应。
	status, body := http.StatusOK, "  "
	// server 回复预设内容并记录 POST 的 Content-Type。
	var contentType string
	// server 测试中的局部对象，承载被测流程的输入、依赖或输出。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contentType = r.Header.Get("Content-Type")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	// client 是零值适配器。
	client := &apiDeliveryClient{}
	// config 是不带内容类型的 POST 配置（空 content_type 走默认）。
	config := cardsapp.APIConfig{URL: server.URL, Method: http.MethodPost, Timeout: 5}
	if _, err := client.doAPIRequest(context.Background(), config, server.URL, nil, []byte("{}"), 1); err == nil {
		t.Fatal("空白响应应失败")
	}
	if contentType != "application/json" {
		t.Fatalf("默认内容类型: %q", contentType)
	}
	status, body = http.StatusOK, `{"data":null}`
	// err、res 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存被测函数返回的结果对象，用于断言其内容。
	if res, err := client.doAPIRequest(context.Background(), config, server.URL, nil, nil, 1); err != nil || res.Content != `{"data":null}` {
		t.Fatalf("data 为 null 回退到整个文档文本: %+v %v", res, err)
	}
	status, body = http.StatusInternalServerError, strings.Repeat("x", int(apiDeliveryResponseLimit)+1)
	// err、res 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存被测函数返回的结果对象，用于断言其内容。
	if res, err := client.doAPIRequest(context.Background(), config, server.URL, nil, nil, 1); err == nil || !res.Dispatched {
		t.Fatalf("5xx 超大正文: %+v %v", res, err)
	}
	status, body = http.StatusOK, `{"x":1}`
	// pathConfig 要求不存在的响应路径。
	pathConfig := config
	pathConfig.ResponsePath = "missing"
	if _, err := client.doAPIRequest(context.Background(), pathConfig, server.URL, nil, nil, 1); err == nil {
		t.Fatal("路径缺失应失败")
	}
	if _, err := client.doAPIRequest(context.Background(), config, "http://a\x7f", nil, nil, 1); err == nil {
		t.Fatal("非法 URL 应失败")
	}
}

// TestAPIDeliveryHelperEdgeCases 覆盖路径解析、重试判定、编码与标量转换的剩余边界。
func TestAPIDeliveryHelperEdgeCases(t *testing.T) {
	// doc 是路径解析使用的 JSON 文档。
	doc := map[string]any{"a": []any{"x", map[string]any{"b": "y"}}, "s": "str"}
	// cases 列出路径与期望命中结果。
	cases := []struct {
		/* 路径 */ path string
		/* 是否命中 */ found bool
	}{
		{"a[0]", true}, {"a[1].b", true}, {"a[5]", false}, {"s[0]", false}, {"s.x", false},
		{"a..b", false}, {"a[x]", false}, {"a[-1]", false}, {"a[]", false}, {"a[0", false}, {"nope", false},
	}
	// tc 当前表驱动用例，包含输入与期望输出。
	for _, tc := range cases {
		// found 测试中的局部对象，承载被测流程的输入、依赖或输出。
		if _, found := lookupAPIResponsePath(doc, tc.path); found != tc.found {
			t.Fatalf("path %q found=%v", tc.path, found)
		}
	}
	if shouldRetryAPIDelivery(nil, 1, 3) || shouldRetryAPIDelivery(&apiDeliveryHTTPError{statusCode: 500}, 3, 3) {
		t.Fatal("无错误或已达上限不应重试")
	}
	if !shouldRetryAPIDelivery(&apiDeliveryHTTPError{statusCode: 429}, 1, 3) || shouldRetryAPIDelivery(&apiDeliveryHTTPError{statusCode: 400}, 1, 3) {
		t.Fatal("HTTP 重试判定错误")
	}
	// netErr 是网络类别错误，需验证 Error/Unwrap 与重试。
	netErr := &apiDeliveryRetryableError{kind: "network", err: io.EOF}
	if !shouldRetryAPIDelivery(netErr, 1, 3) || netErr.Error() == "" || !errors.Is(netErr, io.EOF) {
		t.Fatal("网络错误判定错误")
	}
	if shouldRetryAPIDelivery(errors.New("other"), 1, 3) {
		t.Fatal("未知错误不应重试")
	}
	if apiDeliveryCarriesIdempotencyKey("GET", nil, nil, nil, "") {
		t.Fatal("空幂等键不应判定为携带")
	}
	if got := apiJSONScalar(map[string]any{"a": 1}); got != `{"a":1}` {
		t.Fatalf("复杂值: %q", got)
	}
	if got := apiJSONScalar(make(chan int)); got == "" {
		t.Fatal("不可编码值应回退 Sprint")
	}
	if apiJSONScalar(nil) != "" {
		t.Fatal("nil 应为空文本")
	}
	// 响应读取：非 JSON 文本、对象默认字段与空响应。
	if got, err := readAPIDeliveryResponse(strings.NewReader(`{"card":"C"}`), "", ""); err != nil || got != "C" {
		t.Fatalf("card 默认字段: %q %v", got, err)
	}
	// err、got 依次为：保存本步骤调用返回的错误，用于断言预期的成功或失败路径；保存被测函数的实际返回值，用于与期望值比较。
	if got, err := readAPIDeliveryResponse(strings.NewReader(`{"z":1}`), "", ""); err != nil || got != `{"z":1}` {
		t.Fatalf("无默认字段: %q %v", got, err)
	}
	if _, err := readAPIDeliveryResponse(strings.NewReader(" "), "", ""); err == nil {
		t.Fatal("空响应应失败")
	}
	if _, err := readAPIDeliveryResponse(errReader{}, "", ""); err == nil {
		t.Fatal("读取错误应上抛")
	}
	if _, err := readAPIDeliveryBody(errReader{}); err == nil {
		t.Fatal("读取错误应上抛")
	}
	if isXMLTagName("") || isXMLTagName("1a") || isXMLTagName("a b") || !isXMLTagName("a_b-1") {
		t.Fatal("XML 标签名判定错误")
	}
}

// errReader 是总是读取失败的测试读取器。
type errReader struct{}

// Read 总是返回错误，模拟网络读取中断。
func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }
