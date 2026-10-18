package db

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestCardSummaryHelpers 覆盖卡券 API 摘要的标量解析、超时兼容和模板校验分支。
func TestCardSummaryHelpers(t *testing.T) {
	// cases 保存不同公开配置输入及其摘要预期。
	cases := []struct {
		name  string
		raw   string
		ready bool
	}{
		{name: "valid", raw: `{"url":"https://example.test/api","method":"post","timeout_seconds":5,"retry_enabled":true,"headers":{"x":"{idempotency_key}"}}`, ready: true},
		{name: "legacy-timeout", raw: `{"url":"http://example.test","timeout":"7"}`, ready: true},
		{name: "invalid-json", raw: "{", ready: false},
		{name: "invalid-url", raw: `{"url":"file:///tmp/a"}`, ready: false},
		{name: "invalid-method", raw: `{"url":"https://example.test","method":"PUT"}`, ready: false},
		{name: "invalid-timeout", raw: `{"url":"https://example.test","timeout_seconds":61}`, ready: false},
		{name: "retry-without-key", raw: `{"url":"https://example.test","retry_enabled":true}`, ready: false},
	}
	// item 表示当前待验证的 API 摘要样例。
	for _, item := range cases {
		// summary 保存当前配置的非敏感摘要。
		summary := summarizeCardAPIConfig("api", item.raw)
		if summary == nil || summary.Ready != item.ready {
			t.Fatalf("%s summary=%+v want ready=%v", item.name, summary, item.ready)
		}
	}
	// nonAPI 验证非 API 卡券不产生 API 摘要。
	nonAPI := summarizeCardAPIConfig("text", `{}`)
	if nonAPI != nil {
		t.Fatalf("non-api summary=%+v", nonAPI)
	}
	// timeoutDefault、timeoutInvalid 验证新旧字段和非法文本的解析结果。
	timeoutDefault := parseSummaryTimeout(map[string]json.RawMessage{})
	// timeoutInvalid 保存非法超时文本的解析结果。
	timeoutInvalid := parseSummaryTimeout(map[string]json.RawMessage{"timeout_seconds": json.RawMessage(`"bad"`)})
	if timeoutDefault != 10 || timeoutInvalid != 0 {
		t.Fatalf("timeouts=%d/%d", timeoutDefault, timeoutInvalid)
	}
	// scalarNumber、scalarInvalid 验证摘要标量的数字与非法 JSON 解析。
	scalarNumber := summaryRawString(json.RawMessage(`12`))
	// scalarInvalid 保存非法 JSON 标量的空结果。
	scalarInvalid := summaryRawString(json.RawMessage(`{`))
	if scalarNumber != "12" || scalarInvalid != "" {
		t.Fatalf("scalars=%q/%q", scalarNumber, scalarInvalid)
	}
	// configured 验证空对象、空字符串和有效模板的配置判定。
	if templateConfigured(json.RawMessage(`{}`)) || templateConfigured(json.RawMessage(`""`)) || !templateConfigured(json.RawMessage(`{"x":1}`)) {
		t.Fatal("templateConfigured result incorrect")
	}
	// summary 是公开字段校验使用的有效摘要。
	summary := CardAPIConfigSummary{URL: "https://example.test", Method: "GET", TimeoutSeconds: 5, RetryEnabled: true}
	// missingKey 验证重试配置缺少幂等占位符时被拒绝。
	missingKey := validateSummaryAPIConfig(summary, map[string]any{"headers": map[string]any{"x": "no-key"}})
	if missingKey == nil {
		t.Fatal("retry without idempotency key should fail")
	}
	// nestedKey 验证数组和对象嵌套中的占位符可被找到。
	if !summaryTemplateContains([]any{map[string]any{"x": "{idempotency_key}"}}, "{idempotency_key}") || summaryTemplateContains(42, "x") {
		t.Fatal("nested template matching incorrect")
	}
	// hasEntries 验证数组型历史正文模板被判定为已配置，空数组和字符串不参与 POST 正文判定。
	hasEntries := summaryTemplateHasEntries([]any{map[string]any{"k": "v"}})
	// emptyEntries、stringEntries 分别验证空数组和非容器模板值的判定结果。
	emptyEntries, stringEntries := summaryTemplateHasEntries([]any{}), summaryTemplateHasEntries("plain")
	if !hasEntries || emptyEntries || stringEntries {
		t.Fatalf("hasEntries=%v/%v/%v", hasEntries, emptyEntries, stringEntries)
	}
}

// TestCardSummaryRetryIdempotencyScope 验证摘要层的幂等重试前置条件与真实发送载荷保持一致。
// 摘要结论必须与保存路径相同，否则列表会显示可用、保存却报错，或反之。
func TestCardSummaryRetryIdempotencyScope(t *testing.T) {
	// cases 保存按请求方法、正文和幂等键位置划分的摘要预期。
	cases := []struct {
		// name 是当前用例的可读名称。
		name string
		// raw 是数据库中的完整 API 配置文本。
		raw string
		// ready 表示摘要是否允许该配置被规则选择。
		ready bool
		// wantMessage 是配置不可用时必须包含的错误文本片段。
		wantMessage string
	}{
		{
			name:  "POST 仅正文携带幂等键",
			raw:   `{"url":"https://example.test","method":"POST","retry_enabled":true,"body":{"idempotency_key":"{idempotency_key}"}}`,
			ready: true,
		},
		{
			name:  "POST 正文为空时参数回退为正文",
			raw:   `{"url":"https://example.test","method":"POST","retry_enabled":true,"params":{"idempotency_key":"{idempotency_key}"}}`,
			ready: true,
		},
		{
			name:  "GET 查询参数携带幂等键",
			raw:   `{"url":"https://example.test","method":"GET","retry_enabled":true,"params":{"idempotency_key":"{idempotency_key}"}}`,
			ready: true,
		},
		{
			name:        "POST 正文非空时参数不会发送",
			raw:         `{"url":"https://example.test","method":"POST","retry_enabled":true,"params":{"idempotency_key":"{idempotency_key}"},"body":{"order_id":"{order_id}"}}`,
			ready:       false,
			wantMessage: "请求正文",
		},
		{
			name:        "GET 请求正文不会发送",
			raw:         `{"url":"https://example.test","method":"GET","retry_enabled":true,"body":{"idempotency_key":"{idempotency_key}"}}`,
			ready:       false,
			wantMessage: "查询参数",
		},
		{
			name:  "未启用重试时不再要求幂等键",
			raw:   `{"url":"https://example.test","method":"POST","body":{"order_id":"{order_id}"}}`,
			ready: true,
		},
	}
	// item 表示当前待验证的 API 配置样例。
	for _, item := range cases {
		// summary 保存当前配置的脱敏摘要。
		summary := summarizeCardAPIConfig("api", item.raw)
		if summary == nil || summary.Ready != item.ready {
			t.Fatalf("%s summary=%+v want ready=%v", item.name, summary, item.ready)
		}
		if item.wantMessage != "" && !strings.Contains(summary.ValidationError, item.wantMessage) {
			t.Fatalf("%s 摘要错误文本应提示 %q: %q", item.name, item.wantMessage, summary.ValidationError)
		}
	}
}

// TestCardSummaryRepositoryPaths 验证卡券完整读取和脱敏摘要读取不会混淆敏感 API 模板。
func TestCardSummaryRepositoryPaths(t *testing.T) {
	store, cleanup := newTestDB(t)
	defer cleanup()
	ctx := context.Background()
	// userID 保存卡券所属用户主键。
	userID, _ := seedAccount(t, store)
	// apiCard 保存带请求模板的 API 卡券。
	apiCard := &CardFull{Name: "API卡", Type: "api", APIConfig: `{"url":"https://example.test","method":"GET","timeout_seconds":5}`, Enabled: true, UserID: userID}
	// apiID、createErr 保存 API 卡券创建结果。
	apiID, createErr := store.Cards.Create(ctx, apiCard)
	if createErr != nil || apiID == 0 {
		t.Fatalf("apiID=%d err=%v", apiID, createErr)
	}
	// summary、summaryErr 保存脱敏摘要读取结果。
	summary, summaryErr := store.Cards.GetSummary(ctx, apiID)
	if summaryErr != nil || summary == nil || summary.APIConfig != "" || summary.APIConfigSummary == nil || !summary.APIConfigSummary.Ready {
		t.Fatalf("summary=%+v err=%v", summary, summaryErr)
	}
	// full、fullErr 保存自动发货专用的完整配置读取结果。
	full, fullErr := store.Cards.GetForDelivery(ctx, apiID)
	if fullErr != nil || full == nil || full.APIConfig == "" {
		t.Fatalf("full=%+v err=%v", full, fullErr)
	}
	// all、allErr 保存用户卡券脱敏列表。
	all, allErr := store.Cards.AllForUserSummary(ctx, userID)
	if allErr != nil || len(all) != 1 || all[0].APIConfig != "" || all[0].APIConfigSummary == nil {
		t.Fatalf("all=%+v err=%v", all, allErr)
	}
	// missing、missingErr 验证不存在卡券的统一错误。
	missing, missingErr := store.Cards.GetSummary(ctx, 999999)
	if missing != nil || missingErr == nil {
		t.Fatalf("missing=%+v err=%v", missing, missingErr)
	}
}
