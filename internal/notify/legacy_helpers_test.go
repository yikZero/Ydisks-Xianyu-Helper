package notify

// 本文件保存只供测试调用的邮件发送入口，委托给生产中的邮件渠道实现。

// ---- 邮件 ----
func (n *Notifier) sendEmail(cfg map[string]any, message string) error {
	return n.sendEmailWithUser(cfg, message, 0)
}
