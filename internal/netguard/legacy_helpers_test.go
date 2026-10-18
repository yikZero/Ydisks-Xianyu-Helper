package netguard

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 本文件保存只被测试使用的历史薄包装：生产代码已改走带凭证/租约的正式入口，测试仍通过它们覆盖同一实现。

// PublicHTTPClient 返回只允许连接公网 IP 的客户端。DNS 与每次重定向都会重新校验。
func PublicHTTPClient(timeout time.Duration) *http.Client {
	return policyHTTPClient(NewOutboundPolicy(true), timeout, 10*time.Second, 10*time.Second)
}

// PolicyHTTPClient 返回绑定指定运行时策略的 HTTP 客户端，测试和组合根可使用它隔离全局默认值。
func PolicyHTTPClient(policy *OutboundPolicy, timeout time.Duration) *http.Client {
	if policy == nil {
		policy = defaultOutboundPolicy
	}
	return policyHTTPClient(policy, timeout, 10*time.Second, 10*time.Second)
}

// TrustedEndpointHTTPClient 用于管理员明确配置的集成端点。目标地址完全由管理员
// 信任并负责，因此不限制 IP 类型、DNS 结果或 HTTP 重定向；仅校验 HTTP(S) URL。
func TrustedEndpointHTTPClient(rawBaseURL string, timeout time.Duration) (*http.Client, error) {
	baseURL, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil || baseURL.Hostname() == "" {
		return nil, fmt.Errorf("服务地址无效")
	}
	if baseURL.Scheme != "http" && baseURL.Scheme != "https" {
		return nil, fmt.Errorf("服务地址只支持 http 或 https")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}
