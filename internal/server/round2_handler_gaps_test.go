package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	automationapp "xianyu-go/internal/application/automation"
	cardsapp "xianyu-go/internal/application/cards"
	notificationsapp "xianyu-go/internal/application/notifications"
)

// ruleGapPort 包装自动化规则端口，让分页查询、更新与删除按预设返回错误。
type ruleGapPort struct {
	// AutomationRulesPort 提供其余规则能力的真实实现。
	AutomationRulesPort
	// pageCalls 统计 ListPageForUser 的调用次数，用于让第二次（页码回退后）查询失败。
	pageCalls int
	// failSecondPage 为真时第二次分页查询返回错误，并让首次返回总数 1 以触发页码回退。
	failSecondPage bool
	// normalizeErr 是 NormalizeForUpdate 的错误。
	normalizeErr error
	// updateErr 是 Update 的错误。
	updateErr error
	// deleteErr 是 Delete 的错误。
	deleteErr error
}

// ListPageForUser 首次返回总数 1，按配置让第二次失败。
func (p *ruleGapPort) ListPageForUser(ctx context.Context, f automationapp.RuleFilter) ([]automationapp.Rule, int, error) {
	if !p.failSecondPage {
		return p.AutomationRulesPort.ListPageForUser(ctx, f)
	}
	p.pageCalls++
	if p.pageCalls > 1 {
		return nil, 0, errors.New("second page failed")
	}
	return nil, 1, nil
}

// CountByTriggerForUser 在模拟分页时返回空统计。
func (p *ruleGapPort) CountByTriggerForUser(ctx context.Context, f automationapp.RuleFilter) (map[string]int, error) {
	if p.failSecondPage {
		return map[string]int{}, nil
	}
	return p.AutomationRulesPort.CountByTriggerForUser(ctx, f)
}

// NormalizeForUpdate 返回预设错误。
func (p *ruleGapPort) NormalizeForUpdate(context.Context, int64, int64, automationapp.RuleDraft) (automationapp.RuleInput, error) {
	return automationapp.RuleInput{}, p.normalizeErr
}

// Update 返回预设错误。
func (p *ruleGapPort) Update(context.Context, int64, int64, automationapp.RuleInput) error {
	return p.updateErr
}

// Delete 返回预设错误。
func (p *ruleGapPort) Delete(context.Context, int64, int64) error { return p.deleteErr }

// TestAutomationRuleHandlerRound2 覆盖规则列表的参数校验与更新、删除的错误映射。
func TestAutomationRuleHandlerRound2(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	// port 是可变规则端口桩。
	port := &ruleGapPort{AutomationRulesPort: srv.applications.automationRules}
	srv.applications.automationRules = port
	handler := srv.Router()
	cookie := loginHelper(t, handler)

	// listCases 描述列表请求与期望状态。
	listCases := []struct {
		// path 是列表请求路径与查询串。
		path string
		want int
	}{
		{"/automation-rules?page=1&page_size=0", 200},
		{"/automation-rules?page=1&cookie_id=ghost", 403},
		{"/automation-rules?page=1&trigger_type=bogus", 400},
	}
	// tc 表示当前列表场景。
	for _, tc := range listCases {
		// recorder 是调用 "GET" tc.path 得到的响应记录，其状态码用于断言该场景的错误映射
		if recorder := serveChatCoverageRequest(handler, cookie, "GET", tc.path, ""); recorder.Code != tc.want {
			t.Errorf("%s => %d want %d", tc.path, recorder.Code, tc.want)
		}
	}
	port.failSecondPage = true
	// recorder 是调用 "GET" "/automation-rules?page=9&page_size=10" 得到的响应记录，其状态码用于断言该场景的错误映射
	if recorder := serveChatCoverageRequest(handler, cookie, "GET", "/automation-rules?page=9&page_size=10", ""); recorder.Code != http.StatusInternalServerError {
		t.Errorf("fallback page failure => %d", recorder.Code)
	}
	port.failSecondPage = false

	// updateCases 描述更新请求下的规范化与写入错误。
	updateCases := []struct {
		// name 是子场景名称。
		name string
		// body 是请求体。
		body string
		// normalizeErr、updateErr 是注入错误。
		normalizeErr, updateErr error
		want                    int
	}{
		{"bad body", `{`, nil, nil, 400},
		{"not found", `{}`, automationapp.ErrRuleNotFound, nil, 404},
		{"pricing conflict", `{}`, automationapp.ErrPricingModeConflict, nil, 409},
		{"invalid", `{}`, errors.New("bad rule"), nil, 400},
		{"template changed", `{}`, nil, automationapp.ErrDeliveryTemplateUnavailable, 409},
		{"update conflict", `{}`, nil, automationapp.ErrPricingModeConflict, 409},
		{"update missing", `{}`, nil, automationapp.ErrRuleNotFound, 404},
		{"update failure", `{}`, nil, errors.New("db"), 500},
		{"ok with custom variables", `{"actions":[{"custom_variables":{"k":"v"}}]}`, nil, nil, 200},
	}
	// tc 表示当前更新场景。
	for _, tc := range updateCases {
		port.normalizeErr, port.updateErr = tc.normalizeErr, tc.updateErr
		// recorder 是调用 "PUT" "/automation-rules/1" 得到的响应记录，其状态码用于断言该场景的错误映射
		if recorder := serveChatCoverageRequest(handler, cookie, "PUT", "/automation-rules/1", tc.body); recorder.Code != tc.want {
			t.Errorf("%s => %d want %d body=%s", tc.name, recorder.Code, tc.want, recorder.Body.String())
		}
	}
	// deleteCases 描述删除错误映射。
	deleteCases := []struct {
		err  error
		want int
	}{{automationapp.ErrRuleNotFound, 404}, {automationapp.ErrRuleActive, 409}, {errors.New("db"), 500}}
	// tc 表示当前删除场景。
	for _, tc := range deleteCases {
		port.deleteErr = tc.err
		// recorder 是调用 "DELETE" "/automation-rules/1" 得到的响应记录，其状态码用于断言该场景的错误映射
		if recorder := serveChatCoverageRequest(handler, cookie, "DELETE", "/automation-rules/1", ""); recorder.Code != tc.want {
			t.Errorf("delete %v => %d want %d", tc.err, recorder.Code, tc.want)
		}
	}
}

// notifyGapPort 是通知渠道端口桩，所有方法返回同一个预设错误。
type notifyGapPort struct {
	// NotificationChannelsPort 提供未覆盖方法的默认（空）实现。
	NotificationChannelsPort
	err error
}

// GetChannelEditor 返回预设错误。
func (p notifyGapPort) GetChannelEditor(context.Context, int64, int64) (notificationsapp.ChannelEditor, error) {
	return notificationsapp.ChannelEditor{}, p.err
}

// UpdateChannel 返回预设错误。
func (p notifyGapPort) UpdateChannel(context.Context, int64, int64, notificationsapp.ChannelPatch) error {
	return p.err
}

// DeleteChannel 返回预设错误。
func (p notifyGapPort) DeleteChannel(context.Context, int64, int64) error { return p.err }

// SetBindings 返回预设错误。
func (p notifyGapPort) SetBindings(context.Context, int64, string, []int64) error { return p.err }

// SetSingleBinding 返回预设错误。
func (p notifyGapPort) SetSingleBinding(context.Context, int64, string, int64, bool) error {
	return p.err
}

// DeleteBinding 返回预设错误。
func (p notifyGapPort) DeleteBinding(context.Context, int64, int64) error { return p.err }

// DeleteAccountBindings 返回预设错误。
func (p notifyGapPort) DeleteAccountBindings(context.Context, int64, string) error { return p.err }

// TestNotificationHandlerRound2 覆盖通知渠道与绑定的无效 ID 和各类错误映射。
func TestNotificationHandlerRound2(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	handler := srv.Router()
	cookie := loginHelper(t, handler)
	// recorder 是调用 "GET" "/notification-channels/abc" 得到的响应记录，其状态码用于断言该场景的错误映射
	if recorder := serveChatCoverageRequest(handler, cookie, "GET", "/notification-channels/abc", ""); recorder.Code != http.StatusBadRequest {
		t.Errorf("bad editor id => %d", recorder.Code)
	}

	// routes 描述每个接口及其触发的端口错误映射期望。
	routes := []struct {
		// method、path、body 描述请求。
		method, path, body string
		// err 是注入端口错误。
		err  error
		want int
	}{
		{"GET", "/notification-channels/1", ``, notificationsapp.ErrChannelNotFound, 403},
		{"GET", "/notification-channels/1", ``, errors.New("db"), 500},
		{"PUT", "/notification-channels/1", `{}`, errors.New("db"), 500},
		{"DELETE", "/notification-channels/1", ``, errors.New("db"), 500},
		{"DELETE", "/message-notifications/1", ``, errors.New("db"), 500},
		{"DELETE", "/message-notifications/account/acc1", ``, errors.New("db"), 500},
		{"POST", "/message-notifications/acc1", `{"channel_id":1}`, notificationsapp.ErrAccountForbidden, 404},
		{"POST", "/message-notifications/acc1", `{"channel_id":1}`, notificationsapp.ErrChannelForbidden, 403},
		{"POST", "/message-notifications/acc1", `{"channel_id":1}`, errors.New("db"), 500},
		{"POST", "/message-notifications/acc1", `{"channel_ids":[1]}`, notificationsapp.ErrChannelNotFound, 403},
		{"POST", "/message-notifications/acc1", `{"channel_ids":[1]}`, errors.New("db"), 500},
	}
	// tc 表示当前通知错误场景。
	for _, tc := range routes {
		srv.applications.notificationChannels = notifyGapPort{err: tc.err}
		// recorder 是调用 tc.method tc.path 得到的响应记录，其状态码用于断言该场景的错误映射
		if recorder := serveChatCoverageRequest(handler, cookie, tc.method, tc.path, tc.body); recorder.Code != tc.want {
			t.Errorf("%s %s %v => %d want %d", tc.method, tc.path, tc.err, recorder.Code, tc.want)
		}
	}
}

// TestCardHandlerRound2 覆盖卡券创建、更新、删除的 500/校验映射与 API 配置解码错误。
func TestCardHandlerRound2(t *testing.T) {
	srv, _, cleanup := newTestServer(t)
	defer cleanup()
	handler := srv.Router()
	cookie := loginHelper(t, handler)
	// real 是真实卡券端口。
	real := srv.applications.cards
	// body 是合法卡券请求体。
	const body = `{"name":"n","type":"text","text_content":"x"}`

	// cases 描述卡券端口错误与各接口期望状态。
	cases := []struct {
		// method、path、body 描述请求。
		method, path, body string
		// err 是注入端口错误。
		err  error
		want int
	}{
		{"POST", "/cards", body, errors.New("db"), 500},
		{"POST", "/cards", body, &cardsapp.ValidationError{}, 400},
		{"POST", "/cards", `{`, nil, 400},
		{"POST", "/cards", `{"api_config":5}`, nil, 400},
		{"PUT", "/cards/1", `{`, nil, 400},
		{"PUT", "/cards/1", body, &cardsapp.ValidationError{}, 400},
		{"PUT", "/cards/1", body, errors.New("db"), 500},
		{"DELETE", "/cards/1", ``, errors.New("db"), 500},
	}
	// tc 表示当前卡券场景。
	for _, tc := range cases {
		srv.applications.cards = cardMutationGapPort{CardsPort: real, err: tc.err}
		// recorder 是调用 tc.method tc.path 得到的响应记录，其状态码用于断言该场景的错误映射
		if recorder := serveChatCoverageRequest(handler, cookie, tc.method, tc.path, tc.body); recorder.Code != tc.want {
			t.Errorf("%s %s %v => %d want %d body=%s", tc.method, tc.path, tc.err, recorder.Code, tc.want, recorder.Body.String())
		}
	}
	// 非法的历史字符串和对象配置只能通过直接调用解码函数触达。
	if _, err := decodeCardAPIConfig(json.RawMessage(`"abc`)); err == nil {
		t.Error("broken legacy string should fail")
	}
	// err 是API 配置解码错误；畸形对象必须报错，合法对象必须为 nil
	if _, err := decodeCardAPIConfig(json.RawMessage(`{`)); err == nil {
		t.Error("broken object should fail")
	}
	// err 是API 配置解码错误；畸形对象必须报错，合法对象必须为 nil；got 是被测函数返回的实际值，用于与期望比较
	if got, err := decodeCardAPIConfig(json.RawMessage(`{"url":"u"}`)); err != nil || !strings.Contains(got, "url") {
		t.Errorf("object config = %q %v", got, err)
	}
}

// cardMutationGapPort 包装卡券端口，让创建、更新、删除返回预设错误。
type cardMutationGapPort struct {
	// CardsPort 提供其余卡券能力的真实实现。
	CardsPort
	err error
}

// Create 返回预设错误。
func (p cardMutationGapPort) Create(ctx context.Context, userID int64, draft cardsapp.Draft) (int64, error) {
	if p.err == nil {
		return p.CardsPort.Create(ctx, userID, draft)
	}
	return 0, p.err
}

// Update 返回预设错误。
func (p cardMutationGapPort) Update(ctx context.Context, userID, cardID int64, draft cardsapp.Draft) error {
	if p.err == nil {
		return p.CardsPort.Update(ctx, userID, cardID, draft)
	}
	return p.err
}

// Delete 返回预设错误。
func (p cardMutationGapPort) Delete(ctx context.Context, userID, cardID int64) error {
	if p.err == nil {
		return p.CardsPort.Delete(ctx, userID, cardID)
	}
	return p.err
}
