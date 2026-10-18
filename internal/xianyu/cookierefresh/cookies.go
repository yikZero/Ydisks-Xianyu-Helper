package cookierefresh

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"
)

const (
	metadataSnapshotKey    = "cookies_refresh_snapshot"
	legacyMetadataSnapshot = "cookie_refresh_snapshot"
)

// BrowserCookie 保存浏览器返回的完整 Cookie 属性。
type BrowserCookie struct {
	Name         string  `json:"name"`
	Value        string  `json:"value"`
	Domain       string  `json:"domain,omitempty"`
	Path         string  `json:"path,omitempty"`
	Expires      float64 `json:"expires,omitempty"`
	HTTPOnly     bool    `json:"httpOnly,omitempty"`
	Secure       bool    `json:"secure,omitempty"`
	SameSite     string  `json:"sameSite,omitempty"`
	PartitionKey string  `json:"partitionKey,omitempty"`
}

// ParseCookieString 把 Cookie 头解析为 name -> value。
func ParseCookieString(s string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq <= 0 {
			continue
		}
		out[strings.TrimSpace(part[:eq])] = strings.TrimSpace(part[eq+1:])
	}
	return out
}

// MarshalCookieString 以稳定顺序把 Cookie map 拼回 Cookie 头。
func MarshalCookieString(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if strings.TrimSpace(k) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, "; ")
}

// SnapshotFromCookieString 为只有扁平 Cookie 的历史账号建立兼容快照。浏览器刷新后
// 应使用真实快照覆盖它，避免长期依赖推断出的 Domain/Path。
func SnapshotFromCookieString(cookieString, domain string) []BrowserCookie {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		domain = ".goofish.com"
	}
	values := ParseCookieString(cookieString)
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]BrowserCookie, 0, len(names))
	for _, name := range names {
		value := values[name]
		out = append(out, BrowserCookie{Name: name, Value: value, Domain: domain, Path: "/", Secure: true})
	}
	return NormalizeSnapshot(out)
}

// ReconcileSnapshotWithCookieString 在调用方暂时只能获得扁平 Cookie 结果时保留
// 已知属性，并同步值与删除项。新增字段使用 .goofish.com 根路径作为兼容作用域。
func ReconcileSnapshotWithCookieString(snapshot []BrowserCookie, cookieString string) []BrowserCookie {
	values := ParseCookieString(cookieString)
	counts := make(map[string]int, len(snapshot))
	for _, cookie := range NormalizeSnapshot(snapshot) {
		counts[cookie.Name]++
	}
	seen := make(map[string]struct{})
	out := make([]BrowserCookie, 0, len(snapshot)+len(values))
	for _, cookie := range NormalizeSnapshot(snapshot) {
		value, exists := values[cookie.Name]
		if !exists {
			continue
		}
		// 扁平 Cookie 字符串无法表达同名 Cookie 的 Domain/Path/PartitionKey。
		// 只有名称唯一时才可确定其新值；否则保留各作用域原值，避免把一个值
		// 错误覆盖到完整 Jar 的所有同名项。
		if counts[cookie.Name] == 1 {
			cookie.Value = value
		}
		out = append(out, cookie)
		seen[cookie.Name] = struct{}{}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, exists := seen[name]; exists {
			continue
		}
		value := values[name]
		out = append(out, BrowserCookie{Name: name, Value: value, Domain: ".goofish.com", Path: "/", Secure: true})
	}
	return NormalizeSnapshot(out)
}

// ScopedCookieHeaderForRequest 为带顶级站点 PartitionKey 的请求生成 Cookie header。
func ScopedCookieHeaderForRequest(snapshot []BrowserCookie, rawURL, partitionKey string, now time.Time) (string, bool) {
	if snapshot == nil {
		return "", false
	}
	target, err := url.Parse(rawURL)
	if err != nil || target.Hostname() == "" {
		return "", false
	}
	type matchedCookie struct {
		cookie BrowserCookie
		index  int
	}
	matched := make([]matchedCookie, 0, len(snapshot))
	for index, cookie := range NormalizeSnapshot(snapshot) {
		// Unpartitioned cookies are always eligible. Partitioned cookies are
		// additionally sent when their key matches the current top-level site.
		if cookie.PartitionKey != "" && cookie.PartitionKey != partitionKey {
			continue
		}
		if cookie.Expires > 0 && cookie.Expires <= float64(now.Unix()) {
			continue
		}
		if cookie.Secure && target.Scheme != "https" && target.Scheme != "wss" {
			continue
		}
		if !cookieDomainMatches(target.Hostname(), cookie.Domain) || !cookiePathMatches(target.EscapedPath(), cookie.Path) {
			continue
		}
		matched = append(matched, matchedCookie{cookie: cookie, index: index})
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if len(matched[i].cookie.Path) != len(matched[j].cookie.Path) {
			return len(matched[i].cookie.Path) > len(matched[j].cookie.Path)
		}
		return matched[i].index < matched[j].index
	})
	parts := make([]string, 0, len(matched))
	for _, item := range matched {
		parts = append(parts, item.cookie.Name+"="+item.cookie.Value)
	}
	return strings.Join(parts, "; "), true
}

// ApplySetCookies 把某次请求响应的 Set-Cookie 应用到完整快照。删除操作只删除
// 相同 name/domain/path 的 Cookie，不会误删其他作用域下的同名项。
func ApplySetCookies(snapshot []BrowserCookie, requestURL string, setCookies []string, now time.Time, partitionKeys ...string) []BrowserCookie {
	target, err := url.Parse(requestURL)
	if err != nil || target.Hostname() == "" {
		return NormalizeSnapshot(snapshot)
	}
	// Cookie identity is name/domain/path/partition key, while header ordering for
	// equal paths follows creation time. Keep the input position when replacing an
	// existing cookie; if it is deleted and later recreated, append it as a new
	// creation, just like Chromium's cookie store.
	state := make([]BrowserCookie, 0, len(snapshot)+len(setCookies))
	positions := make(map[string]int, len(snapshot)+len(setCookies))
	for _, cookie := range NormalizeSnapshot(snapshot) {
		key := snapshotKey(cookie)
		if index, exists := positions[key]; exists {
			state[index] = cookie
			continue
		}
		positions[key] = len(state)
		state = append(state, cookie)
	}
	for _, raw := range setCookies {
		parsed, err := http.ParseSetCookie(raw)
		if err != nil || strings.TrimSpace(parsed.Name) == "" {
			continue
		}
		rawDomain := strings.TrimSpace(parsed.Domain)
		domain := strings.ToLower(rawDomain)
		if domain == "" {
			domain = strings.ToLower(target.Hostname())
		} else {
			// Domain 属性无论是否带前导点都表示域 Cookie；前导点按
			// RFC 6265 应被忽略。不能复用已存 Cookie 的 host-only
			// 匹配规则，否则 Domain=goofish.com 会被错误拒绝。
			if !cookieDomainAttributeMatches(strings.ToLower(target.Hostname()), domain) {
				// Chromium rejects a Domain attribute unrelated to the response host.
				continue
			}
			if !strings.HasPrefix(domain, ".") {
				domain = "." + domain
			}
		}
		cookiePath := parsed.Path
		if cookiePath == "" {
			cookiePath = defaultCookiePath(target.Path)
		}
		if parsed.SameSite == http.SameSiteNoneMode && !parsed.Secure {
			// Chromium rejects SameSite=None cookies unless they are also Secure.
			continue
		}
		if strings.HasPrefix(parsed.Name, "__Secure-") && !parsed.Secure {
			continue
		}
		if strings.HasPrefix(parsed.Name, "__Host-") && (!parsed.Secure || rawDomain != "" || parsed.Path != "/") {
			// __Host- requires Secure, an explicit Path=/, and no Domain attribute.
			continue
		}
		partitionKey := ""
		if parsed.Partitioned {
			if !parsed.Secure || len(partitionKeys) == 0 || strings.TrimSpace(partitionKeys[0]) == "" {
				// Without the top-level site key this cookie cannot be represented
				// faithfully; never turn it into an unpartitioned cookie.
				continue
			}
			partitionKey = strings.TrimSpace(partitionKeys[0])
		}
		cookie := BrowserCookie{
			Name: parsed.Name, Value: parsed.Value, Domain: domain, Path: cookiePath,
			HTTPOnly: parsed.HttpOnly, Secure: parsed.Secure, SameSite: sameSiteLabel(parsed.SameSite), PartitionKey: partitionKey,
		}
		if parsed.MaxAge > 0 {
			// RFC 6265: Max-Age 优先于 Expires，并从收到响应的时刻计算。
			cookie.Expires = float64(now.Add(time.Duration(parsed.MaxAge) * time.Second).Unix())
		} else if !parsed.Expires.IsZero() {
			cookie.Expires = float64(parsed.Expires.Unix())
		}
		key := snapshotKey(cookie)
		if parsed.MaxAge < 0 || (parsed.MaxAge == 0 && !parsed.Expires.IsZero() && !parsed.Expires.After(now)) {
			if index, exists := positions[key]; exists {
				state = append(state[:index], state[index+1:]...)
				delete(positions, key)
				for i := index; i < len(state); i++ {
					positions[snapshotKey(state[i])] = i
				}
			}
			continue
		}
		if index, exists := positions[key]; exists {
			state[index] = cookie
			continue
		}
		positions[key] = len(state)
		state = append(state, cookie)
	}
	return NormalizeSnapshot(state)
}

func snapshotKey(cookie BrowserCookie) string {
	return cookie.Name + "\x00" + strings.ToLower(cookie.Domain) + "\x00" + cookie.Path + "\x00" + cookie.PartitionKey
}

func cookieDomainAttributeMatches(host, domain string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	base := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	return base != "" && (host == base || strings.HasSuffix(host, "."+base))
}

func cookieDomainMatches(host, domain string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return false
	}
	if strings.HasPrefix(domain, ".") {
		base := strings.TrimPrefix(domain, ".")
		return host == base || strings.HasSuffix(host, "."+base)
	}
	return host == domain
}

func cookiePathMatches(requestPath, cookiePath string) bool {
	if requestPath == "" {
		requestPath = "/"
	}
	if cookiePath == "" {
		cookiePath = "/"
	}
	if requestPath == cookiePath {
		return true
	}
	if !strings.HasPrefix(requestPath, cookiePath) {
		return false
	}
	return strings.HasSuffix(cookiePath, "/") || (len(requestPath) > len(cookiePath) && requestPath[len(cookiePath)] == '/')
}

func defaultCookiePath(requestPath string) string {
	if requestPath == "" || requestPath[0] != '/' || requestPath == "/" {
		return "/"
	}
	dir := path.Dir(requestPath)
	if dir == "." || dir == "/" {
		return "/"
	}
	return dir
}

func sameSiteLabel(value http.SameSite) string {
	switch value {
	case http.SameSiteStrictMode:
		return "Strict"
	case http.SameSiteLaxMode:
		return "Lax"
	case http.SameSiteNoneMode:
		return "None"
	default:
		return ""
	}
}

// ChangedCookieNames 返回两个 Cookie 字符串之间发生变化的字段名。
func ChangedCookieNames(before, after string) []string {
	a := ParseCookieString(before)
	b := ParseCookieString(after)
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for k := range seen {
		if a[k] != b[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return names
}

// ChangedSnapshotLabels 返回完整浏览器 Cookie 快照变化标签，格式 name@domain/path。
func ChangedSnapshotLabels(before, after []BrowserCookie) []string {
	key := func(c BrowserCookie) string {
		path := c.Path
		return c.Name + "|" + c.Domain + "|" + path + "|" + c.PartitionKey
	}
	label := func(c BrowserCookie) string {
		path := c.Path
		if c.Domain != "" {
			out := c.Name + "@" + c.Domain + path
			if c.PartitionKey != "" {
				out += "#" + c.PartitionKey
			}
			return out
		}
		return c.Name
	}
	oldMap := make(map[string]BrowserCookie)
	newMap := make(map[string]BrowserCookie)
	for _, c := range NormalizeSnapshot(before) {
		oldMap[key(c)] = c
	}
	for _, c := range NormalizeSnapshot(after) {
		newMap[key(c)] = c
	}
	seen := make(map[string]struct{}, len(oldMap)+len(newMap))
	for k := range oldMap {
		seen[k] = struct{}{}
	}
	for k := range newMap {
		seen[k] = struct{}{}
	}
	labels := make([]string, 0, len(seen))
	for k := range seen {
		old := oldMap[k]
		newCookie := newMap[k]
		if old == newCookie {
			continue
		}
		if newCookie.Name != "" {
			labels = append(labels, label(newCookie))
		} else if old.Name != "" {
			labels = append(labels, label(old))
		}
	}
	sort.Strings(labels)
	return labels
}

// NormalizeSnapshot 补齐默认 path 并保留浏览器返回的创建顺序。Cookie
// header 在 Path 长度相同时必须沿用该顺序，因此不能在这里按名称重排。
// NormalizeSnapshot 归一化Snapshot。
func NormalizeSnapshot(cookies []BrowserCookie) []BrowserCookie {
	if cookies == nil {
		return nil
	}
	out := make([]BrowserCookie, 0, len(cookies))
	for _, c := range cookies {
		if c.Name == "" {
			continue
		}
		if c.Path == "" {
			c.Path = "/"
		}
		out = append(out, c)
	}
	return out
}

// SnapshotFromMetadata 从 cookies.metadata_json 中读取浏览器 Cookie 快照。
func SnapshotFromMetadata(metadata string) []BrowserCookie {
	out, _ := SnapshotFromMetadataOK(metadata)
	return out
}

// SnapshotFromMetadataOK 读取完整 Cookie 快照，并报告 metadata 中是否真的
// 存在快照键。存在空数组时返回非 nil 空切片和 true，避免被误当成历史账号
// 而回退到扁平 Cookie。
func SnapshotFromMetadataOK(metadata string) ([]BrowserCookie, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(metadata)), &m); err != nil {
		return nil, false
	}
	var out []BrowserCookie
	if raw := m[metadataSnapshotKey]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, false
		}
	} else if raw := m[legacyMetadataSnapshot]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, false
		}
	} else {
		return nil, false
	}
	if out == nil {
		out = []BrowserCookie{}
	}
	return NormalizeSnapshot(out), true
}

// MetadataWithSnapshot 写入浏览器 Cookie 快照，保留 metadata 中的其他键。
func MetadataWithSnapshot(metadata string, cookies []BrowserCookie) string {
	m := make(map[string]any)
	if strings.TrimSpace(metadata) != "" {
		_ = json.Unmarshal([]byte(metadata), &m)
	}
	delete(m, legacyMetadataSnapshot)
	m[metadataSnapshotKey] = NormalizeSnapshot(cookies)
	// b 保存已合并 Cookie 快照的 metadata JSON；m 来自 JSON 或固定可序列化字段，编码不会失败。
	b, _ := json.Marshal(m)
	return string(b)
}

// MetadataWithoutSnapshot 清除浏览器 Cookie 快照。
func MetadataWithoutSnapshot(metadata string) string {
	if strings.TrimSpace(metadata) == "" {
		return ""
	}
	m := make(map[string]any)
	if err := json.Unmarshal([]byte(metadata), &m); err != nil {
		return metadata
	}
	delete(m, metadataSnapshotKey)
	delete(m, legacyMetadataSnapshot)
	// b 保存移除 Cookie 快照后的 metadata JSON；m 只包含 JSON 可表达的值。
	b, _ := json.Marshal(m)
	return string(b)
}
