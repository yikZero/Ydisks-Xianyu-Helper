package browser

import "strings"

const (
	PasswordLoginStatusProcessing           = "processing"
	PasswordLoginStatusVerificationRequired = "verification_required"
	PasswordLoginStatusFailed               = "failed"
)

// PasswordLoginEvent 描述密码登录过程中的中间状态，供 HTTP 会话轮询接口展示。
type PasswordLoginEvent struct {
	Status          string
	Message         string
	Error           string
	Reason          string
	VerificationURL string
	ScreenshotPath  string
	QRCodeURL       string
	CooldownHours   int
}

// PasswordLoginEventHandler 接收密码登录过程中的阶段事件。
type PasswordLoginEventHandler func(PasswordLoginEvent)

// PasswordLoginEventFromError 把密码登录错误转换为失败事件；err 为 nil 时返回空事件。
func PasswordLoginEventFromError(err error) PasswordLoginEvent {
	if err == nil {
		return PasswordLoginEvent{}
	}
	return PasswordLoginEventFromMessage(err.Error())
}

// PasswordLoginEventFromMessage 根据错误文本识别风控、人脸验证等失败原因及对应冷却时长。
func PasswordLoginEventFromMessage(msg string) PasswordLoginEvent {
	lower := strings.ToLower(msg)
	event := PasswordLoginEvent{Status: PasswordLoginStatusFailed, Message: msg, Error: msg}
	switch {
	case IsBaxiaPunishMessage(msg) || (strings.Contains(msg, "风控") && strings.Contains(msg, "图形验证")):
		event.Reason = "baxia_punish_captcha"
		event.CooldownHours = 5
	case strings.Contains(msg, "人脸") || strings.Contains(lower, "verification") || strings.Contains(lower, "captcha"):
		event.Status = PasswordLoginStatusVerificationRequired
	}
	return event
}

// IsBaxiaPunishMessage 判断错误文本是否来自阿里 baxia 风控拦截或图形验证码页面。
func IsBaxiaPunishMessage(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "baxia") ||
		strings.Contains(lower, "punish") ||
		strings.Contains(lower, "scratch-captcha") ||
		strings.Contains(lower, "captcha-question") ||
		strings.Contains(lower, "scratch-captcha-container") ||
		strings.Contains(msg, "找两个") ||
		strings.Contains(msg, "松鼠")
}
