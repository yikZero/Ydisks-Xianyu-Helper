package mtop

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"xianyu-go/internal/xianyu/cookierefresh"
)

// TestNilReceiverErrorsAndRiskClassification 覆盖各错误类型空接收者、风控错误格式与文本回退分类。
func TestNilReceiverErrorsAndRiskClassification(t *testing.T) {
	// responseErr 保存空 MTOP 响应错误指针，用于验证空接收者的安全返回。
	var responseErr *MTopResponseError
	if responseErr.Error() != "" || responseErr.Unwrap() != nil {
		t.Fatal("空 MTOP 响应错误应返回空文本和空原因")
	}
	// sessionErr 保存空会话错误指针，用于验证空接收者的安全返回。
	var sessionErr *SessionExpiredError
	if sessionErr.Error() != "" {
		t.Fatal("空会话错误应返回空文本")
	}
	// riskErr 保存空风控错误指针，用于验证空接收者的安全返回。
	var riskErr *RiskVerificationError
	if riskErr.Error() != "" {
		t.Fatal("空风控错误应返回空文本")
	}
	// withoutURL 是不带验证地址的风控错误，文本只含前缀与平台原因。
	withoutURL := (&RiskVerificationError{API: "接口", Ret: []string{"RGV587::x"}}).Error()
	if !strings.Contains(withoutURL, "接口触发闲鱼安全验证") || strings.Contains(withoutURL, "验证地址") {
		t.Fatalf("text=%q", withoutURL)
	}
	if IsRiskVerificationErr(nil) {
		t.Fatal("nil 不是风控错误")
	}
	if !IsRiskVerificationErr(errors.New("命中 captcha 页面")) || IsRiskVerificationErr(errors.New("普通失败")) {
		t.Fatal("文本回退分类错误")
	}
	if !IsRiskVerificationErr(&RiskVerificationError{}) {
		t.Fatal("结构化风控错误应被识别")
	}
	if !isRiskVerificationRet([]string{"FAIL_SYS_USER_VALIDATE::x"}) || isRiskVerificationRet([]string{"OK"}) {
		t.Fatal("ret 风控分类错误")
	}
	if !IsSessionExpiredErr(&SessionExpiredError{}) || IsSessionExpiredErr(nil) {
		t.Fatal("会话过期分类错误")
	}
}

// TestCookieSessionReplaceAndFlatPaths 覆盖 Cookie 会话的空接收者、权威替换与扁平写入优先级。
func TestCookieSessionReplaceAndFlatPaths(t *testing.T) {
	// nilSession 保存空会话指针，用于验证空接收者调用不会 panic。
	var nilSession *CookieSession
	nilSession.replace([]cookierefresh.BrowserCookie{})
	nilSession.replaceFlat("a=1")
	if nilSession.absorb("https://h5api.m.goofish.com/", []string{"a=1"}) != "" {
		t.Fatal("空会话吸收应返回空串")
	}
	// session 是扁平 Cookie 会话。
	_, session := WithFlatCookieSession(t.Context(), "a=1")
	session.replaceFlat("a=2")
	session.replaceFlat("a=2")
	// changed 保存被测函数返回的是否发生变化标志；value 保存被测函数返回的值。
	if value, _, changed := session.State(); value != "a=2" || !changed {
		t.Fatalf("value=%q changed=%v", value, changed)
	}
	// 空快照替换会把会话切换到权威空集合。
	session.replace([]cookierefresh.BrowserCookie{})
	session.replaceFlat("a=3")
	// snapshot 保存被测对象返回的 Cookie 快照。
	if _, snapshot, _ := session.State(); snapshot == nil {
		t.Fatal("权威会话应返回非空快照对象")
	}
	if got := normalizedCompleteSnapshot(nil); got == nil || len(got) != 0 {
		t.Fatalf("got=%v", got)
	}
}

// TestApplyFlatSetCookiesEdges 覆盖无效 Set-Cookie 被忽略、过期 Cookie 删除与无变化时保持原值。
func TestApplyFlatSetCookiesEdges(t *testing.T) {
	// now 是固定时钟。
	now := time.Unix(1_700_000_000, 0)
	// changed 保存被测函数返回的是否发生变化标志；got 保存被测函数的实际返回值，用于与期望值比较。
	if got, changed := applyFlatSetCookies("a=1", []string{"%%%bad", "=novalue"}, now); changed || got != "a=1" {
		t.Fatalf("got=%q changed=%v", got, changed)
	}
	// changed 保存被测函数返回的是否发生变化标志；got 保存被测函数的实际返回值，用于与期望值比较。
	if got, changed := applyFlatSetCookies("a=1; b=2", []string{"a=; Max-Age=-1"}, now); !changed || strings.Contains(got, "a=") {
		t.Fatalf("got=%q changed=%v", got, changed)
	}
	if got := absorbMTopResponseCookies(t.Context(), "fallback", nil); got != "fallback" {
		t.Fatalf("got=%q", got)
	}
	// resp 是不带 Set-Cookie 的响应，无会话时应保持兜底值。
	resp := &http.Response{Header: http.Header{}}
	if got := absorbMTopResponseCookies(t.Context(), "fallback", resp); got != "fallback" {
		t.Fatalf("got=%q", got)
	}
}
