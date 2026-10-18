const smtpOverrideKeys = [
  'smtp_server',
  'smtp_port',
  'smtp_user',
  'smtp_password',
  'smtp_from_name',
  'smtp_from_address',
  'smtp_use_tls',
  'smtp_use_ssl',
] as const;

const parseBoolean = (value: unknown, fallback: boolean): boolean => {
  if (typeof value === 'boolean') return value;
  if (typeof value === 'string') {
    if (['true', '1', 'yes', 'on'].includes(value.toLowerCase())) return true;
    if (['false', '0', 'no', 'off'].includes(value.toLowerCase())) return false;
  }
  return fallback;
};

// normalizeEmailChannelConfig 补齐邮件渠道的 use_custom_smtp：未显式设置时，只要存在旧 SMTP 字段就视为自定义 SMTP。
export const normalizeEmailChannelConfig = (config: Record<string, unknown>): Record<string, unknown> => {
  const hasExplicitMode = Object.prototype.hasOwnProperty.call(config, 'use_custom_smtp');
  // hasLegacyOverrides 表示旧 SMTP 字段中是否仍有非空值，以便兼容层推断配置模式。
  const hasLegacyOverrides = smtpOverrideKeys.some(key => String(config[key] ?? '').trim() !== '' /* key 是当前检查的旧 SMTP 字段名。 */);
  return {
    ...config,
    use_custom_smtp: hasExplicitMode
      ? parseBoolean(config.use_custom_smtp, false)
      : hasLegacyOverrides,
  };
};

// enableCustomSMTP 切换为自定义 SMTP，空字段用系统 SMTP 设置填充并补齐端口、发件人和加密默认值。
export const enableCustomSMTP = (
  config: Record<string, unknown>,
  systemSettings: Record<string, unknown>,
): Record<string, unknown> => {
  const next: Record<string, unknown> = { ...config, use_custom_smtp: true };
  for (const key of smtpOverrideKeys) {
    if (String(next[key] ?? '').trim() === '' && systemSettings[key] !== undefined) {
      next[key] = systemSettings[key];
    }
  }
  next.smtp_port ||= 587;
  next.smtp_from_address ||= next.smtp_user || '';
  next.smtp_use_tls = parseBoolean(next.smtp_use_tls, true);
  next.smtp_use_ssl = parseBoolean(next.smtp_use_ssl, false);
  return next;
};

// buildEmailChannelConfig 生成提交给后端的邮件渠道配置；仅在自定义 SMTP 时携带 SMTP 字段。
export const buildEmailChannelConfig = (config: Record<string, unknown>): Record<string, unknown> => {
  const normalized = normalizeEmailChannelConfig(config);
  const result: Record<string, unknown> = {
    to_email: String(normalized.to_email ?? '').trim(),
    use_custom_smtp: normalized.use_custom_smtp === true,
  }; /* result 表示处理结果。 */
  if (result.use_custom_smtp) {
    for (const key of smtpOverrideKeys) result[key] = normalized[key];
  }
  return result;
};
