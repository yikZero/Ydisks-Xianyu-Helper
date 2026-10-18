package mcp

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (c apiClient) accounts() ([]accountRow, error) {
	var accounts []accountRow
	return accounts, c.do(http.MethodGet, "/api/v1/accounts/details", nil, nil, &accounts)
}

func (c apiClient) cards() ([]cardRow, error) {
	var cards []cardRow
	return cards, c.do(http.MethodGet, "/api/v1/cards", nil, nil, &cards)
}

func (c apiClient) rules() ([]ruleRow, error) {
	var rules []ruleRow
	return rules, c.do(http.MethodGet, "/api/v1/automation-rules", nil, nil, &rules)
}

func (c apiClient) items(accountID string) ([]itemRow, error) {
	query := url.Values{}
	if accountID != "" {
		query.Set("cookie_id", accountID)
	}
	var items []itemRow
	return items, c.do(http.MethodGet, "/api/v1/items", query, nil, &items)
}

func runListAccounts(client apiClient, _ json.RawMessage) (any, error) {
	accounts, err := client.accounts()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, map[string]any{
			"account_id": account.ID, "nickname": account.Nickname, "enabled": account.Enabled, "paused": account.Paused,
			"logged_in": account.HasCookie, "auto_delivery": account.AutoConfirm, "auto_confirm_shipment": account.AutoConsign,
			"auto_bargain_free_shipping": account.AutoBargain,
		})
	}
	return out, nil
}

func runListItems(client apiClient, arguments json.RawMessage) (any, error) {
	var args struct {
		AccountID string `json:"account_id"`
		Keyword   string `json:"keyword"`
		Limit     int    `json:"limit"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	items, err := client.items(strings.TrimSpace(args.AccountID))
	if err != nil {
		return nil, err
	}
	if args.Limit <= 0 {
		args.Limit = 100
	}
	keyword := strings.ToLower(strings.TrimSpace(args.Keyword))
	out := []map[string]any{}
	total := 0
	for _, item := range items {
		if keyword != "" && !strings.Contains(strings.ToLower(item.ItemTitle), keyword) {
			continue
		}
		total++
		if len(out) < args.Limit {
			out = append(out, map[string]any{"account_id": item.CookieID, "item_id": item.ItemID, "title": item.ItemTitle, "price": item.ItemPrice, "is_multi_spec": item.IsMultiSpec})
		}
	}
	return map[string]any{"total": total, "items": out}, nil
}

func runSyncItems(client apiClient, arguments json.RawMessage) (any, error) {
	var args struct {
		AccountID string `json:"account_id"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	accountID, err := requireText(args.AccountID, "account_id")
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := client.do(http.MethodPost, "/api/v1/items/get-all-from-account", nil, map[string]any{"cookie_id": accountID}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func runListCards(client apiClient, _ json.RawMessage) (any, error) {
	cards, err := client.cards()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(cards))
	for _, card := range cards {
		out = append(out, cardSummary(card))
	}
	return out, nil
}

func runGetCard(client apiClient, arguments json.RawMessage) (any, error) {
	var args struct {
		CardID int64 `json:"card_id"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	var card cardRow
	if err := client.do(http.MethodGet, "/api/v1/cards/"+strconv.FormatInt(args.CardID, 10), nil, nil, &card); err != nil {
		return nil, err
	}
	out := cardSummary(card)
	switch card.Type {
	case "text":
		out["text_content"] = card.TextContent
	case "data":
		lines := stockLines(card.DataContent)
		if len(lines) > 200 {
			out["stock_truncated"] = true
			lines = lines[:200]
		}
		out["stock_lines"] = lines
	}
	return out, nil
}

func runCreateCard(client apiClient, arguments json.RawMessage) (any, error) {
	var args struct {
		Name         string   `json:"name"`
		Type         string   `json:"type"`
		TextContent  string   `json:"text_content"`
		StockLines   []string `json:"stock_lines"`
		ImageURL     string   `json:"image_url"`
		Description  string   `json:"description"`
		DelaySeconds int      `json:"delay_seconds"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	name, err := requireText(args.Name, "name")
	if err != nil {
		return nil, err
	}
	request := map[string]any{"name": name, "type": args.Type, "description": args.Description, "enabled": true, "delay_seconds": args.DelaySeconds}
	switch args.Type {
	case "text":
		if strings.TrimSpace(args.TextContent) == "" {
			return nil, errors.New("type=text 需要 text_content")
		}
		request["text_content"] = args.TextContent
	case "data":
		request["data_content"] = strings.Join(stockLines(strings.Join(args.StockLines, "\n")), "\n")
	case "image":
		if strings.TrimSpace(args.ImageURL) == "" {
			return nil, errors.New("type=image 需要 image_url")
		}
		request["image_url"] = strings.TrimSpace(args.ImageURL)
	default:
		return nil, errors.New("type 只能是 text、data 或 image")
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := client.do(http.MethodPost, "/api/v1/cards", nil, request, &created); err != nil {
		return nil, err
	}
	return map[string]any{"card_id": created.ID, "name": name, "type": args.Type}, nil
}

func runAppendCardStock(client apiClient, arguments json.RawMessage) (any, error) {
	var args struct {
		CardID int64    `json:"card_id"`
		Lines  []string `json:"lines"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	lines := stockLines(strings.Join(args.Lines, "\n"))
	if len(lines) == 0 {
		return nil, errors.New("lines 不能为空")
	}
	var result struct {
		Added int `json:"added"`
	}
	if err := client.do(http.MethodPost, "/api/v1/cards/"+strconv.FormatInt(args.CardID, 10)+"/append-data", nil, map[string]any{"content": strings.Join(lines, "\n")}, &result); err != nil {
		return nil, err
	}
	return map[string]any{"card_id": args.CardID, "added": result.Added}, nil
}

// orderFields 是返回给模型的订单字段白名单，不含收货人姓名、电话和地址。
var orderFields = []string{"order_id", "cookie_id", "item_id", "item_title", "buyer_id", "buyer_nickname", "quantity", "amount",
	"spec_name", "spec_value", "status", "order_status", "is_bargain", "created_at", "paid_at", "shipped_at", "completed_at"}

func runListOrders(client apiClient, arguments json.RawMessage) (any, error) {
	var args struct {
		AccountID string `json:"account_id"`
		Status    string `json:"status"`
		Search    string `json:"search"`
		Page      int    `json:"page"`
		PageSize  int    `json:"page_size"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	if args.Page <= 0 {
		args.Page = 1
	}
	if args.PageSize <= 0 {
		args.PageSize = 20
	}
	if args.PageSize > 100 {
		args.PageSize = 100
	}
	query := url.Values{"page": {strconv.Itoa(args.Page)}, "page_size": {strconv.Itoa(args.PageSize)}}
	for key, value := range map[string]string{"cookie_id": args.AccountID, "status": args.Status, "search": args.Search} {
		if strings.TrimSpace(value) != "" {
			query.Set(key, strings.TrimSpace(value))
		}
	}
	var page struct {
		Data  []map[string]any `json:"data"`
		Total int              `json:"total"`
		Page  int              `json:"page"`
	}
	if err := client.do(http.MethodGet, "/api/v1/orders", query, nil, &page); err != nil {
		return nil, err
	}
	orders := make([]map[string]any, 0, len(page.Data))
	for _, order := range page.Data {
		kept := map[string]any{}
		for _, field := range orderFields {
			if value, ok := order[field]; ok {
				kept[field] = value
			}
		}
		orders = append(orders, kept)
	}
	return map[string]any{"total": page.Total, "page": page.Page, "orders": orders}, nil
}
