package browser

import "xianyu-go/internal/xianyu/cookierefresh"

// 本文件保存只供测试调用的便捷入口，委托给生产中的 Cookie 快照合并实现。

func credentialCookieSnapshot(existing []cookierefresh.BrowserCookie, incoming map[string]string) []cookierefresh.BrowserCookie {
	return credentialCookieSnapshotForURL(existing, incoming, goofishIMURL)
}
