package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"

	"xianyu-go/internal/db"
	"xianyu-go/internal/logsafe"
)

// send 根据渠道类型将已格式化通知发送到外部服务；渠道配置错误直接返回供 outbox 重试分类。
func (n *Notifier) send(ch db.NotificationChannel, message string) error {
	cfg := parseConfig(ch.Config)
	switch ch.Type {
	case "ding_talk", "dingtalk":
		return n.sendDingTalk(cfg, message)
	case "feishu", "lark":
		return n.sendFeishu(cfg, message)
	case "bark":
		return n.sendBark(cfg, message)
	case "webhook":
		return n.sendWebhook(cfg, message)
	case "wechat":
		return n.sendWeChat(cfg, message)
	case "telegram":
		return n.sendTelegram(cfg, message)
	case "email":
		return n.sendEmailWithUser(cfg, message, ch.UserID)
	case "qq":
		// QQ 渠道配置未标准化，跳过。
		return fmt.Errorf("qq 渠道暂不支持")
	default:
		return fmt.Errorf("不支持的通知渠道类型: %s", ch.Type)
	}
}

// parseConfig 解析 config JSON，失败时兼容旧格式 {"config": <raw>}。
func parseConfig(config string) map[string]any {
	if config == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(config), &m); err != nil {
		return map[string]any{"config": config}
	}
	return m
}

// ---- 钉钉 ----
func (n *Notifier) sendDingTalk(cfg map[string]any, message string) error {
	webhook := strOr(cfg, "webhook_url", strOr(cfg, "config", ""))
	secret := strOr(cfg, "secret", "")
	if webhook == "" {
		return fmt.Errorf("钉钉 webhook_url 为空")
	}
	if secret != "" {
		ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
		stringToSign := ts + "\n" + secret
		h := hmac.New(sha256.New, []byte(secret))
		h.Write([]byte(stringToSign))
		sign := base64.StdEncoding.EncodeToString(h.Sum(nil))
		parsed, err := url.Parse(webhook)
		if err != nil {
			return notificationRequestError(err)
		}
		query := parsed.Query()
		query.Set("timestamp", ts)
		query.Set("sign", sign)
		parsed.RawQuery = query.Encode()
		webhook = parsed.String()
	}
	payload := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]any{
			"title": "闲鱼自动回复通知",
			"text":  message,
		},
	}
	return n.postJSON(webhook, payload)
}

// ---- 飞书 ----
func (n *Notifier) sendFeishu(cfg map[string]any, message string) error {
	webhook := strOr(cfg, "webhook_url", "")
	secret := strOr(cfg, "secret", "")
	if webhook == "" {
		return fmt.Errorf("飞书 webhook_url 为空")
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	data := map[string]any{
		"msg_type":  "text",
		"content":   map[string]any{"text": message},
		"timestamp": ts,
	}
	if secret != "" {
		stringToSign := ts + "\n" + secret
		h := hmac.New(sha256.New, []byte(stringToSign))
		h.Write([]byte(""))
		data["sign"] = base64.StdEncoding.EncodeToString(h.Sum(nil))
	}
	return n.postJSON(webhook, data)
}

// ---- Bark ----
func (n *Notifier) sendBark(cfg map[string]any, message string) error {
	server := strings.TrimRight(strOr(cfg, "server_url", "https://api.day.app"), "/")
	deviceKey := strOr(cfg, "device_key", "")
	if deviceKey == "" {
		return fmt.Errorf("bark device_key 为空")
	}
	data := map[string]any{
		"device_key": deviceKey,
		"title":      strOr(cfg, "title", "闲鱼自动回复通知"),
		"body":       message,
		"sound":      strOr(cfg, "sound", "default"),
		"group":      strOr(cfg, "group", "xianyu"),
	}
	if icon := strOr(cfg, "icon", ""); icon != "" {
		data["icon"] = icon
	}
	if u := strOr(cfg, "url", ""); u != "" {
		data["url"] = u
	}
	return n.postJSON(server+"/push", data)
}

// ---- Webhook ----
func (n *Notifier) sendWebhook(cfg map[string]any, message string) error {
	webhook := strOr(cfg, "webhook_url", "")
	if webhook == "" {
		return fmt.Errorf("webhook_url 为空")
	}
	method := strings.ToUpper(strOr(cfg, "http_method", "POST"))
	headers := map[string]any{}
	if h := strOr(cfg, "headers", ""); h != "" {
		_ = json.Unmarshal([]byte(h), &headers)
	}
	data := map[string]any{
		"message":   message,
		"timestamp": time.Now().Format("2006-01-02 15:04:05"),
		"source":    "xianyu-auto-reply",
	}
	body, _ := json.Marshal(data)
	req, err := http.NewRequest(method, webhook, bytes.NewReader(body))
	if err != nil {
		return notificationRequestError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, fmt.Sprintf("%v", v))
	}
	resp, err := n.httpc.Do(req)
	if err != nil {
		return notificationRequestError(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook 状态码 %d", resp.StatusCode)
	}
	return nil
}

// ---- 企业微信 ----
func (n *Notifier) sendWeChat(cfg map[string]any, message string) error {
	webhook := strOr(cfg, "webhook_url", "")
	if webhook == "" {
		return fmt.Errorf("微信 webhook_url 为空")
	}
	return n.postJSON(webhook, map[string]any{
		"msgtype": "text",
		"text":    map[string]any{"content": message},
	})
}

// ---- Telegram ----
func (n *Notifier) sendTelegram(cfg map[string]any, message string) error {
	botToken := strOr(cfg, "bot_token", "")
	chatID := strOr(cfg, "chat_id", "")
	if botToken == "" || chatID == "" {
		return fmt.Errorf("telegram bot_token/chat_id 不完整")
	}
	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	return n.postJSON(endpoint, map[string]any{
		"chat_id": chatID,
		"text":    message,
	})
}

// sendEmailWithUser 使用邮件渠道配置发送测试或业务通知，并按渠道所有者读取系统 SMTP 秘密。
func (n *Notifier) sendEmailWithUser(cfg map[string]any, message string, channelUserID int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	server := n.smtpConfigValue(ctx, cfg, "smtp_server", "", channelUserID)
	port := n.smtpConfigValue(ctx, cfg, "smtp_port", "587", channelUserID)
	user := n.smtpConfigValue(ctx, cfg, "smtp_user", "", channelUserID)
	pass := n.smtpConfigValue(ctx, cfg, "smtp_password", "", channelUserID)
	useTLS := parseConfigBool(n.smtpConfigValue(ctx, cfg, "smtp_use_tls", "true", channelUserID), true)
	useSSL := parseConfigBool(n.smtpConfigValue(ctx, cfg, "smtp_use_ssl", "false", channelUserID), false)
	fromAddress := n.smtpConfigValue(ctx, cfg, "smtp_from_address", "", channelUserID)
	fromName := n.smtpConfigValue(ctx, cfg, "smtp_from_name", "", channelUserID)
	legacyFrom := n.smtpConfigValue(ctx, cfg, "smtp_from", "", channelUserID)
	to := strOr(cfg, "to_email", strOr(cfg, "email", ""))
	if server == "" || user == "" || to == "" {
		return fmt.Errorf("邮件配置不完整：请配置系统 SMTP 或在邮件渠道中覆盖 SMTP，并填写收件邮箱")
	}
	if legacyFrom != "" {
		if parsed, err := mail.ParseAddress(legacyFrom); err == nil && strings.Contains(parsed.Address, "@") {
			if fromAddress == "" {
				fromAddress = parsed.Address
			}
			if fromName == "" {
				fromName = parsed.Name
			}
		} else if fromName == "" {
			fromName = legacyFrom
		}
	}
	if fromAddress == "" {
		fromAddress = user
	}
	from, err := mail.ParseAddress(fromAddress)
	if err != nil || !strings.Contains(from.Address, "@") {
		return fmt.Errorf("发件邮箱地址无效")
	}
	recipient, err := mail.ParseAddress(to)
	if err != nil || !strings.Contains(recipient.Address, "@") {
		return fmt.Errorf("收件邮箱地址无效")
	}
	from.Name = fromName
	fromHeader := from.Address
	if from.Name != "" {
		fromHeader = from.String()
	}
	toHeader := recipient.Address
	if recipient.Name != "" {
		toHeader = recipient.String()
	}
	addr := server + ":" + port
	// auth 在单次 SMTP 会话内协商认证机制，保留 TLS 与服务器身份校验。
	auth := newSMTPAuth(user, pass, server)
	msg := strings.Join([]string{
		"From: " + fromHeader,
		"To: " + toHeader,
		"Subject: =?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte("闲鱼自动发货通知")) + "?=",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		message,
	}, "\r\n")
	return sendPublicSMTP(ctx, addr, server, auth, from.Address, recipient.Address, []byte(msg), smtpTransportOptions{
		UseSTARTTLS:    useTLS && !useSSL,
		UseImplicitTLS: useSSL,
	})
}

type smtpTransportOptions struct {
	UseSTARTTLS    bool
	UseImplicitTLS bool
}

func sendPublicSMTP(ctx context.Context, addr, server string, auth smtp.Auth, from, to string, message []byte, options ...smtpTransportOptions) error {
	_, rawPort, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("SMTP 地址无效")
	}
	port, err := strconv.Atoi(strings.TrimSpace(rawPort))
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("SMTP 端口无效")
	}
	conn, err := dialPublicSMTP(ctx, "tcp", addr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("连接 SMTP 服务器失败: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	transport := smtpTransportOptions{UseSTARTTLS: true}
	if len(options) > 0 {
		transport = options[0]
	}
	if transport.UseImplicitTLS {
		tlsConn := tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS12, ServerName: server})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("SMTP SSL 握手失败: %w", err)
		}
		conn = tlsConn
	}
	client, err := smtp.NewClient(conn, server)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if transport.UseSTARTTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return fmt.Errorf("SMTP 服务器不支持要求的 STARTTLS")
		}
		if err := client.StartTLS(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: server}); err != nil {
			return err
		}
	}
	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(message); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func parseConfigBool(raw string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func (n *Notifier) configOrSetting(ctx context.Context, cfg map[string]any, key, fallbackValue string, userIDs ...int64) string {
	if v := strings.TrimSpace(strOr(cfg, key, "")); v != "" {
		return v
	}
	// value 表示渠道配置缺失时从系统设置解析出的最终候选值。
	if value := n.settingValue(ctx, key, userIDs...); value != "" {
		return value
	}
	return fallbackValue
}

// settingValue 读取系统设置；存在渠道所有者时优先走带审计的用户作用域端口。
func (n *Notifier) settingValue(ctx context.Context, key string, userIDs ...int64) string {
	if n == nil || n.repository == nil {
		return ""
	}
	if len(userIDs) > 0 && userIDs[0] > 0 {
		// scoped 表示支持用户作用域读取的仓储；ok 表示当前仓储是否实现该能力。
		if scoped, ok := n.repository.(userScopedSettings); ok {
			// value 是用户作用域设置值；err 是读取设置或审计失败信息。
			if value, err := scoped.GetSettingForUser(ctx, userIDs[0], key); err == nil && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	// value 是兼容旧通知器仓储读取到的设置值；err 是读取失败信息。
	if value, err := n.repository.GetSetting(ctx, key); err == nil {
		return strings.TrimSpace(value)
	}
	return ""
}

// smtpConfigValue keeps legacy per-field fallback behavior for existing rows,
// while new rows use an explicit all-system or all-channel SMTP mode.
func (n *Notifier) smtpConfigValue(ctx context.Context, cfg map[string]any, key, fallbackValue string, userIDs ...int64) string {
	modeValue, hasExplicitMode := cfg["use_custom_smtp"]
	if !hasExplicitMode {
		return n.configOrSetting(ctx, cfg, key, fallbackValue, userIDs...)
	}
	if parseConfigBool(fmt.Sprintf("%v", modeValue), false) {
		if value := strings.TrimSpace(strOr(cfg, key, "")); value != "" {
			return value
		}
		return fallbackValue
	}
	// value 表示系统 SMTP 模式下解析出的设置值。
	if value := n.settingValue(ctx, key, userIDs...); value != "" {
		return value
	}
	return fallbackValue
}

// postJSON 通用 JSON POST。
func (n *Notifier) postJSON(url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// requestCtx、requestCancel 为单次渠道 HTTP 请求创建有界取消路径。
	requestCtx, requestCancel := context.WithTimeout(context.Background(), legacyNotifierOperationTimeout)
	defer requestCancel()
	// req、err 分别是携带取消语义的渠道 HTTP 请求及其构造错误。
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return notificationRequestError(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.httpc.Do(req)
	if err != nil {
		return notificationRequestError(err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("状态码 %d", resp.StatusCode)
	}
	if err := notificationBusinessError(responseBody); err != nil {
		return err
	}
	return nil
}

// notificationRequestError 将外部通知请求错误转换为不含 URL 路径凭证的内部诊断错误。
// err 是 net/http、URL 解析或传输层返回的原始错误；返回值不得再暴露其完整请求地址。
func notificationRequestError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("通知渠道请求失败: %s", logsafe.ExternalError(err))
}

func notificationBusinessError(body []byte) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	message := strings.TrimSpace(firstMapString(payload, "errmsg", "msg", "message", "description"))
	if code, ok := mapNumber(payload, "errcode"); ok && code != 0 {
		return fmt.Errorf("通知渠道返回错误 %.0f: %s", code, message)
	}
	if code, ok := mapNumber(payload, "StatusCode"); ok && code != 0 {
		return fmt.Errorf("通知渠道返回错误 %.0f: %s", code, message)
	}
	if code, ok := mapNumber(payload, "code"); ok && code != 0 && code != 200 {
		return fmt.Errorf("通知渠道返回错误 %.0f: %s", code, message)
	}
	if okValue, exists := payload["ok"].(bool); exists && !okValue {
		return fmt.Errorf("通知渠道返回失败: %s", message)
	}
	return nil
}

func mapNumber(payload map[string]any, key string) (float64, bool) {
	value, ok := payload[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		return typed, true
	case string:
		number, err := strconv.ParseFloat(typed, 64)
		return number, err == nil
	default:
		return 0, false
	}
}

func firstMapString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && value != "" {
			return value
		}
	}
	return "未知错误"
}

// strOr 从 map 取字符串，缺失返回 fallback。
func strOr(m map[string]any, key, fallback string) string {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case string:
			return x
		default:
			return fmt.Sprintf("%v", x)
		}
	}
	return fallback
}

func fallback(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
