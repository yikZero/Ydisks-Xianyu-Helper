package server

import (
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	chatapp "xianyu-go/internal/application/chat"
)

// TestParseItemPublishSpecsBranches 覆盖规格解析的空值、不完整、格式错误、图片引用与价格错误分支。
func TestParseItemPublishSpecsBranches(t *testing.T) {
	// err、skus、specs 分别保存本步骤的返回值：err为本步骤返回的错误，用于断言失败或成功路径；skus 为转换后的 SKU 组合；specs 为转换后的规格维度
	if specs, skus, err := parseItemPublishSpecs(" ", "", 0); err != nil || specs != nil || skus != nil {
		t.Errorf("empty should be nil: %v", err)
	}
	if _, _, err := parseItemPublishSpecs("[]", "", 0); err == nil {
		t.Error("incomplete should fail")
	}
	if _, _, err := parseItemPublishSpecs("{", "[]", 0); err == nil {
		t.Error("bad specs json should fail")
	}
	if _, _, err := parseItemPublishSpecs("[]", "{", 0); err == nil {
		t.Error("bad skus json should fail")
	}
	// badImage 引用了超出范围的规格图片下标。
	badImage := `[{"propertyName":"颜色","propertyValues":[{"propertyValue":"红","image_index":3}]}]`
	if _, _, err := parseItemPublishSpecs(badImage, "[]", 1); err == nil {
		t.Error("out of range image should fail")
	}
	// badPrice 的 SKU 价格不是合法金额。
	if _, _, err := parseItemPublishSpecs("[]", `[{"price":"abc","quantity":1}]`, 0); err == nil {
		t.Error("bad price should fail")
	}
	// good 同时包含被忽略的空规格值、带图片的规格值和一个 SKU。
	good := `[{"propertyName":" 颜色 ","supportImage":true,"propertyValues":[{"propertyValue":" "},{"propertyValue":"红","image_index":0},{"propertyValue":"蓝"}]}]`
	// err、skus、specs 分别保存本步骤的返回值：err为本步骤返回的错误，用于断言失败或成功路径；skus 为转换后的 SKU 组合；specs 为转换后的规格维度
	specs, skus, err := parseItemPublishSpecs(good, `[{"price":"1.5","quantity":2,"propertyList":[{"propertyText":" 颜色 ","valueText":" 红 "}]}]`, 1)
	if err != nil || len(specs) != 1 || len(specs[0].Values) != 2 || specs[0].Values[1].ImageIndex != -1 || len(skus) != 1 || skus[0].PriceCents != 150 {
		t.Fatalf("good specs=%+v skus=%+v err=%v", specs, skus, err)
	}
}

// TestChatSessionCursorInvalidAndNil 覆盖游标 JSON 非法、缺少会话标识和 nil 游标。
func TestChatSessionCursorInvalidAndNil(t *testing.T) {
	// notJSON 是合法 Base64 但内容不是 JSON 的游标。
	notJSON := base64.RawURLEncoding.EncodeToString([]byte("not-json"))
	if _, err := decodeChatSessionCursor(notJSON); err == nil {
		t.Error("non-json cursor should fail")
	}
	// err、got 分别保存本步骤的返回值：err为本步骤返回的错误，用于断言失败或成功路径；got为被测函数的实际返回值，用于与期望值比较
	if got, err := encodeChatSessionCursor(nil); err != nil || got != "" {
		t.Errorf("nil cursor => %q %v", got, err)
	}
	if _, err := encodeChatSessionCursor(&chatapp.SessionCursor{ChatID: " "}); err == nil {
		t.Error("blank chat id should fail")
	}
}

// TestLoginLimiterHelpers 覆盖无端口远端地址和超过 2048 桶时清理过期记录。
func TestLoginLimiterHelpers(t *testing.T) {
	// req 的 RemoteAddr 不含端口。
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.9"
	if got := loginClientIP(req); got != "10.0.0.9" {
		t.Errorf("ip = %q", got)
	}
	// limiter 是待测的登录失败限流器。
	limiter := newLoginFailureLimiter()
	// base 是测试使用的固定时钟起点。
	base := time.Unix(1_700_000_000, 0)
	// i 为用于填充过期记录的序号。
	for i := 0; i < 1100; i++ {
		limiter.failure("ip"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('A'+i/26)), "p"+string(rune(0x4e00+i)), base)
	}
	// later 晚于窗口，使先前的桶全部过期。
	later := base.Add(limiter.window + time.Second)
	limiter.failure("new-ip", "new-principal", later)
	limiter.mu.Lock()
	// remaining 是清理后仍保留的桶数量。
	remaining := len(limiter.buckets)
	limiter.mu.Unlock()
	if remaining > 2 {
		t.Errorf("expired buckets should be purged, remaining=%d", remaining)
	}
}
