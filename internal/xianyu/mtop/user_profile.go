// Package mtop: 账号资料域 — mtop.idle.web.user.page.nav 调用与解析。
package mtop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"xianyu-go/internal/xianyu/protocol"
)

// FetchUserProfile 获取当前 cookie 对应账号的实时昵称和头像。
func (c *ClientImpl) FetchUserProfile(ctx context.Context, cookiesStr string) (*UserProfileResult, error) {
	currentCookies := cookiesStr
	if session := cookieSessionFromContext(ctx); session != nil {
		currentCookies, _, _ = session.State()
	}
	var lastRet []string
	for attempt := 0; attempt < 4; attempt++ {
		res, ret, updatedCookies, err := c.fetchUserProfileOnce(ctx, currentCookies)
		if err != nil {
			if !IsMTopTokenExpiredErr(err) {
				return nil, err
			}
			lastRet = mtopErrorRet(err)
		} else {
			lastRet = ret
			if res != nil {
				return res, nil
			}
			// failure 保存平台非成功响应的统一失败分类。
			failure := c.mtopResponseFailure("账号资料接口", http.StatusOK, ret, "")
			if !IsMTopTokenExpiredErr(failure) {
				return nil, failure
			}
		}
		if updatedCookies != "" {
			// tokenChanged 表示响应 Cookie 是否真的轮换了签名令牌；普通 Cookie 变化不能跳过主动刷新。
			tokenChanged := mtopTokenCookieChanged(currentCookies, updatedCookies)
			currentCookies = updatedCookies
			if tokenChanged {
				if err := sleepCtx(ctx, MTopRetryGap); err != nil {
					return nil, err
				}
				continue
			}
		}
		if err := sleepCtx(ctx, MTopRetryGap); err != nil {
			return nil, err
		}
		refreshed, err := c.RefreshTokenContext(ctx, currentCookies)
		if err != nil {
			return nil, fmt.Errorf("刷新 mtop token 失败: %w", err)
		}
		currentCookies = refreshed.UpdatedCookies
	}
	return nil, fmt.Errorf("账号资料接口 token 重试失败: %w", c.mtopResponseFailure("账号资料接口", http.StatusOK, lastRet, "重试次数已耗尽"))
}

func (c *ClientImpl) fetchUserProfileOnce(ctx context.Context, cookiesStr string) (*UserProfileResult, []string, string, error) {
	hc := c.httpClient()

	dataVal := "{}"
	signingCookies, requestCookies := mtopRequestCookies(ctx, cookiesStr, "https://www.goofish.com/", UserPageNavAPI)
	t := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sign := protocol.GenerateSign(t, protocol.SignToken(signingCookies), dataVal)
	query := buildUserPageNavQuery(t, sign)
	body := "data=" + url.QueryEscape(dataVal)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, UserPageNavAPI+"?"+query, strings.NewReader(body))
	if err != nil {
		return nil, nil, cookiesStr, err
	}
	setCommonHeaders(req, requestCookies)

	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, cookiesStr, fmt.Errorf("账号资料请求失败: %w", err)
	}
	defer resp.Body.Close()
	updated := absorbMTopResponseCookies(ctx, cookiesStr, resp)
	raw, err := readMTopBody(resp)
	if err != nil {
		return nil, nil, updated, c.mtopResponseFailureWithCause("账号资料接口", resp.StatusCode, nil, "读取响应失败", err)
	}

	var decoded struct {
		Ret  []string       `json:"ret"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, nil, updated, c.mtopResponseFailureWithCause("账号资料接口", resp.StatusCode, nil, "JSON 解析失败", err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, decoded.Ret, updated, c.mtopResponseFailure("账号资料接口", resp.StatusCode, decoded.Ret, "HTTP 状态异常")
	}
	if !hasMTopSuccess(decoded.Ret) {
		return nil, decoded.Ret, updated, nil
	}

	profile := parseUserProfile(decoded.Data)
	profile.UpdatedCookies = updated
	return profile, decoded.Ret, updated, nil
}

func buildUserPageNavQuery(t, sign string) string {
	parts := [][2]string{
		{"jsv", "2.7.2"},
		{"appKey", protocol.SignAppKey},
		{"t", t},
		{"sign", sign},
		{"v", "1.0"},
		{"type", "originaljson"},
		{"accountSite", "xianyu"},
		{"dataType", "json"},
		{"timeout", "20000"},
		{"api", "mtop.idle.web.user.page.nav"},
		{"sessionOption", "AutoLoginOnly"},
		{"ecode", "0"},
		{"spm_cnt", "a21ybx.home.0.0"},
	}
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(p[0])
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(p[1]))
	}
	return b.String()
}

func parseUserProfile(data map[string]any) *UserProfileResult {
	module, _ := data["module"].(map[string]any)
	base, _ := module["base"].(map[string]any)
	if base == nil {
		return &UserProfileResult{}
	}
	nickname := strings.TrimSpace(mtopString(base["displayName"]))
	displayNick := strings.TrimSpace(mtopString(base["displayNick"]))
	if nickname == "" {
		nickname = displayNick
	}
	return &UserProfileResult{
		Nickname:    nickname,
		DisplayNick: displayNick,
		AvatarURL:   strings.TrimSpace(mtopString(base["avatar"])),
	}
}
