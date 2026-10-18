package ws

import (
	"context"
	"log/slog"
)

// 本文件保存只供测试使用的一步式建连入口；生产路径按 Open → 获取 Token → Register 分步执行。

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
