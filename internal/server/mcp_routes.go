package server

import (
	"context"

	"github.com/go-chi/chi/v5"

	"xianyu-go/internal/auth"
	"xianyu-go/internal/mcp"
	"xianyu-go/internal/version"
)

// mountMCP 挂载 /mcp 端点，工具调用以 Token 所属用户身份在进程内转发到管理接口。
// 调用方负责把返回 handler 的 API 设为同一路由树。
func (s *Server) mountMCP(r chi.Router) *mcp.Handler {
	handler := &mcp.Handler{
		Authenticate: func(ctx context.Context, token string) (mcp.Principal, error) {
			principal, err := s.Auth.AuthenticateAPIToken(ctx, token)
			if err != nil {
				return mcp.Principal{}, err
			}
			return mcp.Principal{TokenID: principal.TokenID, UserID: principal.UserID, Username: principal.Username,
				IsAdmin: principal.IsAdmin, Write: principal.Write}, nil
		},
		Trust: func(ctx context.Context, principal mcp.Principal) context.Context {
			// 清除外层 /mcp 请求的 chi 路由上下文，否则内部转发会沿用已匹配的路由状态而路由到错误的 handler。
			ctx = context.WithValue(ctx, chi.RouteCtxKey, nil)
			return auth.WithTrustedPrincipal(ctx, auth.TokenPrincipal{TokenID: principal.TokenID, UserID: principal.UserID,
				Username: principal.Username, IsAdmin: principal.IsAdmin, Write: principal.Write})
		},
		Logger:  s.Logger,
		Version: version.Version,
	}
	r.Handle("/mcp", handler)
	return handler
}
