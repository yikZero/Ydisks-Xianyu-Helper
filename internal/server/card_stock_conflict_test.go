package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"xianyu-go/internal/db"
)

// TestUpdateCardRejectsStaleDataStockSnapshot 验证编辑期间库存被自动发货消费后，携带旧快照的更新返回 409 且不写回已发出的卡密。
func TestUpdateCardRejectsStaleDataStockSnapshot(t *testing.T) {
	srv, store, cleanup := newTestServer(t)
	defer cleanup()
	// ctx 控制夹具操作。
	ctx := context.Background()
	// owner、err 是当前管理员用户。
	owner, err := store.Users.GetAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// cardID、err 是编辑者打开弹窗时库存为 A、B 的 data 卡券。
	cardID, err := store.Cards.Create(ctx, &db.CardFull{Name: "库存卡", Type: "data", DataContent: "A\nB", Enabled: true, UserID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	// consumed、err 模拟编辑期间自动发货先发出 A。
	consumed, err := store.Cards.ConsumeBatchData(ctx, cardID)
	if err != nil || consumed != "A" {
		t.Fatalf("消费库存失败 consumed=%q err=%v", consumed, err)
	}
	handler := srv.Router()
	// cookie 是当前管理员的登录会话。
	cookie := loginHelper(t, handler)
	// path 是被编辑卡券的更新入口。
	path := "/api/v1/cards/" + strconv.FormatInt(cardID, 10)
	// staleRequest 携带打开弹窗时的旧快照提交追加后的库存。
	staleRequest := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"name":"库存卡","type":"data","enabled":true,"data_content":"A\nB\nC","expected_data_content":"A\nB"}`))
	staleRequest.AddCookie(cookie)
	// staleRecorder 捕获冲突响应。
	staleRecorder := httptest.NewRecorder()
	handler.ServeHTTP(staleRecorder, staleRequest)
	if staleRecorder.Code != http.StatusConflict {
		t.Fatalf("旧快照应返回 409，got %d body=%s", staleRecorder.Code, staleRecorder.Body.String())
	}
	assertOpenAPIResponse(t, staleRequest, staleRecorder)
	// current、err 验证冲突后库存仍是自动发货后的剩余行，已发出的 A 没有被写回。
	current, err := store.Cards.GetForDelivery(ctx, cardID)
	if err != nil || current.DataContent != "B" {
		t.Fatalf("冲突后库存被改动 current=%+v err=%v", current, err)
	}
	// freshRequest 使用最新快照重新提交，应正常保存。
	freshRequest := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"name":"库存卡","type":"data","enabled":true,"data_content":"B\nC","expected_data_content":"B"}`))
	freshRequest.AddCookie(cookie)
	// freshRecorder 捕获最新快照的保存结果。
	freshRecorder := httptest.NewRecorder()
	handler.ServeHTTP(freshRecorder, freshRequest)
	assertOpenAPISuccessResponse(t, freshRequest, freshRecorder)
	// saved、err 验证最新快照写入后的库存。
	saved, err := store.Cards.GetForDelivery(ctx, cardID)
	if err != nil || saved.DataContent != "B\nC" {
		t.Fatalf("最新快照未写入 saved=%+v err=%v", saved, err)
	}
}
