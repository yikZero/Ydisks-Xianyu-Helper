package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"xianyu-go/internal/db"
)

// mcpPost 以 token 调用 /mcp，返回 HTTP 状态和解码后的 JSON-RPC 响应。
func mcpPost(t *testing.T, h http.Handler, token, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var decoded map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	return rec.Code, decoded
}

// mcpTool 调用工具并返回结果文本与 isError。
func mcpTool(t *testing.T, h http.Handler, token, name string, arguments any) (string, bool) {
	t.Helper()
	encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": arguments}})
	status, response := mcpPost(t, h, token, string(encoded))
	if status != http.StatusOK || response["error"] != nil {
		t.Fatalf("%s status=%d response=%v", name, status, response)
	}
	result := response["result"].(map[string]any)
	content := result["content"].([]any)[0].(map[string]any)
	return content["text"].(string), result["isError"] == true
}

// mcpToolJSON 调用工具并要求成功，返回解码后的结果。
func mcpToolJSON(t *testing.T, h http.Handler, token, name string, arguments any) map[string]any {
	t.Helper()
	text, isError := mcpTool(t, h, token, name, arguments)
	if isError {
		t.Fatalf("%s 失败: %s", name, text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s 结果不是对象: %s", name, text)
	}
	return out
}

// TestMCPEndpointAuthenticationAndProtocol 覆盖 Token 认证、协议协商、通知、批量、未知方法与只读 Token 的工具过滤。
func TestMCPEndpointAuthenticationAndProtocol(t *testing.T) {
	srv, store, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()
	h := srv.Router()
	writeToken, writeMeta, err := store.APITokens.Create(ctx, 1, "write", db.APITokenScopeWrite)
	if err != nil {
		t.Fatal(err)
	}
	readToken, _, err := store.APITokens.Create(ctx, 1, "read", db.APITokenScopeRead)
	if err != nil {
		t.Fatal(err)
	}

	if status, _ := mcpPost(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); status != http.StatusUnauthorized {
		t.Fatalf("无 Token 应 401，got %d", status)
	}
	if status, _ := mcpPost(t, h, "ydx_invalid", `{"jsonrpc":"2.0","id":1,"method":"ping"}`); status != http.StatusUnauthorized {
		t.Fatalf("无效 Token 应 401，got %d", status)
	}
	getReq := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应 405，got %d", getRec.Code)
	}

	_, initialized := mcpPost(t, h, writeToken, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	result := initialized["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" || result["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatalf("initialize=%v", initialized)
	}
	if status, _ := mcpPost(t, h, writeToken, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); status != http.StatusAccepted {
		t.Fatalf("通知应返回 202，got %d", status)
	}

	countTools := func(token string) int {
		_, listed := mcpPost(t, h, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		return len(listed["result"].(map[string]any)["tools"].([]any))
	}
	if writeCount, readCount := countTools(writeToken), countTools(readToken); readCount >= writeCount || readCount == 0 {
		t.Fatalf("只读 Token 不应看到写工具: write=%d read=%d", writeCount, readCount)
	}
	if text, isError := mcpTool(t, h, readToken, "create_card", map[string]any{"name": "x", "type": "text", "text_content": "y"}); !isError || !strings.Contains(text, "只读") {
		t.Fatalf("只读 Token 调用写工具应失败: %s", text)
	}
	if _, response := mcpPost(t, h, writeToken, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"drop_database"}}`); response["error"].(map[string]any)["code"].(float64) != -32602 {
		t.Fatalf("未知工具应返回 -32602: %v", response)
	}
	if _, response := mcpPost(t, h, writeToken, `{"jsonrpc":"2.0","id":4,"method":"resources/list"}`); response["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("未知方法应返回 -32601: %v", response)
	}
	batchReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`[{"jsonrpc":"2.0","id":5,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"}]`))
	batchReq.Header.Set("Authorization", "Bearer "+writeToken)
	batchRec := httptest.NewRecorder()
	h.ServeHTTP(batchRec, batchReq)
	var batch []map[string]any
	if err := json.Unmarshal(batchRec.Body.Bytes(), &batch); err != nil || len(batch) != 1 {
		t.Fatalf("批量请求只应返回非通知的响应: %s", batchRec.Body.String())
	}

	if err := store.APITokens.Revoke(ctx, 1, writeMeta.ID); err != nil {
		t.Fatal(err)
	}
	if status, _ := mcpPost(t, h, writeToken, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); status != http.StatusUnauthorized {
		t.Fatalf("吊销后应 401，got %d", status)
	}
}

// TestMCPDeliveryWorkflow 覆盖建卡密、补库存、绑定商品（新建/追加规格/替换）、预演、启停规则，
// 并确认绑定结果被真实的自动化匹配逻辑选中。
func TestMCPDeliveryWorkflow(t *testing.T) {
	srv, store, cleanup := newTestServer(t)
	defer cleanup()
	ctx := context.Background()
	h := srv.Router()
	token, _, err := store.APITokens.Create(ctx, 1, "grok", db.APITokenScopeWrite)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.ExecContext(ctx, `INSERT INTO item_info (cookie_id, item_id, item_title) VALUES ('acc1','it-mcp','夸克资料包')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.ExecContext(ctx, `INSERT INTO item_info (cookie_id, item_id, item_title, is_multi_spec) VALUES ('acc1','it-multi','多版本资料',1)`); err != nil {
		t.Fatal(err)
	}

	stockCard := mcpToolJSON(t, h, token, "create_card", map[string]any{"name": "资料包库存", "type": "data", "stock_lines": []string{"链接1", " ", "链接2"}})
	stockCardID := stockCard["card_id"].(float64)
	if appended := mcpToolJSON(t, h, token, "append_card_stock", map[string]any{"card_id": stockCardID, "lines": []string{"链接3"}}); appended["added"].(float64) != 1 {
		t.Fatalf("append=%v", appended)
	}
	textCard := mcpToolJSON(t, h, token, "create_card", map[string]any{"name": "蓝色版链接", "type": "text", "text_content": "https://pan.quark.cn/s/blue 提取码 1234"})
	textCardID := textCard["card_id"].(float64)
	if text, isError := mcpTool(t, h, token, "create_card", map[string]any{"name": "空文本", "type": "text"}); !isError {
		t.Fatalf("text 类型缺内容应失败: %s", text)
	}

	cardsText, _ := mcpTool(t, h, token, "list_cards", map[string]any{})
	if !strings.Contains(cardsText, `"stock": 3`) || strings.Contains(cardsText, "链接2") {
		t.Fatalf("卡密列表应只给出库存条数: %s", cardsText)
	}

	bound := mcpToolJSON(t, h, token, "bind_item_delivery", map[string]any{"account_id": "acc1", "item_id": "it-mcp", "card_id": stockCardID})
	if bound["operation"] != "created" || len(bound["deliveries"].([]any)) != 1 {
		t.Fatalf("首次绑定=%v", bound)
	}
	rules, err := store.Automation.Match(ctx, "acc1", "it-mcp", "order_paid")
	if err != nil || len(rules) != 1 {
		t.Fatalf("真实匹配逻辑应选中新规则: rules=%v err=%v", rules, err)
	}
	ruleID := rules[0].ID

	if text, isError := mcpTool(t, h, token, "bind_item_delivery", map[string]any{"account_id": "acc1", "item_id": "it-mcp", "card_id": textCardID, "spec_name": "版本", "spec_value": "蓝色"}); !isError || !strings.Contains(text, "不是多规格") {
		t.Fatalf("非多规格商品填规格应拒绝: %s", text)
	}
	if text, isError := mcpTool(t, h, token, "bind_item_delivery", map[string]any{"account_id": "acc1", "item_id": "it-multi", "card_id": textCardID}); !isError || !strings.Contains(text, "必须按规格") {
		t.Fatalf("多规格商品不填规格应拒绝: %s", text)
	}
	first := mcpToolJSON(t, h, token, "bind_item_delivery", map[string]any{"account_id": "acc1", "item_id": "it-multi", "card_id": stockCardID, "spec_name": "版本", "spec_value": "红色"})
	if first["operation"] != "created" {
		t.Fatalf("多规格首次绑定=%v", first)
	}
	spec := mcpToolJSON(t, h, token, "bind_item_delivery", map[string]any{"account_id": "acc1", "item_id": "it-multi", "card_id": textCardID, "spec_name": "版本", "spec_value": "蓝色"})
	if spec["operation"] != "appended" || len(spec["deliveries"].([]any)) != 1 || spec["deliveries"].([]any)[0].(map[string]any)["card_id"].(float64) != textCardID {
		t.Fatalf("追加规格=%v", spec)
	}
	if red := mcpToolJSON(t, h, token, "preview_delivery", map[string]any{"account_id": "acc1", "item_id": "it-multi", "spec_name": "版本", "spec_value": "红色"}); red["deliveries"].([]any)[0].(map[string]any)["card_id"].(float64) != stockCardID {
		t.Fatalf("红色规格应命中库存卡: %v", red)
	}
	plain := mcpToolJSON(t, h, token, "preview_delivery", map[string]any{"account_id": "acc1", "item_id": "it-mcp", "quantity": 5})
	deliveries := plain["deliveries"].([]any)
	if len(deliveries) != 1 || deliveries[0].(map[string]any)["card_id"].(float64) != stockCardID || !strings.Contains(strings.Join(anyStrings(plain["warnings"]), ""), "不够") {
		t.Fatalf("无规格预演应命中库存卡并提示库存不足: %v", plain)
	}

	replaced := mcpToolJSON(t, h, token, "bind_item_delivery", map[string]any{"account_id": "acc1", "item_id": "it-multi", "card_id": stockCardID, "spec_name": "版本", "spec_value": "蓝色", "delivery_count": 2})
	if replaced["operation"] != "replaced" || replaced["deliveries"].([]any)[0].(map[string]any)["units"].(float64) != 2 {
		t.Fatalf("替换规格=%v", replaced)
	}
	listed, _ := mcpTool(t, h, token, "list_delivery_rules", map[string]any{"item_id": "it-multi"})
	if strings.Count(listed, `"action_id"`) != 2 {
		t.Fatalf("规则应保留两个发货动作: %s", listed)
	}

	disabled := mcpToolJSON(t, h, token, "set_delivery_rule_enabled", map[string]any{"rule_id": ruleID, "enabled": false})
	if disabled["enabled"] != false {
		t.Fatalf("停用=%v", disabled)
	}
	if after := mcpToolJSON(t, h, token, "preview_delivery", map[string]any{"account_id": "acc1", "item_id": "it-mcp"}); after["will_deliver"] != false {
		t.Fatalf("停用后预演不应发货: %v", after)
	}
	if text, isError := mcpTool(t, h, token, "set_delivery_rule_enabled", map[string]any{"rule_id": 999999, "enabled": true}); !isError {
		t.Fatalf("不存在的规则应失败: %s", text)
	}
	if text, isError := mcpTool(t, h, token, "bind_item_delivery", map[string]any{"account_id": "acc1", "item_id": "it-multi", "card_id": stockCardID, "spec_name": "版本"}); !isError {
		t.Fatalf("规格只填一半应失败: %s", text)
	}
	if text, isError := mcpTool(t, h, token, "bind_item_delivery", map[string]any{"account_id": "acc1", "item_id": "no-such-item", "card_id": stockCardID}); !isError {
		t.Fatalf("不存在的商品应失败: %s", text)
	}

	accounts, _ := mcpTool(t, h, token, "list_accounts", map[string]any{})
	if !strings.Contains(accounts, `"account_id": "acc1"`) || strings.Contains(strings.ToLower(accounts), "cookie_value") {
		t.Fatalf("账号列表=%s", accounts)
	}
	items := mcpToolJSON(t, h, token, "list_items", map[string]any{"keyword": "夸克"})
	if items["total"].(float64) != 1 || !strings.Contains(fmt.Sprint(items["items"]), "it-mcp") {
		t.Fatalf("商品过滤=%v", items)
	}
	if orders := mcpToolJSON(t, h, token, "list_orders", map[string]any{"account_id": "acc1"}); orders["orders"] == nil {
		t.Fatalf("订单=%v", orders)
	}
	card := mcpToolJSON(t, h, token, "get_card", map[string]any{"card_id": stockCardID})
	if len(card["stock_lines"].([]any)) != 3 {
		t.Fatalf("get_card=%v", card)
	}
}

// anyStrings 把 JSON 数组转换为字符串切片，非数组返回空。
func anyStrings(value any) []string {
	values, _ := value.([]any)
	out := make([]string, 0, len(values))
	for _, item := range values {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}
