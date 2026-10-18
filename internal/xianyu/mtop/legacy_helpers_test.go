package mtop

// 本文件保存只被测试使用的历史薄包装：生产代码已改走带凭证/租约的正式入口，测试仍通过它们覆盖同一实现。

// IsCredentialRefreshableErr 保留历史分类入口；Token 由 MTOP 客户端内部刷新，只有 Session 失效允许账号级恢复。
func IsCredentialRefreshableErr(err error) bool {
	return IsSessionExpiredErr(err)
}
