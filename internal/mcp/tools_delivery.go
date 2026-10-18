package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"xianyu-go/internal/orderspec"
)

// ruleSummary 生成规则摘要，供列表、绑定和启停结果展示。
func ruleSummary(rule ruleRow) map[string]any {
	actions := []map[string]any{}
	for _, action := range rule.Actions {
		if !action.isDelivery() {
			continue
		}
		specName, specValue, _ := action.actionSpec()
		entry := map[string]any{"action_id": action.ID, "type": action.ActionType, "enabled": action.Enabled, "delivery_count": action.DeliveryCount,
			"spec_name": specName, "spec_value": specValue}
		if action.ActionType == actionSendCard {
			entry["card_id"], entry["card_name"] = action.CardID, action.CardName
		} else {
			entry["template_id"], entry["template_name"] = action.DeliveryTemplateID, action.DeliveryTemplateName
		}
		actions = append(actions, entry)
	}
	return map[string]any{"rule_id": rule.ID, "account_id": rule.CookieID, "item_id": rule.ItemID, "item_title": rule.ItemTitle, "name": rule.Name,
		"scope": rule.scope(), "enabled": rule.Enabled, "priority": rule.Priority, "ready": rule.SKUMigrationStatus == "ready", "deliveries": actions}
}

func runListDeliveryRules(client apiClient, arguments json.RawMessage) (any, error) {
	var args struct {
		AccountID string `json:"account_id"`
		ItemID    string `json:"item_id"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	rules, err := client.rules()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, rule := range rules {
		if rule.TriggerType != triggerOrderPaid {
			continue
		}
		if args.AccountID != "" && rule.CookieID != strings.TrimSpace(args.AccountID) {
			continue
		}
		if args.ItemID != "" && rule.ItemID != strings.TrimSpace(args.ItemID) {
			continue
		}
		out = append(out, ruleSummary(rule))
	}
	return out, nil
}

// selectDeliveryRule 按自动化中心的匹配规则选出付款时会执行的规则：
// 商品级规则优先（启用、已就绪、优先级数值最小、ID 最小）；没有商品级规则时才使用确认适用全部商品的账号级规则。
func selectDeliveryRule(rules []ruleRow, accountID, itemID string) *ruleRow {
	pick := func(match func(ruleRow) bool) *ruleRow {
		candidates := []ruleRow{}
		for _, rule := range rules {
			if rule.TriggerType == triggerOrderPaid && rule.Enabled && rule.SKUMigrationStatus == "ready" && rule.CookieID == accountID && match(rule) {
				candidates = append(candidates, rule)
			}
		}
		if len(candidates) == 0 {
			return nil
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			if candidates[i].Priority != candidates[j].Priority {
				return candidates[i].Priority < candidates[j].Priority
			}
			return candidates[i].ID < candidates[j].ID
		})
		return &candidates[0]
	}
	if rule := pick(func(r ruleRow) bool { return r.ItemID == itemID }); rule != nil {
		return rule
	}
	return pick(func(r ruleRow) bool { return r.ItemID == "" && r.allowsAllItems() })
}

// actionMatchesSpec 与自动化中心一致：动作未设规格时只匹配无规格订单（账号级全商品规则除外），设了规格时要求完整相等；
// 规格配置损坏的动作不匹配任何订单。
func actionMatchesSpec(action actionRow, allowAllItems bool, specName, specValue string) bool {
	actionName, actionValue, ok := action.actionSpec()
	if !ok {
		return false
	}
	if actionName == "" && actionValue == "" {
		return allowAllItems || (strings.TrimSpace(specName) == "" && strings.TrimSpace(specValue) == "")
	}
	return orderspec.Equal(specName, specValue, actionName, actionValue)
}

// deliveryPreview 预演 accountID/itemID 在给定规格和数量下付款时会发送的内容。
func deliveryPreview(client apiClient, accountID, itemID, specName, specValue string, quantity int) (map[string]any, error) {
	if quantity <= 0 {
		quantity = 1
	}
	accounts, err := client.accounts()
	if err != nil {
		return nil, err
	}
	rules, err := client.rules()
	if err != nil {
		return nil, err
	}
	cards, err := client.cards()
	if err != nil {
		return nil, err
	}
	warnings := []string{}
	var account *accountRow
	for i := range accounts {
		if accounts[i].ID == accountID {
			account = &accounts[i]
		}
	}
	if account == nil {
		return nil, fmt.Errorf("账号 %s 不存在或不属于当前用户", accountID)
	}
	if !account.Enabled {
		warnings = append(warnings, "账号已停用，不会执行任何自动化")
	}
	if !account.AutoConfirm {
		warnings = append(warnings, "账号未开启“自动发货”开关，付款后不会自动发货")
	}
	result := map[string]any{"account_id": accountID, "item_id": itemID, "spec_name": specName, "spec_value": specValue, "quantity": quantity}
	rule := selectDeliveryRule(rules, accountID, itemID)
	if rule == nil {
		result["will_deliver"] = false
		result["warnings"] = append(warnings, "没有命中任何已启用的付款发货规则")
		return result, nil
	}
	result["rule"] = map[string]any{"rule_id": rule.ID, "name": rule.Name, "scope": rule.scope()}
	cardByID := map[int64]cardRow{}
	for _, card := range cards {
		cardByID[card.ID] = card
	}
	deliveries := []map[string]any{}
	for _, action := range rule.Actions {
		if !action.Enabled || !action.isDelivery() || !actionMatchesSpec(action, rule.allowsAllItems(), specName, specValue) {
			continue
		}
		perUnit := action.DeliveryCount
		if perUnit <= 0 {
			perUnit = 1
		}
		units := perUnit * quantity
		entry := map[string]any{"action_id": action.ID, "type": action.ActionType, "units": units}
		if action.ActionType == actionSendTemplate {
			entry["template_id"], entry["template_name"] = action.DeliveryTemplateID, action.DeliveryTemplateName
			deliveries = append(deliveries, entry)
			continue
		}
		card, ok := cardByID[action.CardID]
		if !ok {
			warnings = append(warnings, fmt.Sprintf("动作 %d 引用的卡密组 %d 不存在", action.ID, action.CardID))
			continue
		}
		summary := cardSummary(card)
		for key, value := range summary {
			entry[key] = value
		}
		if !card.Enabled {
			warnings = append(warnings, fmt.Sprintf("卡密组「%s」已停用，发货会失败", card.Name))
		}
		if card.Type == "data" {
			if stock := summary["stock"].(int); stock < units {
				warnings = append(warnings, fmt.Sprintf("卡密组「%s」库存 %d 条，不够本单需要的 %d 条", card.Name, stock, units))
			}
		}
		deliveries = append(deliveries, entry)
	}
	if len(deliveries) == 0 {
		warnings = append(warnings, "规则存在，但没有与该规格匹配的发货内容")
	}
	result["deliveries"] = deliveries
	result["will_deliver"] = len(deliveries) > 0 && account.Enabled && account.AutoConfirm
	result["warnings"] = warnings
	return result, nil
}

func runPreviewDelivery(client apiClient, arguments json.RawMessage) (any, error) {
	var args struct {
		AccountID string `json:"account_id"`
		ItemID    string `json:"item_id"`
		SpecName  string `json:"spec_name"`
		SpecValue string `json:"spec_value"`
		Quantity  int    `json:"quantity"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	accountID, err := requireText(args.AccountID, "account_id")
	if err != nil {
		return nil, err
	}
	itemID, err := requireText(args.ItemID, "item_id")
	if err != nil {
		return nil, err
	}
	return deliveryPreview(client, accountID, itemID, args.SpecName, args.SpecValue, args.Quantity)
}

// ruleRequest 是写回自动化规则接口的请求体，字段与 /api/v1/automation-rules 的请求 DTO 一致。
type ruleRequest struct {
	CookieID    string          `json:"cookie_id"`
	ItemID      string          `json:"item_id"`
	Name        string          `json:"name"`
	TriggerType string          `json:"trigger_type"`
	Enabled     bool            `json:"enabled"`
	Priority    int             `json:"priority"`
	ConfigJSON  string          `json:"config_json"`
	Actions     []actionRequest `json:"actions"`
}

// actionRequest 是规则动作的请求体；模板绑定使用请求侧的 key 字段名。
type actionRequest struct {
	ID                 int64                    `json:"id,omitempty"`
	ActionType         string                   `json:"action_type"`
	CardID             int64                    `json:"card_id"`
	DeliveryCount      int                      `json:"delivery_count"`
	MessageTemplate    string                   `json:"message_template"`
	DelaySeconds       int                      `json:"delay_seconds"`
	ConfigJSON         string                   `json:"config_json"`
	Enabled            bool                     `json:"enabled"`
	SortOrder          int                      `json:"sort_order"`
	DeliveryTemplateID int64                    `json:"delivery_template_id"`
	TemplateBindings   []templateBindingRequest `json:"template_bindings"`
	CustomVariables    map[string]string        `json:"custom_variables"`
}

// templateBindingRequest 是模板变量绑定的请求体。
type templateBindingRequest struct {
	Key           string `json:"key"`
	CardID        int64  `json:"card_id"`
	DeliveryCount int    `json:"delivery_count"`
}

// requestFromRule 把规则响应原样转换为更新请求，保留全部现有动作。
func requestFromRule(rule ruleRow) ruleRequest {
	request := ruleRequest{CookieID: rule.CookieID, ItemID: rule.ItemID, Name: rule.Name, TriggerType: rule.TriggerType,
		Enabled: rule.Enabled, Priority: rule.Priority, ConfigJSON: rule.ConfigJSON, Actions: []actionRequest{}}
	for _, action := range rule.Actions {
		bindings := []templateBindingRequest{}
		for _, binding := range action.TemplateBindings {
			bindings = append(bindings, templateBindingRequest{Key: binding.VariableKey, CardID: binding.CardID, DeliveryCount: binding.DeliveryCount})
		}
		request.Actions = append(request.Actions, actionRequest{ID: action.ID, ActionType: action.ActionType, CardID: action.CardID,
			DeliveryCount: action.DeliveryCount, MessageTemplate: action.MessageTemplate, DelaySeconds: action.DelaySeconds,
			ConfigJSON: action.ConfigJSON, Enabled: action.Enabled, SortOrder: action.SortOrder, DeliveryTemplateID: action.DeliveryTemplateID,
			TemplateBindings: bindings, CustomVariables: action.CustomVariables})
	}
	return request
}

// bindArguments 是 bind_item_delivery 的参数；publish_item 发布后绑定也复用它。
type bindArguments struct {
	AccountID     string `json:"account_id"`
	ItemID        string `json:"item_id"`
	CardID        int64  `json:"card_id"`
	SpecName      string `json:"spec_name"`
	SpecValue     string `json:"spec_value"`
	DeliveryCount int    `json:"delivery_count"`
}

func runBindItemDelivery(client apiClient, arguments json.RawMessage) (any, error) {
	var args bindArguments
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	return bindItemDelivery(client, args)
}

// bindItemDelivery 把商品（可指定规格）绑定到卡密组：没有付款发货规则时新建，已有时替换同规格动作或追加，返回绑定后的预演。
func bindItemDelivery(client apiClient, args bindArguments) (map[string]any, error) {
	accountID, err := requireText(args.AccountID, "account_id")
	if err != nil {
		return nil, err
	}
	itemID, err := requireText(args.ItemID, "item_id")
	if err != nil {
		return nil, err
	}
	specName, specValue := strings.TrimSpace(args.SpecName), strings.TrimSpace(args.SpecValue)
	if (specName == "") != (specValue == "") {
		return nil, errors.New("spec_name 和 spec_value 要么都填，要么都留空")
	}
	if specName != "" {
		normalized, specErr := orderspec.NormalizeColumns(specName, specValue)
		if specErr != nil {
			return nil, fmt.Errorf("规格格式错误：%v", specErr)
		}
		specName, specValue = normalized.Name, normalized.Value
	}
	if args.DeliveryCount <= 0 {
		args.DeliveryCount = 1
	}
	if args.CardID <= 0 {
		return nil, errors.New("缺少参数 card_id")
	}
	// 后端会清掉非多规格商品动作上的规格，因此先按商品事实校验，避免追加出重复的无规格动作。
	items, err := client.items(accountID)
	if err != nil {
		return nil, err
	}
	var item *itemRow
	for i := range items {
		if items[i].ItemID == itemID {
			item = &items[i]
		}
	}
	if item == nil {
		return nil, fmt.Errorf("本地商品列表中没有商品 %s，请先调用 sync_items 同步该账号商品", itemID)
	}
	if item.IsMultiSpec && specName == "" {
		return nil, errors.New("这是多规格商品，必须按规格分别绑定（填写 spec_name 和 spec_value）")
	}
	if !item.IsMultiSpec && specName != "" {
		return nil, errors.New("这不是多规格商品，绑定时不要填写规格")
	}
	rules, err := client.rules()
	if err != nil {
		return nil, err
	}
	existing := []ruleRow{}
	for _, rule := range rules {
		if rule.TriggerType == triggerOrderPaid && rule.CookieID == accountID && rule.ItemID == itemID {
			existing = append(existing, rule)
		}
	}
	if len(existing) > 1 {
		ids := []string{}
		for _, rule := range existing {
			ids = append(ids, strconv.FormatInt(rule.ID, 10))
		}
		return nil, fmt.Errorf("该商品已有多条付款发货规则（ID %s），付款时只会执行优先级最高的一条；请先在管理后台合并后再绑定", strings.Join(ids, "、"))
	}
	config, _ := json.Marshal(map[string]string{"spec_name": specName, "spec_value": specValue})
	operation := "created"
	if len(existing) == 0 {
		request := ruleRequest{CookieID: accountID, ItemID: itemID, Name: "自动发货 " + itemID, TriggerType: triggerOrderPaid, Enabled: true, Priority: 100, ConfigJSON: "{}",
			Actions: []actionRequest{{ActionType: actionSendCard, CardID: args.CardID, DeliveryCount: args.DeliveryCount, ConfigJSON: string(config), Enabled: true, SortOrder: 1,
				TemplateBindings: []templateBindingRequest{}, CustomVariables: map[string]string{}}}}
		if err := client.do(http.MethodPost, "/api/v1/automation-rules", nil, request, nil); err != nil {
			return nil, err
		}
	} else {
		rule := existing[0]
		request := requestFromRule(rule)
		request.Enabled = true
		replaced := false
		maxSort := 0
		for i, action := range rule.Actions {
			if action.SortOrder > maxSort {
				maxSort = action.SortOrder
			}
			if replaced || !action.isDelivery() || !actionMatchesSpec(action, false, specName, specValue) {
				continue
			}
			request.Actions[i].ActionType = actionSendCard
			request.Actions[i].CardID = args.CardID
			request.Actions[i].DeliveryCount = args.DeliveryCount
			request.Actions[i].DeliveryTemplateID = 0
			request.Actions[i].TemplateBindings = []templateBindingRequest{}
			request.Actions[i].Enabled = true
			replaced = true
		}
		operation = "replaced"
		if !replaced {
			operation = "appended"
			request.Actions = append(request.Actions, actionRequest{ActionType: actionSendCard, CardID: args.CardID, DeliveryCount: args.DeliveryCount,
				ConfigJSON: string(config), Enabled: true, SortOrder: maxSort + 1, TemplateBindings: []templateBindingRequest{}, CustomVariables: map[string]string{}})
		}
		if err := client.do(http.MethodPut, rule.path(), nil, request, nil); err != nil {
			return nil, err
		}
	}
	result, err := deliveryPreview(client, accountID, itemID, specName, specValue, 1)
	if err != nil {
		return nil, err
	}
	result["operation"] = operation
	return result, nil
}

func runSetDeliveryRuleEnabled(client apiClient, arguments json.RawMessage) (any, error) {
	var args struct {
		RuleID  int64 `json:"rule_id"`
		Enabled *bool `json:"enabled"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	if args.Enabled == nil {
		return nil, errors.New("缺少参数 enabled")
	}
	rules, err := client.rules()
	if err != nil {
		return nil, err
	}
	for _, rule := range rules {
		if rule.ID != args.RuleID {
			continue
		}
		request := requestFromRule(rule)
		request.Enabled = *args.Enabled
		if err := client.do(http.MethodPut, rule.path(), nil, request, nil); err != nil {
			return nil, err
		}
		rule.Enabled = *args.Enabled
		return ruleSummary(rule), nil
	}
	return nil, fmt.Errorf("规则 %d 不存在或不属于当前用户", args.RuleID)
}
