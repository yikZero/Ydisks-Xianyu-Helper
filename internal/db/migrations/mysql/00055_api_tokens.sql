-- +goose Up
-- API Token：供 MCP 等外部客户端以用户身份调用管理接口。只保存 SHA-256 摘要，明文仅在创建时显示一次。
CREATE TABLE IF NOT EXISTS api_tokens (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id BIGINT NOT NULL,
    name VARCHAR(255) NOT NULL,
    token_hash CHAR(64) NOT NULL,
    token_prefix VARCHAR(32) NOT NULL,
    scope VARCHAR(16) NOT NULL DEFAULT 'read',
    created_at BIGINT NOT NULL,
    last_used_at BIGINT NOT NULL DEFAULT 0,
    revoked_at BIGINT NOT NULL DEFAULT 0,
    UNIQUE KEY uk_api_tokens_hash (token_hash),
    KEY idx_api_tokens_user (user_id),
    CONSTRAINT fk_api_tokens_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- +goose Down
DROP TABLE IF EXISTS api_tokens;
