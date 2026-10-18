package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	itemapp "xianyu-go/internal/application/items"
	"xianyu-go/internal/db"
)

// mcpPublishStub 记录发布输入，并像真实服务一样把发布成功的商品写入本地商品表。
type mcpPublishStub struct {
	store  *db.Store
	inputs []itemapp.PublishInput
}

// PublishSingle 记录输入并返回固定商品 ID。
func (s *mcpPublishStub) PublishSingle(ctx context.Context, input itemapp.PublishInput) (itemapp.PublishOutcome, error) {
	s.inputs = append(s.inputs, input)
	if _, err := s.store.DB.ExecContext(ctx, `INSERT INTO item_info (cookie_id, item_id, item_title) VALUES (?, 'pub-1', ?)`, input.CookieID, input.Title); err != nil {
		return itemapp.PublishOutcome{}, err
	}
	return itemapp.PublishOutcome{Result: &itemapp.PublishResult{ItemID: "pub-1", ItemURL: "https://www.goofish.com/item?id=pub-1", Title: input.Title,
		PriceText: "9.90", Quantity: input.Quantity, CategoryName: "电子资料"}}, nil
}

// TestMCPPublishItemPreviewConfirmAndBind 覆盖 publish_item：预览不发布、参数和图片目录边界、确认发布并绑定发货、同标题拦截。
func TestMCPPublishItemPreviewConfirmAndBind(t *testing.T) {
	uploadRoot := t.TempDir()
	t.Setenv("XIANYU_UPLOAD_DIR", uploadRoot)
	imageDir := filepath.Join(uploadRoot, "mcp")
	if err := os.MkdirAll(imageDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imageDir, "cover.png"), []byte("\x89PNG\r\n\x1a\nrest"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploadRoot, "outside.png"), []byte("\x89PNG\r\n\x1a\nrest"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, store, cleanup := newTestServer(t)
	defer cleanup()
	stub := &mcpPublishStub{store: store}
	srv.applications.itemSinglePublish = stub
	h := srv.Router()
	token, _, err := store.APITokens.Create(context.Background(), 1, "grok", db.APITokenScopeWrite)
	if err != nil {
		t.Fatal(err)
	}

	_, initialized := mcpPost(t, h, token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if instructions := initialized["result"].(map[string]any)["instructions"].(string); !strings.Contains(instructions, imageDir) {
		t.Fatalf("initialize 说明应给出本地图片目录: %s", instructions)
	}
	card := mcpToolJSON(t, h, token, "create_card", map[string]any{"name": "夸克资料", "type": "text", "text_content": "https://pan.quark.cn/s/x 提取码 1234"})
	args := map[string]any{"account_id": "acc1", "title": "考研资料合集", "description": "网盘链接，付款后自动发送", "price": 9.9,
		"images": []string{"cover.png"}, "card_id": card["card_id"]}

	preview := mcpToolJSON(t, h, token, "publish_item", args)
	if preview["published"] != false || preview["next_step"] == nil || preview["auto_delivery_card"] == nil || len(stub.inputs) != 0 {
		t.Fatalf("不带 confirm 只能预览: %v inputs=%d", preview, len(stub.inputs))
	}
	for name, bad := range map[string]map[string]any{
		"缺描述":    {"account_id": "acc1", "title": "x", "price": 1, "images": []string{"cover.png"}},
		"价格为 0":  {"account_id": "acc1", "title": "x", "description": "d", "price": 0, "images": []string{"cover.png"}},
		"目录外图片":  {"account_id": "acc1", "title": "x", "description": "d", "price": 1, "images": []string{"../outside.png"}},
		"绝对路径越界": {"account_id": "acc1", "title": "x", "description": "d", "price": 1, "images": []string{filepath.Join(uploadRoot, "outside.png")}},
		"他人账号":   {"account_id": "nobody", "title": "x", "description": "d", "price": 1, "images": []string{"cover.png"}},
	} {
		if text, isError := mcpTool(t, h, token, "publish_item", bad); !isError {
			t.Fatalf("%s 应失败: %s", name, text)
		}
	}

	args["confirm"] = true
	args["images"] = []string{filepath.Join(imageDir, "cover.png")}
	published := mcpToolJSON(t, h, token, "publish_item", args)
	if published["published"] != true || published["item_id"] != "pub-1" || published["bind_error"] != nil || published["delivery"] == nil {
		t.Fatalf("确认后应发布并绑定发货: %v", published)
	}
	if len(stub.inputs) != 1 {
		t.Fatalf("应只发布一次，got %d", len(stub.inputs))
	}
	input := stub.inputs[0]
	if input.Title != "考研资料合集" || input.Description != "网盘链接，付款后自动发送" || input.PriceCents != 990 || input.Quantity != 1 ||
		input.PostageMode != "free" || len(input.Images) != 1 || input.Images[0].ContentType != "image/png" || input.Images[0].Filename != "cover.png" {
		t.Fatalf("发布输入=%+v", input)
	}
	matched, err := store.Automation.Match(context.Background(), "acc1", "pub-1", "order_paid")
	if err != nil || matched == nil {
		t.Fatalf("发布后应命中付款发货规则: rule=%v err=%v", matched, err)
	}

	if text, isError := mcpTool(t, h, token, "publish_item", args); !isError || !strings.Contains(text, "同标题") || len(stub.inputs) != 1 {
		t.Fatalf("同标题商品应拦截重复发布: %s inputs=%d", text, len(stub.inputs))
	}
}
