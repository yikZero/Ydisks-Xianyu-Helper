package cookierefresh

import (
	"strings"
	"time"
)

// 本文件保存只被测试使用的历史薄包装：生产代码已改走带凭证/租约的正式入口，测试仍通过它们覆盖同一实现。

// MergeSetCookies 将 Set-Cookie 响应头合并进原 Cookie 字符串。
func MergeSetCookies(original string, setCookies []string) string {
	m := ParseCookieString(original)
	for _, raw := range setCookies {
		first := strings.TrimSpace(strings.Split(raw, ";")[0])
		eq := strings.Index(first, "=")
		if eq <= 0 {
			continue
		}
		name := strings.TrimSpace(first[:eq])
		if name == "" {
			continue
		}
		m[name] = strings.TrimSpace(first[eq+1:])
	}
	return MarshalCookieString(m)
}

// CookieHeaderForURL 按浏览器 Domain/Path/Secure/Expires 规则生成指定 URL 的
// Cookie header，并保留不同路径下的同名 Cookie。
func CookieHeaderForURL(snapshot []BrowserCookie, rawURL string, now time.Time) string {
	header, _ := ScopedCookieHeaderForURL(snapshot, rawURL, now)
	return header
}

// ScopedCookieHeaderForURL 按浏览器匹配规则生成 Cookie header。第二个返回值
// 表示调用方提供了权威快照且 URL 有效；即使没有任何 Cookie 匹配，也会返回
// ("", true)，让调用方能区分“明确为空”和“没有快照，需要兼容回退”。
//
// 此简化入口不具备顶级站点上下文，因此不会发送分区 Cookie。需要处理 CHIPS
// 时应使用 ScopedCookieHeaderForRequest 并传入浏览器提供的 PartitionKey。
func ScopedCookieHeaderForURL(snapshot []BrowserCookie, rawURL string, now time.Time) (string, bool) {
	return ScopedCookieHeaderForRequest(snapshot, rawURL, "", now)
}

// CookieStringFromSnapshot 将浏览器 Cookie 快照压成请求 Cookie 字符串。
func CookieStringFromSnapshot(cookies []BrowserCookie) string {
	parts := make([]string, 0, len(cookies))
	for _, c := range NormalizeSnapshot(cookies) {
		if c.Name == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// MergeOriginalFields 补回浏览器未返回但原 Cookie 中存在的字段。
func MergeOriginalFields(original, browserCookieString string) string {
	m := ParseCookieString(original)
	for k, v := range ParseCookieString(browserCookieString) {
		m[k] = v
	}
	return MarshalCookieString(m)
}
