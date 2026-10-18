package netguard

import (
	"net/http"
	"time"
)

// 本文件保存只供测试构造 HTTP 客户端的便捷入口，委托给生产中的 policyHTTPClient。

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
