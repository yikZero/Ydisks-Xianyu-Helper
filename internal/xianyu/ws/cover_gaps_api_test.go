package ws

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// startScriptedAPIServer 启动按 lwp 路径返回指定业务码的本地服务；closeAfter 非空时，应答该路径后立即关闭连接。
func startScriptedAPIServer(t *testing.T, codes map[string]int, closeAfter string) *httptest.Server {
	t.Helper()
	// srv 是按路径脚本应答请求帧的服务。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// serverConn 与 acceptErr 是升级后的连接及升级错误。
		serverConn, acceptErr := websocket.Accept(w, r, nil)
		if acceptErr != nil {
			return
		}
		defer serverConn.CloseNow()
		for {
			// raw 与 readErr 是客户端请求帧及读取错误。
			_, raw, readErr := serverConn.Read(r.Context())
			if readErr != nil {
				return
			}
			// message 是解码后的请求帧。
			var message map[string]any
			if json.Unmarshal(raw, &message) != nil {
				continue
			}
			// path 与 headers 是请求路径和携带 mid 的头。
			path, _ := message["lwp"].(string)
			// headers 保存请求帧中携带 mid 的头部。
			headers, _ := message["headers"].(map[string]any)
			// code 是该路径配置的业务码，默认 200。
			code := 200
			// configured 保存脚本中为当前路径配置的业务码；ok 保存被测函数返回的成功或命中标志。
			if configured, ok := codes[path]; ok {
				code = configured
			}
			// response 是带非空 body 的应答帧。
			response, _ := json.Marshal(map[string]any{"code": code, "headers": map[string]any{"mid": headers["mid"]}, "body": map[string]any{"ok": true}})
			if serverConn.Write(r.Context(), websocket.MessageText, response) != nil {
				return
			}
			if closeAfter == path {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// closedConn 返回读取循环已结束的连接，其后所有请求都会因连接结束而失败。
func closedConn(t *testing.T) *Conn {
	t.Helper()
	// srv 是立即关闭连接的服务。
	srv := startFrameServer(t, nil)
	// conn 是连到服务的包装连接。
	conn := dialLocal(t, srv, Config{})
	conn.ensureReadPump()
	_ = conn.ws.CloseNow()
	<-conn.readDone
	return conn
}

// TestHistoryAndConversationErrorBranches 覆盖消息历史与会话列表的连接失败、非 200 状态、缺少 body 与平台拒绝。
func TestHistoryAndConversationErrorBranches(t *testing.T) {
	ctx := context.Background()
	// dead 是已断开的连接。
	dead := closedConn(t)
	if _, err := dead.ListUserMessages(ctx, "cid", 0, 0); err == nil {
		t.Fatal("断开连接必须失败")
	}
	if _, err := dead.ListConversations(ctx, 0, 0); err == nil {
		t.Fatal("断开连接必须失败")
	}
	// rejected 连接的服务返回 500。
	rejected, _ := newAPIResponseConn(t, map[string]any{"ok": true}, http.StatusInternalServerError)
	if _, err := rejected.ListUserMessages(ctx, "cid", 0, 0); err == nil {
		t.Fatal("非 200 必须失败")
	}
	// reasoned 连接的服务在 body 中给出失败原因。
	reasoned, _ := newAPIResponseConn(t, map[string]any{"reason": "denied"}, http.StatusOK)
	if _, err := reasoned.ListConversations(ctx, 0, 0); err == nil {
		t.Fatal("带 reason 必须失败")
	}
	if _, err := reasoned.ListUserMessages(ctx, "cid", 0, 0); err == nil {
		t.Fatal("带 reason 必须失败")
	}
	// bodyless 连接的服务不返回 body。
	bodyless, _ := newAPIResponseConn(t, nil, http.StatusOK)
	if _, err := bodyless.ListConversations(ctx, 0, 0); err == nil {
		t.Fatal("缺少 body 必须失败")
	}
}

// TestMarkChatReadBranches 覆盖已读上报的空列表、非 PNM 过滤、成功、平台拒绝与连接失败。
func TestMarkChatReadBranches(t *testing.T) {
	ctx := context.Background()
	// ids 包含空值、非 PNM 关联标识与一个合法 PNM 标识。
	ids := []map[string]any{{}, {"messageId": "uuid-only"}, {"messageId": "1.PNM"}}
	// ok 连接的服务返回 200。
	okConn, _ := newAPIResponseConn(t, map[string]any{}, http.StatusOK)
	if err := okConn.MarkChatRead(ctx, "cid", ids[:2]); err != nil {
		t.Fatal(err)
	}
	if err := okConn.MarkChatRead(ctx, "cid", ids); err != nil {
		t.Fatal(err)
	}
	// rejected 连接的服务返回 400，应只告警并继续。
	rejected, _ := newAPIResponseConn(t, map[string]any{}, http.StatusBadRequest)
	if err := rejected.MarkChatRead(ctx, "cid", ids); err != nil {
		t.Fatal(err)
	}
	if err := closedConn(t).MarkChatRead(ctx, "cid", ids); err == nil {
		t.Fatal("断开连接必须失败")
	}
}

// TestSendChatContentErrorKinds 覆盖发送前取消、缺少参数、连接失败（结果不确定）与平台拒绝的错误分类。
func TestSendChatContentErrorKinds(t *testing.T) {
	// conn 的服务端对发送请求返回 500。
	conn, _ := newAPIResponseConn(t, map[string]any{}, http.StatusInternalServerError)
	// cancelled 是已取消的上下文。
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	// sendErr 是发送前取消的错误。
	var sendErr *SendError
	if err := conn.SendText(cancelled, "me", "c", "p", "hi"); !errors.As(err, &sendErr) || sendErr.Kind != SendNotSent {
		t.Fatalf("err=%v", err)
	}
	if err := conn.SendText(context.Background(), "", "c", "p", "hi"); !errors.As(err, &sendErr) || sendErr.Kind != SendNotSent {
		t.Fatalf("err=%v", err)
	}
	if err := conn.SendText(context.Background(), "me", "c", "p", "hi"); err == nil {
		t.Fatal("平台 500 必须报错")
	}
	if err := closedConn(t).SendText(context.Background(), "me", "c", "p", "hi"); !errors.As(err, &sendErr) || sendErr.Kind != SendUncertain {
		t.Fatalf("err=%v", err)
	}
	// unmarshalable 内容无法序列化，必须在发送前失败。
	if err := conn.sendChatContent(context.Background(), "me", "c", "p", make(chan int)); !errors.As(err, &sendErr) || sendErr.Kind != SendNotSent {
		t.Fatalf("err=%v", err)
	}
}

// TestHandleSyncExtraFailureBranches 覆盖增量同步恢复中 getState/ackDiff 的请求失败与异常状态。
func TestHandleSyncExtraFailureBranches(t *testing.T) {
	ctx := context.Background()
	// msg 是要求恢复同步状态的服务端帧。
	msg := map[string]any{"body": map[string]any{"syncExtraType": map[string]any{"type": float64(1)}}}
	if err := (&Conn{}).handleSyncExtra(ctx, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := closedConn(t).handleSyncExtra(ctx, msg); err == nil {
		t.Fatal("getState 请求失败必须报错")
	}
	// scenarios 描述不同服务端脚本。
	scenarios := map[string]struct {
		codes      map[string]int
		closeAfter string
	}{
		"getState-500": {map[string]int{"/r/SyncStatus/getState": 500}, ""},
		"ackDiff-500":  {map[string]int{"/r/SyncStatus/ackDiff": 500}, ""},
		"ackDiff-dead": {nil, "/r/SyncStatus/getState"},
	}
	// name 与 sc 是当前场景。
	for name, sc := range scenarios {
		// conn 保存连到本地桩服务的连接。
		conn := dialLocal(t, startScriptedAPIServer(t, sc.codes, sc.closeAfter), Config{})
		// handleCtx 限制单场景等待。
		handleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := conn.handleSyncExtra(handleCtx, msg); err == nil {
			t.Fatalf("%s 必须报错", name)
		}
		cancel()
	}
	// 正常路径：两次请求均 200。
	conn := dialLocal(t, startScriptedAPIServer(t, nil, ""), Config{})
	if err := conn.handleSyncExtra(ctx, msg); err != nil {
		t.Fatal(err)
	}
}
