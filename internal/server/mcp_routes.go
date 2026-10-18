package server

import (
	"context"

	"github.com/go-chi/chi/v5"

	"xianyu-go/internal/auth"
	"xianyu-go/internal/mcp"
	"xianyu-go/internal/version"
)

// mountMCP 挂载 /mcp 端点，工具调用以 Token 所属用户身份在进程内转发到同一路由树 r。
func (s *Server) mountMCP(r chi.Router) {
	r.Handle("/mcp", &mcp.Handler{
		Authenticate: func(ctx context.Context, token string) (context.Context, mcp.Principal, error) {
			principal, err := s.Auth.AuthenticateAPIToken(ctx, token)
			if err != nil {
				return nil, mcp.Principal{}, err
			}
			// 清除外层 /mcp 请求的 chi 路由上下文，否则内部转发会沿用已匹配的路由状态而路由到错误的 handler。
			ctx = context.WithValue(ctx, chi.RouteCtxKey, nil)
			return auth.WithTrustedPrincipal(ctx, principal),
				mcp.Principal{TokenID: principal.TokenID, UserID: principal.UserID, Write: principal.Write}, nil
		},
		API:     r,
		Logger:  s.Logger,
		Version: version.Version,
	})
}
