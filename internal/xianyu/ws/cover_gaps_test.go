package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// startFrameServer 启动本地 WS 服务：升级后依次发送给定文本帧，再等待客户端关闭连接。
func startFrameServer(t *testing.T, frames []string) *httptest.Server {
	t.Helper()
	// srv 是按脚本推送帧的本地服务。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// serverConn 与 acceptErr 是升级后的连接及升级错误。
		serverConn, acceptErr := websocket.Accept(w, r, nil)
		if acceptErr != nil {
			return
		}
		defer serverConn.CloseNow()
		// frame 是当前要推送的文本帧。
		for _, frame := range frames {
			if err := serverConn.Write(r.Context(), websocket.MessageText, []byte(frame)); err != nil {
				return
			}
		}
		// 阻塞到客户端关闭，读取错误即退出。
		_, _, _ = serverConn.Read(r.Context())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestReadPumpRecordsNonJSONAndInvalidLWPFrames 验证读取循环记录非 JSON 与缺少 lwp 的帧，并丢弃它们而不分发。
func TestReadPumpRecordsNonJSONAndInvalidLWPFrames(t *testing.T) {
	// srv 推送一条非 JSON 帧、一条缺少 lwp 的 JSON 帧和一条无法匹配请求的响应帧。
	srv := startFrameServer(t, []string{`not json`, `{"headers":{"mid":"x"}}`, `{"code":200,"headers":{"mid":"nobody"}}`})
	// conn 是连到该服务的包装连接。
	conn := dialLocal(t, srv, Config{})
	// mu 保护 statuses；recorder 在读取协程中被调用。
	var mu sync.Mutex
	// statuses 按到达顺序保存记录器收到的解析状态。
	statuses := []string{}
	// done 在三个状态都出现后关闭。
	done := make(chan struct{})
	conn.SetRecorder(func(_, _, _, status, _ string) {
		mu.Lock()
		defer mu.Unlock()
		statuses = append(statuses, status)
		// 响应帧会产生 json 状态，三帧全部到达后通知测试。
		if len(statuses) == 4 {
			close(done)
		}
	})
	conn.ensureReadPump()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("记录器未收到全部帧")
	}
	mu.Lock()
	defer mu.Unlock()
	if statuses[0] != "non_json" || statuses[1] != "json" || statuses[2] != "skip_invalid_lwp" {
		t.Fatalf("statuses=%v", statuses)
	}
	if conn.dispatchResponse(map[string]any{"headers": map[string]any{"mid": "nobody"}}) {
		t.Fatal("没有等待者时不应分发")
	}
}

// TestRegisterRejectsMalformedAccessToken 验证无法解码或包含非法 UTF-8 的 accessToken 在发送前被拒绝。
func TestRegisterRejectsMalformedAccessToken(t *testing.T) {
	// srv 提供注册握手服务，但本用例不应发出任何注册请求。
	srv, _ := startRegServer(t)
	// conn 是连到该服务的包装连接。
	conn := dialLocal(t, srv, Config{})
	// token 依次是非法百分号转义和解码后非 UTF-8 的 token。
	for _, token := range []string{"%zz", "%ff"} {
		if err := conn.Register(context.Background(), "device", token); err == nil {
			t.Fatalf("token=%q 必须被拒绝", token)
		}
	}
}

// TestOpenAndDialFailFastWhenContextCancelled 验证上下文已取消时 Open 与 Dial 立即失败，且 nil logger 回退默认 logger。
func TestOpenAndDialFailFastWhenContextCancelled(t *testing.T) {
	// ctx 已取消，握手不会访问真实平台。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, Config{}, nil); err == nil {
		t.Fatal("取消的上下文必须使 Open 失败")
	}
	if _, err := Dial(ctx, Config{}, nilLogger()); err == nil {
		t.Fatal("取消的上下文必须使 Dial 失败")
	}
}

// TestOpenBatchRequiresConfiguredRace 验证没有竞速连接配置时直接报错。
func TestOpenBatchRequiresConfiguredRace(t *testing.T) {
	// original 保存原始竞速延迟，测试结束后恢复。
	original := batchConnectDelays
	batchConnectDelays = nil
	t.Cleanup(func() { batchConnectDelays = original })
	if _, err := openBatch(context.Background(), "ws://127.0.0.1:1", Config{}, nilLogger()); err == nil {
		t.Fatal("缺少竞速配置必须报错")
	}
}

// TestNewConnAndReadPumpDefaultLogger 验证未提供 logger 时连接回退到默认 logger。
func TestNewConnAndReadPumpDefaultLogger(t *testing.T) {
	// srv 提供空闲 WS 服务。
	srv := startFrameServer(t, nil)
	// dialed 与 err 是原始 WS 连接及拨号错误。
	dialed, _, err := websocket.Dial(context.Background(), wsURL(srv), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = newConn(dialed, Config{}, nil)
	// conn 是未设置 logger 的原始连接包装。
	conn := &Conn{ws: dialed}
	conn.ensureReadPump()
	if conn.logger == nil {
		t.Fatal("读取循环启动后 logger 不应为空")
	}
	_ = conn.Close()
}

// TestOutgoingRequestIDAndMidKeyEdges 覆盖发送请求 mid 的空上下文与空白 mid。
func TestOutgoingRequestIDAndMidKeyEdges(t *testing.T) {
	// nilCtx 模拟调用方遗漏上下文。
	var nilCtx context.Context
	if outgoingRequestID(nilCtx) != "" {
		t.Fatal("空上下文应返回空 mid")
	}
	if got := outgoingRequestID(WithOutgoingRequestID(context.Background(), " m1 ")); got != "m1" {
		t.Fatalf("got=%q", got)
	}
	if midKey("   ") != "" {
		t.Fatal("空白 mid 应返回空键")
	}
}

// TestExtractSyncPayloadsAndDecodeEdges 覆盖同步包缺字段、非对象条目、解密失败与解密后非 JSON。
func TestExtractSyncPayloadsAndDecodeEdges(t *testing.T) {
	// ok 保存被测函数返回的成功或命中标志。
	if _, ok := extractSyncPayloads(map[string]any{}); ok {
		t.Fatal("缺少 body 应失败")
	}
	// ok 保存被测函数返回的成功或命中标志。
	if _, ok := extractSyncPayloads(map[string]any{"body": map[string]any{}}); ok {
		t.Fatal("缺少 syncPushPackage 应失败")
	}
	// ok 保存被测函数返回的成功或命中标志。
	if _, ok := extractSyncPayloads(map[string]any{"body": map[string]any{"syncPushPackage": map[string]any{"data": []any{}}}}); ok {
		t.Fatal("空 data 应失败")
	}
	// entries 与 ok 是包含非对象与空 data 条目的解析结果。
	entries, ok := extractSyncPayloads(map[string]any{"body": map[string]any{"syncPushPackage": map[string]any{"data": []any{"x", map[string]any{"data": " "}, map[string]any{"data": "abc"}}}}})
	if !ok || len(entries) != 3 || entries[0].valid || entries[1].valid || !entries[2].valid {
		t.Fatalf("entries=%+v ok=%v", entries, ok)
	}
	if _, err := decodeSyncData("!!!not-valid!!!"); err == nil {
		t.Fatal("无法解密的数据必须报错")
	}
	if parsed, err := decodeSyncData("eyJhIjoxfQ=="); err != nil || parsed["a"] == nil {
		t.Fatalf("parsed=%v err=%v", parsed, err)
	}
}

// TestOutgoingEchoValidation 逐层破坏出站回显响应，验证每个校验分支都拒绝，完整响应则通过。
func TestOutgoingEchoValidation(t *testing.T) {
	// sent 是本次请求写入的文字正文。
	sent := []byte(`{"contentType":1,"text":{"text":"你好"}}`)
	// encoded 是回显中的 Base64 正文。
	encoded := "eyJjb250ZW50VHlwZSI6MSwidGV4dCI6eyJ0ZXh0Ijoi5L2g5aW9In19"
	// build 构造可被逐项破坏的完整响应。
	build := func() map[string]any {
		return map[string]any{"body": map[string]any{
			"messageId": "pnm-1",
			"extension": map[string]any{"senderUserId": "me@goofish"},
			"content":   map[string]any{"custom": map[string]any{"data": encoded}},
		}}
	}
	// echo 保存被测函数解析出的出站回显；ok 保存被测函数返回的成功或命中标志。
	if echo, ok := outgoingEchoFromResponse(build(), " rid ", "me", "c@goofish", "peer@goofish", sent); !ok || echo.MessageType != "text" || echo.Text != "你好" || echo.RequestID != "rid" {
		t.Fatalf("echo=%+v ok=%v", echo, ok)
	}
	// breakers 是依次破坏响应某一层的修改器。
	breakers := map[string]func(map[string]any){
		"no-body":       func(m map[string]any) { delete(m, "body") },
		"no-message-id": func(m map[string]any) { delete(m["body"].(map[string]any), "messageId") },
		"no-extension":  func(m map[string]any) { delete(m["body"].(map[string]any), "extension") },
		"other-sender": func(m map[string]any) {
			m["body"].(map[string]any)["extension"] = map[string]any{"senderUserId": "x"}
		},
		"no-content": func(m map[string]any) { delete(m["body"].(map[string]any), "content") },
		"no-custom": func(m map[string]any) {
			m["body"].(map[string]any)["content"] = map[string]any{}
		},
		"no-data": func(m map[string]any) {
			m["body"].(map[string]any)["content"] = map[string]any{"custom": map[string]any{}}
		},
		"bad-base64": func(m map[string]any) {
			m["body"].(map[string]any)["content"] = map[string]any{"custom": map[string]any{"data": "***"}}
		},
	}
	// name 与 breaker 是当前破坏项。
	for name, breaker := range breakers {
		// response 保存构造的平台响应帧。
		response := build()
		breaker(response)
		// ok 保存被测函数返回的成功或命中标志。
		if _, ok := outgoingEchoFromResponse(response, "r", "me", "c", "p", sent); ok {
			t.Fatalf("%s 必须被拒绝", name)
		}
	}
	// unsupported 是不被自动化确认支持的各类正文。
	unsupported := []string{
		`not json`, `{"contentType":"x"}`, `{"contentType":1}`, `{"contentType":1,"text":{"text":" "}}`,
		`{"contentType":2}`, `{"contentType":2,"image":{}}`, `{"contentType":2,"image":{"pics":[]}}`,
		`{"contentType":2,"image":{"pics":["x"]}}`, `{"contentType":2,"image":{"pics":[{}]}}`, `{"contentType":9}`,
	}
	// body 是当前不支持的正文。
	for _, body := range unsupported {
		// ok 保存被测函数返回的成功或命中标志。
		if _, _, _, ok := outgoingEchoContent([]byte(body)); ok {
			t.Fatalf("body=%s 不应被支持", body)
		}
	}
	// kind 保存被测函数返回的发送结果类别；ok 保存被测函数返回的成功或命中标志；url 保存被测函数返回的地址。
	if kind, _, url, ok := outgoingEchoContent([]byte(`{"contentType":2,"image":{"pics":[{"url":"https://img/x.png"}]}}`)); !ok || kind != "image" || url != "https://img/x.png" {
		t.Fatalf("kind=%s url=%s ok=%v", kind, url, ok)
	}
	// observe 为空或连接为空时不应 panic。
	(*Conn)(nil).observeOutgoingResponse(nil, "", "", "", "", nil)
	(&Conn{}).observeOutgoingResponse(nil, "", "", "", "", nil)
	// codes 描述严格状态码解析的各种输入。
	codes := []struct {
		value any
		want  int
		ok    bool
	}{
		{7, 7, true}, {float64(3), 3, true}, {float64(1.5), 0, false}, {json.Number("4"), 4, true},
		{json.Number("4.5"), 0, false}, {" 12 ", 12, true}, {"", 0, false}, {"1x", 0, false}, {true, 0, false},
	}
	// tc 是当前状态码样例。
	for _, tc := range codes {
		if got, ok := strictChatSendResponseCode(tc.value); got != tc.want || ok != tc.ok {
			t.Fatalf("value=%v got=%d ok=%v", tc.value, got, ok)
		}
	}
}

// TestRegErrorReasonUnknownFrame 验证注册失败帧没有任何原因字段时返回统一的未知原因。
func TestRegErrorReasonUnknownFrame(t *testing.T) {
	if got := regErrorReason(map[string]any{}); got != "unknown authentication error" {
		t.Fatalf("got=%q", got)
	}
}
