package server

import (
	"context"
	"errors"

	accountapp "xianyu-go/internal/application/account"
)

// loadCookieSummaryDetail 读取账号非敏感摘要并转换为 Server 内部的兼容模型。
func (s *Server) loadCookieSummaryDetail(ctx context.Context, userID int64, cookieID string) (accountapp.AccountSummary, error) {
	if s == nil {
		return accountapp.AccountSummary{}, errors.New("账号摘要读取服务未初始化")
	}
	// service 提供按用户和账号联合过滤的非敏感摘要；Server 不直接访问账号表。
	service := s.accountSummaryApplication()
	if service == nil {
		return accountapp.AccountSummary{}, errors.New("账号摘要服务未初始化")
	}
	// summary、queryErr 保存应用服务返回的非敏感摘要及查询错误。
	summary, queryErr := service.GetOwnedSummary(ctx, userID, cookieID)
	if queryErr != nil {
		return accountapp.AccountSummary{}, queryErr
	}
	return summary, nil
}
