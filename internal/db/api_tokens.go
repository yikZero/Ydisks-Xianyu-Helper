package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// APITokenPrefix 是明文 API Token 的固定前缀，便于在日志或配置中识别用途；摘要计算包含该前缀。
const APITokenPrefix = "ydx_"

const (
	// APITokenScopeRead 只允许读取类调用。
	APITokenScopeRead = "read"
	// APITokenScopeWrite 允许读取和修改类调用。
	APITokenScopeWrite = "write"
)

// ErrInvalidAPITokenScope 表示创建 Token 时传入了未知权限范围。
var ErrInvalidAPITokenScope = errors.New("API Token 权限范围只能是 read 或 write")

// APIToken 是不含明文与摘要的 Token 元数据，可安全展示给所有者。
type APIToken struct {
	ID         int64
	UserID     int64
	Name       string
	Prefix     string
	Scope      string
	CreatedAt  int64
	LastUsedAt int64
	RevokedAt  int64
}

// APITokenIdentity 是 Token 校验通过后可用于授权的用户身份。
type APITokenIdentity struct {
	TokenID  int64
	UserID   int64
	Username string
	IsAdmin  bool
	Scope    string
}

// APITokens 读写 api_tokens 表；只保存 SHA-256 摘要，明文 Token 仅在 Create 返回时出现一次。
type APITokens struct {
	DB      *sql.DB
	Dialect Dialect
}

// hashAPIToken 返回明文 Token 的十六进制 SHA-256 摘要。Token 由 32 字节随机数生成，无需加盐或慢哈希。
func hashAPIToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Create 为 userID 生成新 Token 并返回明文（只此一次）与元数据。
func (t *APITokens) Create(ctx context.Context, userID int64, name, scope string) (string, *APIToken, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil, errors.New("API Token 名称不能为空")
	}
	if scope != APITokenScopeRead && scope != APITokenScopeWrite {
		return "", nil, ErrInvalidAPITokenScope
	}
	secret, err := randomSessionID()
	if err != nil {
		return "", nil, fmt.Errorf("生成 API Token: %w", err)
	}
	plain := APITokenPrefix + secret
	token := &APIToken{UserID: userID, Name: name, Prefix: plain[:len(APITokenPrefix)+6], Scope: scope, CreatedAt: time.Now().Unix()}
	id, err := insertReturningID(ctx, t.DB, t.Dialect,
		`INSERT INTO api_tokens (user_id, name, token_hash, token_prefix, scope, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		userID, token.Name, hashAPIToken(plain), token.Prefix, scope, token.CreatedAt)
	if err != nil {
		return "", nil, fmt.Errorf("写入 API Token: %w", err)
	}
	token.ID = id
	return plain, token, nil
}

// List 返回 userID 的全部 Token 元数据（含已吊销），按创建顺序排列。
func (t *APITokens) List(ctx context.Context, userID int64) ([]APIToken, error) {
	rows, err := t.DB.QueryContext(ctx,
		`SELECT id, user_id, name, token_prefix, scope, created_at, last_used_at, revoked_at
		   FROM api_tokens WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := []APIToken{}
	for rows.Next() {
		var token APIToken
		if err := rows.Scan(&token.ID, &token.UserID, &token.Name, &token.Prefix, &token.Scope, &token.CreatedAt, &token.LastUsedAt, &token.RevokedAt); err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// Revoke 吊销 userID 名下的 tokenID；Token 不存在、不属于该用户或已吊销时返回 ErrNotFound。
func (t *APITokens) Revoke(ctx context.Context, userID, tokenID int64) error {
	res, err := t.DB.ExecContext(ctx,
		`UPDATE api_tokens SET revoked_at=? WHERE id=? AND user_id=? AND revoked_at=0`, time.Now().Unix(), tokenID, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// apiTokenTouchInterval 是刷新 last_used_at 的最小间隔，避免每次调用都写库。
const apiTokenTouchInterval = 60

// Authenticate 校验明文 Token，返回所属的已激活用户身份并按分钟粒度记录最近使用时间。
// Token 不存在、已吊销或用户已停用时返回 ErrNotFound。
func (t *APITokens) Authenticate(ctx context.Context, plain string) (*APITokenIdentity, error) {
	plain = strings.TrimSpace(plain)
	if !strings.HasPrefix(plain, APITokenPrefix) {
		return nil, ErrNotFound
	}
	var identity APITokenIdentity
	var isAdmin int
	var lastUsedAt int64
	err := t.DB.QueryRowContext(ctx,
		`SELECT t.id, t.user_id, u.username, u.is_admin, t.scope, t.last_used_at
		   FROM api_tokens t
		   JOIN users u ON u.id=t.user_id
		  WHERE t.token_hash=? AND t.revoked_at=0 AND u.is_active=1`,
		hashAPIToken(plain)).Scan(&identity.TokenID, &identity.UserID, &identity.Username, &isAdmin, &identity.Scope, &lastUsedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	identity.IsAdmin = isAdmin != 0
	// 最近使用时间只用于展示，写入失败不影响本次认证。
	if now := time.Now().Unix(); now-lastUsedAt >= apiTokenTouchInterval {
		_, _ = t.DB.ExecContext(ctx, `UPDATE api_tokens SET last_used_at=? WHERE id=?`, now, identity.TokenID)
	}
	return &identity, nil
}
