package adapter

import (
	"context"
	accountapp "xianyu-go/internal/application/account"
	chatapp "xianyu-go/internal/application/chat"
	"xianyu-go/internal/xianyu/cookierefresh"
)

// 本文件保存只被测试使用的历史薄包装：生产代码已改走带凭证/租约的正式入口，测试仍通过它们覆盖同一实现。

// getCookieValue 读取图片上传所需的账号凭证；调用方不得记录返回值。
func (r chatCredentialRepository) getCookieValue(ctx context.Context, accountID string) (string, error) {
	if r.store == nil || r.store.Cookies == nil {
		return "", chatapp.ErrUnavailable
	}
	return r.store.Cookies.GetValue(ctx, accountID)
}

// updateCookieValue 保存图片上传后平台刷新的账号凭证。
func (r chatCredentialRepository) updateCookieValue(ctx context.Context, accountID, cookieValue string) error {
	if r.store == nil || r.store.Cookies == nil {
		return chatapp.ErrUnavailable
	}
	return r.store.Cookies.UpdateValueExisting(ctx, accountID, cookieValue)
}

// HasStoredCookieCredential 判断凭证视图是否包含扁平 Cookie 或完整 Cookie 快照。
func HasStoredCookieCredential(detail *accountapp.CredentialDetail) bool {
	if detail == nil {
		return false
	}
	if detail.Value != "" {
		return true
	}
	// complete 表示 metadata 是否包含可用于平台请求的完整快照。
	_, complete := cookierefresh.SnapshotFromMetadataOK(detail.MetadataJSON)
	return complete
}
