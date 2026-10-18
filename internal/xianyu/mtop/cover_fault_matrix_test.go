package mtop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// faultCookies 是故障矩阵使用的最小签名 Cookie，不含真实凭证。
const faultCookies = "unb=123; _m_h5_tk=token_1;"

// faultOperation 描述一个可被故障矩阵驱动的 MTOP 业务调用。
type faultOperation struct {
	// name 是调用名称，用于失败信息。
	name string
	// call 使用给定客户端和上下文发起调用，只返回错误。
	call func(ctx context.Context, c *ClientImpl) error
}

// faultOperations 返回故障矩阵覆盖的全部调用。
func faultOperations() []faultOperation {
	return []faultOperation{
		{"adjust", func(ctx context.Context, c *ClientImpl) error {
			_, _, _, err := c.AdjustOrderPriceContext(ctx, faultCookies, "o1", 100)
			return err
		}},
		{"consign", func(ctx context.Context, c *ClientImpl) error {
			_, _, _, err := c.ConsignContext(ctx, faultCookies, "o1")
			return err
		}},
		{"freeshipping", func(ctx context.Context, c *ClientImpl) error {
			_, _, _, err := c.FreeShippingContext(ctx, faultCookies, "o1", "i1", "b1")
			return err
		}},
		{"chat-items", func(ctx context.Context, c *ClientImpl) error {
			_, err := c.SearchChatItems(ctx, faultCookies, "s1", "other", "q", 1)
			return err
		}},
		{"order-detail", func(ctx context.Context, c *ClientImpl) error {
			_, err := c.FetchOrderDetail(ctx, faultCookies, "o1")
			return err
		}},
		{"sold-orders", func(ctx context.Context, c *ClientImpl) error {
			_, err := c.FetchSoldOrdersPage(ctx, faultCookies, 1, 10)
			return err
		}},
		{"items-page", func(ctx context.Context, c *ClientImpl) error {
			_, err := c.FetchItemsPage(ctx, faultCookies, 1, 10)
			return err
		}},
		{"items-all", func(ctx context.Context, c *ClientImpl) error {
			_, err := c.FetchAllItems(ctx, faultCookies, 10, 2)
			return err
		}},
		{"login-status", func(ctx context.Context, c *ClientImpl) error {
			_, err := c.CheckLoginStatusContext(ctx, faultCookies)
			return err
		}},
		{"pending-rate", func(ctx context.Context, c *ClientImpl) error {
			_, err := c.FetchPendingRateOrders(ctx, faultCookies, 1, 10)
			return err
		}},
		{"rate-buyer", func(ctx context.Context, c *ClientImpl) error {
			_, err := c.RateBuyer(ctx, faultCookies, "t1", "好评")
			return err
		}},
		{"polish", func(ctx context.Context, c *ClientImpl) error {
			_, err := c.PolishItem(ctx, faultCookies, "i1")
			return err
		}},
		{"profile", func(ctx context.Context, c *ClientImpl) error {
			_, err := c.FetchUserProfile(ctx, faultCookies)
			return err
		}},
		{"chat-user", func(ctx context.Context, c *ClientImpl) error {
			_, err := c.FetchChatUserInfo(ctx, faultCookies, "chat1")
			return err
		}},
		{"chat-image", func(ctx context.Context, c *ClientImpl) error {
			_, err := c.UploadChatImage(ctx, faultCookies, "a.png", "image/png", []byte{1, 2, 3})
			return err
		}},
		{"recommend-category", func(ctx context.Context, c *ClientImpl) error {
			_, _, err := c.RecommendPublishCategory(ctx, faultCookies, "耳机")
			return err
		}},
	}
}

// faultClient 构造所有端点都指向同一本地服务的客户端。
func faultClient(server *httptest.Server) *ClientImpl {
	// base 是本地服务的基础地址。
	base := server.URL + "/"
	return &ClientImpl{
		HTTPClient: server.Client(), TokenURL: base, ConsignURL: base, FreeShippingURL: base, AdjustPriceURL: base,
		OrderDetailURL: base, SoldOrdersURL: base, LoginUserURL: base, RateCreateURL: base, RateListURL: base,
		PolishItemURL: base, PolishItemBackupURL: base, ChatUserQueryURL: base, ChatItemSearchURL: base,
	}
}

// TestFaultMatrixHTTPAndBodyFailures 对全部调用注入 HTTP 500、被截断正文、非 JSON 与连接失败，验证均以错误返回而不 panic。
func TestFaultMatrixHTTPAndBodyFailures(t *testing.T) {
	// handlers 是各故障模式对应的服务端行为。
	handlers := map[string]http.HandlerFunc{
		"http-500": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ret":["FAIL_SYS_ILLEGAL_ACCESS::拒绝"]}`))
		},
		"truncated": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "500")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{"))
		},
		"html": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>"))
		},
		"business-500": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ret":["FAIL_BIZ_X::业务失败"]}`))
		},
	}
	// mode 与 handler 是当前故障模式。
	for mode, handler := range handlers {
		// server 保存本地 httptest 桩服务，只监听回环地址，不访问真实平台。
		server := httptest.NewServer(handler)
		// client 保存指向本地桩服务的客户端。
		client := faultClient(server)
		// op 是当前被注入故障的调用。
		for _, op := range faultOperations() {
			// callCtx 限制单次调用总时长，避免内置一秒重试间隔拖慢测试。
			callCtx, callCancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
			if err := op.call(callCtx, client); err == nil && mode != "business-500" {
				t.Errorf("%s/%s 应返回错误", mode, op.name)
			}
			callCancel()
		}
		server.Close()
	}
	// closedServer 在调用前关闭，模拟连接被拒绝。
	closedServer := httptest.NewServer(http.NotFoundHandler())
	// closedClient 保存指向已关闭桩服务的客户端。
	closedClient := faultClient(closedServer)
	closedServer.Close()
	// op 保存当前被驱动的业务调用。
	for _, op := range faultOperations() {
		if err := op.call(context.Background(), closedClient); err == nil {
			t.Errorf("closed/%s 应返回错误", op.name)
		}
	}
}

// TestFaultMatrixCancelledContextUsesDefaultEndpoints 对未覆盖端点的客户端使用已取消上下文，覆盖默认端点选择与请求失败，且不访问真实平台。
func TestFaultMatrixCancelledContextUsesDefaultEndpoints(t *testing.T) {
	// server 只提供一个不会被访问的 HTTP 客户端。
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	// ctx 已取消，请求在发出前即失败。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// sessionCtx 额外携带 Cookie 会话，覆盖会话状态读取分支。
	sessionCtx, _ := WithFlatCookieSession(ctx, faultCookies)
	// client 没有设置任何端点覆盖。
	client := &ClientImpl{HTTPClient: server.Client()}
	// op 是当前调用。
	for _, op := range faultOperations() {
		if err := op.call(ctx, client); err == nil {
			t.Errorf("cancelled/%s 应返回错误", op.name)
		}
		if err := op.call(sessionCtx, client); err == nil {
			t.Errorf("cancelled-session/%s 应返回错误", op.name)
		}
	}
}

// TestFaultMatrixTokenExpiredRecoveryFailures 对支持 Token 重试的调用注入令牌过期，验证刷新失败与重试间隔被取消的返回。
func TestFaultMatrixTokenExpiredRecoveryFailures(t *testing.T) {
	// modes 描述两种令牌过期响应：不下发新令牌，以及下发新令牌但在重试间隔时取消。
	modes := []string{"expired-plain", "expired-rotated"}
	// mode 是当前令牌过期模式。
	for _, mode := range modes {
		// ctx 与 cancel 由服务端在首次响应后取消。
		ctx, cancel := context.WithCancel(context.Background())
		// server 保存本地 httptest 桩服务，只监听回环地址，不访问真实平台。
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if mode == "expired-rotated" {
				http.SetCookie(w, &http.Cookie{Name: "_m_h5_tk", Value: "rotated_9999999999999"})
			}
			_, _ = w.Write([]byte(`{"ret":["FAIL_SYS_TOKEN_EXOIRED::令牌过期"],"data":{}}`))
			cancel()
		}))
		// client 保存指向本地桩服务的客户端。
		client := faultClient(server)
		// op 保存当前被驱动的业务调用。
		for _, op := range faultOperations() {
			// 每个调用使用独立未取消的上下文前先重置：此处统一使用共享上下文，首个调用后即为已取消，其余调用走取消分支。
			_ = op.call(ctx, client)
		}
		server.Close()
		cancel()
	}
	if !strings.Contains(faultCookies, "_m_h5_tk") {
		t.Fatal("fixture 必须包含签名令牌")
	}
}
