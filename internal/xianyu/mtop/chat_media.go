package mtop

import (
	"context"
	"fmt"
	"strings"
)

const ChatUserQueryAPI = "https://h5api.m.goofish.com/h5/mtop.taobao.idlemessage.pc.user.query/4.0/"

// ChatUserInfo 是会话对端的昵称和头像。
type ChatUserInfo struct {
	Nickname       string
	AvatarURL      string
	UpdatedCookies string
}

// ChatImageUpload 是上传后可在聊天中发送的图片地址和尺寸。
type ChatImageUpload struct {
	URL            string
	Width          int
	Height         int
	UpdatedCookies string
}

// FetchChatUserInfo resolves the peer identity for a conversation. Xianyu's
// API expects the conversation id as sessionId rather than the user id.
func (c *ClientImpl) FetchChatUserInfo(ctx context.Context, cookiesStr, chatID string) (*ChatUserInfo, error) {
	decoded, updated, err := c.accountTaskRequest(ctx, cookiesStr,
		firstNonEmptyURL(c.ChatUserQueryURL, ChatUserQueryAPI), "mtop.taobao.idlemessage.pc.user.query", "4.0",
		map[string]any{"type": 0, "sessionType": 1, "sessionId": strings.TrimSpace(chatID), "isOwner": false},
		"https://www.goofish.com/")
	if err != nil {
		return nil, err
	}
	userInfo, _ := decoded.Data["userInfo"].(map[string]any)
	if userInfo == nil {
		return nil, fmt.Errorf("会话用户接口响应缺少 userInfo")
	}
	nickname := strings.TrimSpace(mtopString(userInfo["fishNick"]))
	if nickname == "" {
		nickname = strings.TrimSpace(mtopString(userInfo["nick"]))
	}
	return &ChatUserInfo{Nickname: nickname,
		AvatarURL: strings.TrimSpace(mtopString(userInfo["logo"])), UpdatedCookies: updated}, nil
}

// UploadChatImage 上传聊天图片并返回可发送的图片地址和尺寸。
func (c *ClientImpl) UploadChatImage(ctx context.Context, cookiesStr, filename, contentType string, data []byte) (*ChatImageUpload, error) {
	uploaded, updated, err := c.uploadPublishImage(ctx, cookiesStr, PublishImage{
		Filename: filename, ContentType: contentType, Data: data,
	})
	if err != nil {
		return nil, err
	}
	return &ChatImageUpload{URL: uploaded.URL, Width: uploaded.Width, Height: uploaded.Height, UpdatedCookies: updated}, nil
}
