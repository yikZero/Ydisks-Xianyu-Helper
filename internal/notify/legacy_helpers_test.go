package notify

// 本文件保存只被测试使用的历史薄包装：生产代码已改走带凭证/租约的正式入口，测试仍通过它们覆盖同一实现。

// ---- 邮件 ----
func (n *Notifier) sendEmail(cfg map[string]any, message string) error {
	return n.sendEmailWithUser(cfg, message, 0)
}
