package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ── 控制面：users / sessions / user_api_keys ──────────────────────
// 全部搬自公版（db.go 建表 + auth.go/user.go 的内联 SQL），SQL 语句一字未动，
// 只是 db.Exec(...) → *Store 的方法。HTTP 形状（掩码、状态码）不在这里 ——
// 那属于 api 层。

// User 控制面用户（对应 users 表）。
type User struct {
	ID       int64  `json:"user_id"`
	Nickname string `json:"nickname"`
	Avatar   string `json:"avatar"`
	Quota    int64  `json:"quota"`
	Used     int64  `json:"used"`
}

// UserKey 用户的下游 API Key（对应 user_api_keys 表）。
// Key 返回明文：掩码是展示层的决定（user.go 原来 key[:12]+"..."+key[-4:]），
// 存储层不该替调用方决定给不给看。
type UserKey struct {
	ID        int64
	Key       string
	Name      string
	Enabled   bool
	CreatedAt float64
}

// UpsertUser 按 subject 找用户，没有就建（默认额度 1000）。
// 照搬 auth.go upsertUser：openid 唯一、首次给 1000 额度、每次登录刷
// last_login_at；nickname 非空才覆盖。两次调用返回同一 id。
func (s *Store) UpsertUser(subject, nickname string) (int64, error) {
	var userID int64
	err := s.db.QueryRow("SELECT id FROM users WHERE openid = ?", subject).Scan(&userID)
	if err == sql.ErrNoRows {
		now := float64(time.Now().Unix())
		res, err := s.db.Exec(
			"INSERT INTO users(openid, nickname, quota, enabled, created_at, last_login_at) VALUES(?,?,?,1,?,?)",
			subject, nickname, 1000, now, now,
		)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	} else if err != nil {
		return 0, err
	}
	s.db.Exec("UPDATE users SET last_login_at = ? WHERE id = ?", float64(time.Now().Unix()), userID)
	if nickname != "" {
		s.db.Exec("UPDATE users SET nickname = ? WHERE id = ?", nickname, userID)
	}
	return userID, nil
}

// CreateSession 落一条会话。token 是唯一键（照搬公版 sessions 表）。
func (s *Store) CreateSession(token string, userID int64, expiresAt, createdAt float64) error {
	_, err := s.db.Exec("INSERT INTO sessions(token,user_id,expires_at,created_at) VALUES(?,?,?,?)",
		token, userID, expiresAt, createdAt)
	return err
}

// FindSession 查会话归属；不存在（含已删/未落库）返回 ErrSessionInvalid。
// 过期与否是 session 管理者的判断（auth 层），这里只读。
func (s *Store) FindSession(token string) (userID int64, expiresAt float64, err error) {
	err = s.db.QueryRow(
		"SELECT user_id, expires_at FROM sessions WHERE token = ?", token,
	).Scan(&userID, &expiresAt)
	if err == sql.ErrNoRows {
		return 0, 0, ErrSessionInvalid
	}
	return userID, expiresAt, err
}

// DeleteSession 删一条会话（过期清理 / 登出共用）。
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE token = ?", token)
	return err
}

// ErrSessionInvalid 表示查无此会话（区别于真 DB 故障，调用方按未登录处理）。
var ErrSessionInvalid = errors.New("store: session 不存在")

// GetUser 读用户（/api/user/me 用），不存在返回 ErrUserMissing。
func (s *Store) GetUser(userID int64) (User, error) {
	var u User
	err := s.db.QueryRow(
		"SELECT id, nickname, avatar_url, quota, used FROM users WHERE id = ?", userID,
	).Scan(&u.ID, &u.Nickname, &u.Avatar, &u.Quota, &u.Used)
	if err == sql.ErrNoRows {
		return User{}, fmt.Errorf("store: user %d: %w", userID, ErrUserMissing)
	}
	return u, err
}

// ErrUserMissing 表示查无此用户。
var ErrUserMissing = errors.New("store: user 不存在")

// CountKeys 数一个用户名下已有的 key 数（生成前的上限检查由调用方做，
// 上限 5 是 HTTP 层的策略，不属于存储）。
func (s *Store) CountKeys(userID int64) (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM user_api_keys WHERE user_id = ?", userID).Scan(&count)
	return count, err
}

// GenerateKey 为用户落一把新 key。enabled 固定 1（公版原语义）。
func (s *Store) GenerateKey(userID int64, key, name string) error {
	_, err := s.db.Exec(
		"INSERT INTO user_api_keys(user_id, key, name, enabled, created_at) VALUES(?,?,?,1,?)",
		userID, key, name, float64(time.Now().Unix()),
	)
	return err
}

// ListKeys 列一个用户的全部 key，新→旧（ORDER BY id DESC，公版原序）。
func (s *Store) ListKeys(userID int64) ([]UserKey, error) {
	rows, err := s.db.Query(
		"SELECT id, key, name, enabled, created_at FROM user_api_keys WHERE user_id = ? ORDER BY id DESC",
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []UserKey
	for rows.Next() {
		var k UserKey
		var enabled int
		if err := rows.Scan(&k.ID, &k.Key, &k.Name, &enabled, &k.CreatedAt); err != nil {
			return nil, err
		}
		k.Enabled = enabled == 1
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// DeleteKey 删指定 id 的 key，且必须属于该用户（WHERE id=? AND user_id=?，
// 公版原文）；不存在也不报错（幂等，公版 DELETE 后无条件返回 ok）。
func (s *Store) DeleteKey(keyID string, userID int64) error {
	_, err := s.db.Exec("DELETE FROM user_api_keys WHERE id = ? AND user_id = ?", keyID, userID)
	return err
}

// LookupUserKey 查某把用户 key 的归属与额度：key 与 user 必须都 enabled，
// 这是 sk-user-* 鉴权的判定源（SQL 为公版 KeyStore.Auth 内联原文）。
// 无匹配行 → ErrKeyUnknown，调用方（Keystore/Auth）按鉴权失败处理。
// quota<0 表示不限额。used>=quota 的判定在鉴权方（保持原归属，避免 store
// 与 Keystore 两处写同一策略）。
func (s *Store) LookupUserKey(key string) (userID, quota, used int64, err error) {
	err = s.db.QueryRow(`
		SELECT u.id, u.quota, u.used FROM user_api_keys k
		JOIN users u ON u.id = k.user_id
		WHERE k.key = ? AND k.enabled = 1 AND u.enabled = 1`, key,
	).Scan(&userID, &quota, &used)
	if err == sql.ErrNoRows {
		return 0, 0, 0, ErrKeyUnknown
	}
	return userID, quota, used, err
}

// ConsumeUserKey 成功响应后给用户 used+1（公版 Consume 的 sk-user 分支原文）。
func (s *Store) ConsumeUserKey(key string) error {
	_, err := s.db.Exec(
		"UPDATE users SET used = used + 1 WHERE id = (SELECT user_id FROM user_api_keys WHERE key = ?)", key)
	return err
}

// ErrKeyUnknown 表示查无此 key（或 key/user 被禁用）。
var ErrKeyUnknown = errors.New("store: user key 不存在或已禁用")
