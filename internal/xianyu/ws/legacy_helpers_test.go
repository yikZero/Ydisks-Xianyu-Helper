package ws

import (
	"context"
	"log/slog"
)

// 本文件保存只被测试使用的历史薄包装：生产代码已改走带凭证/租约的正式入口，测试仍通过它们覆盖同一实现。

// Dial 保留旧的一步式入口；新账号主循环使用 Open → 获取 token → Register，
// 从而与官网 authConnect 的顺序一致。
func Dial(ctx context.Context, cfg Config, logger *slog.Logger) (*Conn, error) {
	conn, err := Open(ctx, cfg, logger)
	if err != nil {
		return nil, err
	}
	if err := conn.Register(ctx, cfg.DeviceID, cfg.AccessToken); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// register 兼容包内旧测试。
func (c *Conn) register(ctx context.Context) error {
	return c.Register(ctx, c.cfg.DeviceID, c.cfg.AccessToken)
}
