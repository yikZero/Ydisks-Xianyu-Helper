package mcp

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// serverInstructions 在 initialize 时告诉模型这些工具的用途和安全约定。
const serverInstructions = `Ydisks 闲鱼助手管理工具。可以查看闲鱼账号、商品、订单，维护卡密库存，并配置“付款后自动发货”规则。
约定：
1. 修改发货规则前先用 preview_delivery 预演；绑定后再预演一次，把结果告诉用户确认。
2. 商品必须先存在于本地商品列表；新上架的商品先调用 sync_items 同步。
3. 多规格商品要按规格分别绑定（spec_name/spec_value 与订单规格完全一致，多个维度用中文分号；分隔）。
4. 本工具集不提供删除操作；需要删除时请让用户在管理后台操作。`

// toolDefinition 描述一个 MCP 工具：write 为 true 的工具需要可写 Token。
type toolDefinition struct {
	name        string
	description string
	schema      map[string]any
	write       bool
	run         func(client apiClient, arguments json.RawMessage) (any, error)
}

// tools 按展示顺序列出全部工具。
var tools = []toolDefinition{
	{
		name:        "list_accounts",
		description: "列出已登录的闲鱼账号，以及每个账号的自动发货（auto_delivery）、自动确认发货、自动免拼开关。",
		schema:      objectSchema(nil),
		run:         runListAccounts,
	},
	{
		name:        "list_items",
		description: "列出本地已同步的商品（商品 ID、标题、价格、是否多规格）。可按账号和标题关键词过滤。",
		schema: objectSchema(map[string]any{
			"account_id": stringProp("闲鱼账号 ID，留空表示全部账号"),
			"keyword":    stringProp("按商品标题包含的关键词过滤"),
			"limit":      intProp("最多返回条数，默认 100"),
		}),
		run: runListItems,
	},
	{
		name:        "sync_items",
		description: "从闲鱼重新同步某个账号的全部在售商品到本地。新上架商品绑定发货前需要先同步。",
		schema:      objectSchema(map[string]any{"account_id": stringProp("闲鱼账号 ID")}, "account_id"),
		write:       true,
		run:         runSyncItems,
	},
	{
		name:        "list_cards",
		description: "列出卡密组（发货内容）：类型（text 固定文本 / data 逐条库存 / image 图片 / api 接口取货）、是否启用、data 类型的剩余库存条数和文本预览。",
		schema:      objectSchema(nil),
		run:         runListCards,
	},
	{
		name:        "get_card",
		description: "查看一个卡密组的完整内容；data 类型最多返回前 200 条库存。",
		schema:      objectSchema(map[string]any{"card_id": intProp("卡密组 ID")}, "card_id"),
		run:         runGetCard,
	},
	{
		name:        "create_card",
		description: "新建卡密组。text：每单发送同一段文本（例如网盘链接+提取码）；data：每单消耗一条库存；image：发送图片。",
		schema: objectSchema(map[string]any{
			"name":          stringProp("卡密组名称"),
			"type":          map[string]any{"type": "string", "enum": []string{"text", "data", "image"}, "description": "卡密类型"},
			"text_content":  stringProp("type=text 时发送的文本"),
			"stock_lines":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "type=data 时的初始库存，每个元素一条"},
			"image_url":     stringProp("type=image 时发送的图片地址"),
			"description":   stringProp("备注"),
			"delay_seconds": intProp("付款后延迟多少秒发送，默认 0"),
		}, "name", "type"),
		write: true,
		run:   runCreateCard,
	},
	{
		name:        "append_card_stock",
		description: "给 data 类型卡密组追加库存，每个元素一条。",
		schema: objectSchema(map[string]any{
			"card_id": intProp("卡密组 ID"),
			"lines":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "要追加的库存，每个元素一条"},
		}, "card_id", "lines"),
		write: true,
		run:   runAppendCardStock,
	},
	{
		name:        "list_delivery_rules",
		description: "列出“付款后自动发货”规则：每条规则绑定的商品、规格和发货卡密组。",
		schema: objectSchema(map[string]any{
			"account_id": stringProp("按闲鱼账号 ID 过滤"),
			"item_id":    stringProp("按商品 ID 过滤"),
		}),
		run: runListDeliveryRules,
	},
	{
		name:        "preview_delivery",
		description: "预演：如果某商品（某规格）现在被付款，会命中哪条规则、发出什么内容、库存是否够。不会真的发货。",
		schema: objectSchema(map[string]any{
			"account_id": stringProp("闲鱼账号 ID"),
			"item_id":    stringProp("商品 ID"),
			"spec_name":  stringProp("订单规格名称，多维度用；分隔，单规格商品留空"),
			"spec_value": stringProp("订单规格值，与 spec_name 一一对应"),
			"quantity":   intProp("购买数量，默认 1"),
		}, "account_id", "item_id"),
		run: runPreviewDelivery,
	},
	{
		name: "bind_item_delivery",
		description: "把商品（可指定规格）绑定到卡密组：买家付款后自动发送该卡密组内容。商品没有发货规则时新建；" +
			"已有规则时替换同规格的发货内容或追加新规格。绑定后返回预演结果。",
		schema: objectSchema(map[string]any{
			"account_id":     stringProp("闲鱼账号 ID"),
			"item_id":        stringProp("商品 ID"),
			"card_id":        intProp("卡密组 ID"),
			"spec_name":      stringProp("规格名称，单规格商品留空"),
			"spec_value":     stringProp("规格值，单规格商品留空"),
			"delivery_count": intProp("每件商品发送几份，默认 1"),
		}, "account_id", "item_id", "card_id"),
		write: true,
		run:   runBindItemDelivery,
	},
	{
		name:        "set_delivery_rule_enabled",
		description: "启用或停用一条自动发货规则。",
		schema: objectSchema(map[string]any{
			"rule_id": intProp("规则 ID"),
			"enabled": map[string]any{"type": "boolean", "description": "true 启用，false 停用"},
		}, "rule_id", "enabled"),
		write: true,
		run:   runSetDeliveryRuleEnabled,
	},
	{
		name:        "list_orders",
		description: "查询订单（不含收货人地址电话）。",
		schema: objectSchema(map[string]any{
			"account_id": stringProp("按闲鱼账号 ID 过滤"),
			"status":     stringProp("按订单状态过滤，例如 pending_ship、shipped、completed"),
			"search":     stringProp("按订单号、商品标题等搜索"),
			"page":       intProp("页码，默认 1"),
			"page_size":  intProp("每页条数，默认 20，最大 100"),
		}),
		run: runListOrders,
	},
}

// toolsByName 按名称索引工具定义。
var toolsByName = func() map[string]toolDefinition {
	index := make(map[string]toolDefinition, len(tools))
	for _, definition := range tools {
		index[definition.name] = definition
	}
	return index
}()

// toolDescriptors 返回 tools/list 的工具描述；只读 Token 看不到写工具。
func toolDescriptors(write bool) []map[string]any {
	descriptors := make([]map[string]any, 0, len(tools))
	for _, definition := range tools {
		if definition.write && !write {
			continue
		}
		descriptors = append(descriptors, map[string]any{
			"name":        definition.name,
			"description": definition.description,
			"inputSchema": definition.schema,
			"annotations": map[string]any{"readOnlyHint": !definition.write, "destructiveHint": false},
		})
	}
	return descriptors
}

// objectSchema 构造 JSON Schema 对象类型。
func objectSchema(properties map[string]any, required ...string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// stringProp 构造字符串参数。
func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// intProp 构造整数参数。
func intProp(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

// decodeArguments 解析工具参数；类型不符时返回可读错误。
func decodeArguments(arguments json.RawMessage, out any) error {
	if err := json.Unmarshal(arguments, out); err != nil {
		return fmt.Errorf("参数格式错误：%v", err)
	}
	return nil
}

// requireText 校验必填字符串参数。
func requireText(value, name string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("缺少参数 %s", name)
	}
	return value, nil
}

const (
	triggerOrderPaid   = "order_paid"
	actionSendCard     = "send_card"
	actionSendTemplate = "send_template"
)

// accountRow 是账号详情接口中工具需要的非敏感字段。
type accountRow struct {
	ID          string `json:"id"`
	Nickname    string `json:"nickname"`
	Enabled     bool   `json:"enabled"`
	Paused      bool   `json:"paused"`
	HasCookie   bool   `json:"has_cookie"`
	AutoConfirm bool   `json:"auto_confirm"`
	AutoConsign bool   `json:"auto_consign"`
	AutoBargain bool   `json:"auto_bargain"`
}

// itemRow 是商品列表接口中工具需要的字段。
type itemRow struct {
	CookieID    string `json:"cookie_id"`
	ItemID      string `json:"item_id"`
	ItemTitle   string `json:"item_title"`
	ItemPrice   any    `json:"item_price"`
	IsMultiSpec bool   `json:"is_multi_spec"`
}

// cardRow 是卡密组接口返回的字段。
type cardRow struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	TextContent  string `json:"text_content"`
	DataContent  string `json:"data_content"`
	ImageURL     string `json:"image_url"`
	Description  string `json:"description"`
	Enabled      bool   `json:"enabled"`
	DelaySeconds int    `json:"delay_seconds"`
}

// templateBindingRow 是模板发货动作的变量绑定。
type templateBindingRow struct {
	VariableKey   string `json:"variable_key"`
	CardID        int64  `json:"card_id"`
	DeliveryCount int    `json:"delivery_count"`
}

// actionRow 是规则动作接口返回的字段。
type actionRow struct {
	ID                   int64                `json:"id"`
	ActionType           string               `json:"action_type"`
	CardID               int64                `json:"card_id"`
	CardName             string               `json:"card_name"`
	DeliveryCount        int                  `json:"delivery_count"`
	MessageTemplate      string               `json:"message_template"`
	DelaySeconds         int                  `json:"delay_seconds"`
	ConfigJSON           string               `json:"config_json"`
	Enabled              bool                 `json:"enabled"`
	SortOrder            int                  `json:"sort_order"`
	DeliveryTemplateID   int64                `json:"delivery_template_id"`
	DeliveryTemplateName string               `json:"delivery_template_name"`
	TemplateBindings     []templateBindingRow `json:"template_bindings"`
	CustomVariables      map[string]string    `json:"custom_variables"`
}

// ruleRow 是自动化规则接口返回的字段。
type ruleRow struct {
	ID                 int64       `json:"id"`
	CookieID           string      `json:"cookie_id"`
	ItemID             string      `json:"item_id"`
	ItemTitle          string      `json:"item_title"`
	Name               string      `json:"name"`
	TriggerType        string      `json:"trigger_type"`
	Enabled            bool        `json:"enabled"`
	Priority           int         `json:"priority"`
	ConfigJSON         string      `json:"config_json"`
	SKUMigrationStatus string      `json:"sku_migration_status"`
	Actions            []actionRow `json:"actions"`
}

// actionSpec 读取动作配置里的规格过滤条件；配置不是合法 JSON 时 ok 为 false。
func (a actionRow) actionSpec() (name, value string, ok bool) {
	var config struct {
		SpecName  string `json:"spec_name"`
		SpecValue string `json:"spec_value"`
	}
	if json.Unmarshal([]byte(a.ConfigJSON), &config) != nil {
		return "", "", false
	}
	return strings.TrimSpace(config.SpecName), strings.TrimSpace(config.SpecValue), true
}

// isDelivery 判断动作是否为发货动作（发卡密或发模板）。
func (a actionRow) isDelivery() bool {
	return a.ActionType == actionSendCard || a.ActionType == actionSendTemplate
}

// allowsAllItems 判断账号级规则是否经用户确认适用于全部商品（与自动化中心的判定一致）。
func (r ruleRow) allowsAllItems() bool {
	if r.ItemID != "" {
		return false
	}
	var config struct {
		AllowAllItems bool `json:"allow_all_items"`
	}
	return json.Unmarshal([]byte(r.ConfigJSON), &config) == nil && config.AllowAllItems
}

// scope 返回规则作用范围：绑定商品的为 item，账号级为 account。
func (r ruleRow) scope() string {
	if r.ItemID == "" {
		return "account"
	}
	return "item"
}

// path 返回规则的管理接口地址。
func (r ruleRow) path() string {
	return "/api/v1/automation-rules/" + strconv.FormatInt(r.ID, 10)
}

// stockLines 按发货扣减库存时的规则（\n 或 \r 分隔、跳过空白行）返回 data 类型卡密的库存行，并去掉首尾空白。
func stockLines(content string) []string {
	lines := []string{}
	for _, line := range strings.FieldsFunc(content, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// preview 截断过长文本，避免一次返回大量内容。
func preview(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}

// cardSummary 生成卡密组摘要：data 类型给出库存条数，不返回全部库存。
func cardSummary(card cardRow) map[string]any {
	summary := map[string]any{"card_id": card.ID, "name": card.Name, "type": card.Type, "enabled": card.Enabled}
	switch card.Type {
	case "data":
		summary["stock"] = len(stockLines(card.DataContent))
	case "text":
		summary["text_preview"] = preview(card.TextContent, 80)
	case "image":
		summary["image_url"] = card.ImageURL
	}
	if card.DelaySeconds > 0 {
		summary["delay_seconds"] = card.DelaySeconds
	}
	if card.Description != "" {
		summary["description"] = card.Description
	}
	return summary
}
