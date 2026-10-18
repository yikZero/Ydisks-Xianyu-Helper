package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	automationapp "xianyu-go/internal/application/automation"
	"xianyu-go/internal/auth"
)

// automationTemplateBindingRequest 是模板变量绑定请求 DTO。
type automationTemplateBindingRequest struct {
	// Key 是模板变量键。
	Key string `json:"key"`
	// CardID 是绑定卡密组标识。
	CardID int64 `json:"card_id"`
	// DeliveryCount 是每件商品准备的卡密份数。
	DeliveryCount int `json:"delivery_count"`
}

// automationActionRequest 是自动化动作请求 DTO。
type automationActionRequest struct {
	// ID 是更新时对应的既有动作标识；创建时为零。
	ID              int64  `json:"id"`
	ActionType      string `json:"action_type"`
	CardID          int64  `json:"card_id"`
	DeliveryCount   int    `json:"delivery_count"`
	MessageTemplate string `json:"message_template"`
	DelaySeconds    int    `json:"delay_seconds"`
	ConfigJSON      string `json:"config_json"`
	Enabled         *bool  `json:"enabled"`
	SortOrder       int    `json:"sort_order"`
	// DeliveryTemplateID 是模板发货动作引用的模板标识。
	DeliveryTemplateID int64 `json:"delivery_template_id"`
	// TemplateBindings 是模板变量绑定列表。
	TemplateBindings []automationTemplateBindingRequest `json:"template_bindings"`
	// CustomVariables 是传给发货模板的自定义字符串键值表。
	CustomVariables map[string]string `json:"custom_variables"`
}

type automationRuleRequest struct {
	CookieID    string                    `json:"cookie_id"`
	ItemID      string                    `json:"item_id"`
	Name        string                    `json:"name"`
	TriggerType string                    `json:"trigger_type"`
	Enabled     bool                      `json:"enabled"`
	Priority    int                       `json:"priority"`
	ConfigJSON  string                    `json:"config_json"`
	Actions     []automationActionRequest `json:"actions"`
}

func (s *Server) listAutomationRules(w http.ResponseWriter, r *http.Request) {
	sess := auth.SessionFromContext(r.Context())
	query := r.URL.Query()
	_, paginated := query["page"]
	if !paginated {
		rules, err := s.automationRulesApplication().ListForUser(r.Context(), sess.UserID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "查询自动化规则失败")
			return
		}
		writeJSON(w, http.StatusOK, automationRulesJSON(rules))
		return
	}

	page := atoiDefault(query.Get("page"), 1)
	pageSize := atoiDefault(query.Get("page_size"), 10)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 10
	}
	if pageSize > 100 {
		pageSize = 100
	}
	cookieID := strings.TrimSpace(query.Get("cookie_id"))
	if cookieID != "" {
		if !s.cookieOwnedByUser(r.Context(), sess.UserID, cookieID) {
			writeErr(w, http.StatusForbidden, "无权限操作该账号")
			return
		}
	}
	triggerType := strings.TrimSpace(query.Get("trigger_type"))
	if triggerType != "" {
		switch triggerType {
		case automationapp.TriggerOrderCreated, automationapp.TriggerOrderPaid, automationapp.TriggerBuyerReviewed, automationapp.TriggerReviewMissingTimeout:
		default:
			writeErr(w, http.StatusBadRequest, "不支持的触发类型")
			return
		}
	}
	var enabled *bool
	if rawEnabled := strings.TrimSpace(query.Get("enabled")); rawEnabled != "" {
		value, parseErr := strconv.ParseBool(rawEnabled)
		if parseErr != nil {
			writeErr(w, http.StatusBadRequest, "启用状态必须是 true 或 false")
			return
		}
		enabled = &value
	}

	rules, total, err := s.automationRulesApplication().ListPageForUser(r.Context(), automationapp.RuleFilter{
		UserID: sess.UserID, CookieID: cookieID, TriggerType: triggerType, Enabled: enabled,
		Search: query.Get("search"), Limit: pageSize, Offset: (page - 1) * pageSize,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "查询自动化规则失败")
		return
	}
	filter := automationapp.RuleFilter{
		UserID: sess.UserID, CookieID: cookieID, TriggerType: triggerType, Enabled: enabled,
		Search: query.Get("search"),
	}
	triggerCounts, err := s.automationRulesApplication().CountByTriggerForUser(r.Context(), filter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "统计自动化规则失败")
		return
	}
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages > 0 && page > totalPages {
		page = totalPages
		rules, _, err = s.automationRulesApplication().ListPageForUser(r.Context(), automationapp.RuleFilter{
			UserID: sess.UserID, CookieID: cookieID, TriggerType: triggerType, Enabled: enabled,
			Search: query.Get("search"), Limit: pageSize, Offset: (page - 1) * pageSize,
		})
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "查询自动化规则失败")
			return
		}
	}
	writeJSON(w, http.StatusOK, automationRulePageResponse{
		Success: true, Data: automationRulesJSON(rules), Total: total, Page: page,
		PageSize: pageSize, TotalPages: totalPages, TriggerCounts: triggerCounts,
	})
}

func automationRulesJSON(rules []automationapp.Rule) []automationRuleResponse {
	out := make([]automationRuleResponse, 0, len(rules))
	for _, rule := range rules {
		actions := make([]automationActionResponse, 0, len(rule.Actions))
		for _, action := range rule.Actions {
			actions = append(actions, automationActionResponse{
				ID: action.ID, ActionType: action.ActionType, CardID: action.CardID, CardName: action.CardName,
				DeliveryCount: action.DeliveryCount, MessageTemplate: action.MessageTemplate,
				DelaySeconds: action.DelaySeconds, ConfigJSON: action.ConfigJSON, Enabled: action.Enabled,
				SortOrder: action.SortOrder, DeliveryTemplateID: action.DeliveryTemplateID,
				DeliveryTemplateName: action.DeliveryTemplateName, TemplateKeys: append([]string(nil), action.TemplateKeys...),
				TemplateBindings: automationTemplateBindingResponses(action.TemplateBindings), CustomVariables: copyAutomationCustomVariables(action.CustomVariables),
			})
		}
		out = append(out, automationRuleResponse{
			ID: rule.ID, CookieID: rule.CookieID, ItemID: rule.ItemID, ItemTitle: rule.ItemTitle,
			Name: rule.Name, TriggerType: rule.TriggerType, Enabled: rule.Enabled, Priority: rule.Priority,
			ConfigJSON: rule.ConfigJSON, SKUMigrationStatus: rule.SKUMigrationStatus, Actions: actions, CreatedAt: rule.CreatedAt, UpdatedAt: rule.UpdatedAt,
		})
	}
	return out
}

// automationTemplateBindingResponses 将应用层模板绑定转换为响应 DTO。
func automationTemplateBindingResponses(bindings []automationapp.TemplateBinding) []automationTemplateBindingResponse {
	// responses 保存模板绑定响应列表。
	responses := make([]automationTemplateBindingResponse, 0, len(bindings))
	for /* binding 表示当前待转换的应用模板绑定。 */ _, binding := range bindings {
		responses = append(responses, automationTemplateBindingResponse{VariableKey: binding.VariableKey, CardID: binding.CardID, CardName: binding.CardName, DeliveryCount: binding.DeliveryCount})
	}
	return responses
}

// createAutomationRule 创建自动化规则并返回数值主键 DTO。
func (s *Server) createAutomationRule(w http.ResponseWriter, r *http.Request) {
	sess := auth.SessionFromContext(r.Context())
	var req automationRuleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	in, err := s.automationRulesApplication().Normalize(r.Context(), sess.UserID, automationRuleDraft(req))
	if err != nil {
		if errors.Is(err, automationapp.ErrPricingModeConflict) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.automationRulesApplication().Create(r.Context(), in)
	if err != nil {
		if errors.Is(err, automationapp.ErrDeliveryTemplateUnavailable) {
			writeErr(w, http.StatusConflict, "发货模板状态已变化，请重新选择后保存")
			return
		}
		if errors.Is(err, automationapp.ErrPricingModeConflict) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "创建自动化规则失败")
		return
	}
	writeJSON(w, http.StatusOK, mutationIDResponse{Success: true, ID: id})
}

func (s *Server) updateAutomationRule(w http.ResponseWriter, r *http.Request) {
	ruleID, err := strconv.ParseInt(chi.URLParam(r, "rule_id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效规则ID")
		return
	}
	sess := auth.SessionFromContext(r.Context())
	var req automationRuleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	in, err := s.automationRulesApplication().NormalizeForUpdate(r.Context(), sess.UserID, ruleID, automationRuleDraft(req))
	if err != nil {
		if errors.Is(err, automationapp.ErrRuleNotFound) {
			writeErr(w, http.StatusNotFound, "自动化规则不存在")
			return
		}
		if errors.Is(err, automationapp.ErrPricingModeConflict) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.automationRulesApplication().Update(r.Context(), sess.UserID, ruleID, in); err != nil {
		if errors.Is(err, automationapp.ErrDeliveryTemplateUnavailable) {
			writeErr(w, http.StatusConflict, "发货模板状态已变化，请重新选择后保存")
			return
		}
		if errors.Is(err, automationapp.ErrPricingModeConflict) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, automationapp.ErrRuleNotFound) {
			writeErr(w, http.StatusNotFound, "自动化规则不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "更新自动化规则失败")
		return
	}
	writeJSON(w, http.StatusOK, operationResponse{Success: true})
}

func (s *Server) deleteAutomationRule(w http.ResponseWriter, r *http.Request) {
	ruleID, err := strconv.ParseInt(chi.URLParam(r, "rule_id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "无效规则ID")
		return
	}
	sess := auth.SessionFromContext(r.Context())
	if err := s.automationRulesApplication().Delete(r.Context(), sess.UserID, ruleID); err != nil {
		if errors.Is(err, automationapp.ErrRuleNotFound) {
			writeErr(w, http.StatusNotFound, "自动化规则不存在")
			return
		}
		if errors.Is(err, automationapp.ErrRuleActive) {
			writeErr(w, http.StatusConflict, "规则仍有运行中或待人工处理的任务，处理完成后才能删除")
			return
		}
		writeErr(w, http.StatusInternalServerError, "删除自动化规则失败")
		return
	}
	writeJSON(w, http.StatusOK, operationResponse{Success: true})
}

// automationRuleDraft 将 HTTP 请求转换为应用层规则草稿。
func automationRuleDraft(req automationRuleRequest) automationapp.RuleDraft {
	// actions 保存转换后的应用层动作草稿，不携带数据库模型。
	actions := make([]automationapp.ActionDraft, 0, len(req.Actions))
	// action 是当前待转换的 HTTP 动作请求。
	for _, action := range req.Actions {
		// bindings 保存当前动作的模板变量绑定。
		bindings := make([]automationapp.TemplateBinding, 0, len(action.TemplateBindings))
		for /* binding 表示请求中的模板变量绑定。 */ _, binding := range action.TemplateBindings {
			bindings = append(bindings, automationapp.TemplateBinding{VariableKey: strings.TrimSpace(binding.Key), CardID: binding.CardID, DeliveryCount: binding.DeliveryCount})
		}
		actions = append(actions, automationapp.ActionDraft{ID: action.ID, ActionType: action.ActionType, CardID: action.CardID,
			DeliveryCount: action.DeliveryCount, MessageTemplate: action.MessageTemplate, DelaySeconds: action.DelaySeconds,
			ConfigJSON: action.ConfigJSON, Enabled: action.Enabled, SortOrder: action.SortOrder,
			DeliveryTemplateID: action.DeliveryTemplateID, TemplateBindings: bindings, CustomVariables: copyAutomationCustomVariables(action.CustomVariables)})
	}
	return automationapp.RuleDraft{CookieID: req.CookieID, ItemID: req.ItemID, Name: req.Name, TriggerType: req.TriggerType,
		Enabled: req.Enabled, Priority: req.Priority, ConfigJSON: req.ConfigJSON, Actions: actions}
}

// copyAutomationCustomVariables 复制规则自定义变量，避免响应和应用模型共享可变 map。
func copyAutomationCustomVariables(values map[string]string) map[string]string {
	// copied 保存自定义变量键值表的独立副本。
	copied := make(map[string]string, len(values))
	for /* key 表示自定义变量键；value 表示自定义字符串。 */ key, value := range values {
		copied[key] = value
	}
	return copied
}
