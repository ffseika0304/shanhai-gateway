package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"shanhai-gateway/internal/core/store"
)

// Service 会话服务：登录态的唯一管理者。
// 持有 store（不可变引用）—— 不再依赖包级 db 变量。
type Service struct {
	st *store.Store
}

// NewService 组装会话服务；store 不接受 nil（nil 会把 panic 推迟到第一个请求）。
func NewService(st *store.Store) (*Service, error) {
	if st == nil {
		return nil, errors.New("auth: store 为 nil，无法组装 Service")
	}
	return &Service{st: st}, nil
}

// IssueSession 把一个 Identity 落库并换出 session token。
// 照搬 issueSession：upsert 用户 → 生成 token → 写 sessions 表。
// 首次登录默认额度 1000 这一策略在 store.UpsertUser（公版原逻辑），不在这里。
func (s *Service) IssueSession(id Identity) (token string, userID int64, expiresAt int64, err error) {
	userID, err = s.st.UpsertUser(id.Subject, id.Nickname)
	if err != nil {
		return "", 0, 0, err
	}
	ttl := id.TTL
	if ttl <= 0 {
		ttl = defaultSessionTTL
	}
	token = GenerateToken()
	expiresAt = time.Now().Add(ttl).Unix()
	if err := s.st.CreateSession(token, userID, float64(expiresAt), float64(time.Now().Unix())); err != nil {
		return "", 0, 0, err
	}
	return token, userID, expiresAt, nil
}

// ValidateSession 校验 token 并返回 user_id；过期即删（公版原语义）。
// 不存在/过期/伪造一律 (0, false)，调用方按未登录处理。
func (s *Service) ValidateSession(token string) (int64, bool) {
	userID, expires, err := s.st.FindSession(token)
	if err != nil {
		return 0, false
	}
	if time.Now().Unix() > int64(expires) {
		s.st.DeleteSession(token)
		return 0, false
	}
	return userID, true
}

// GenerateToken 生成 32 字节随机 token（256 bit）。
// 原 auth.go 的 generateToken，导出化：providers 与用户中心生成 sk-user-* key
// 都要用同一套随机源 —— 各处私有实现会导致 token 熵参不齐。
func GenerateToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// AuthMiddleware 保护 /api/user/* 这类「用户中心」路由。
// 原 authMiddleware（挂在 Service 上之后行为逐位一致）。
//
// ⚠️ **只认 header，不认 `?token=` query** —— query 会进 access log / 代理日志 /
// 浏览器历史，是 token 泄漏的常见路径（公版原实现认 query，这里刻意收紧，10-01
// 那处收紧必须保住）。需要在浏览器里直接访问的用户中心请走 cookie 或 Bearer，
// 不要走 query。
func (s *Service) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("X-Session-Token")
		if token == "" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		userID, ok := s.ValidateSession(token)
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		r.Header.Set("X-User-ID", fmt.Sprintf("%d", userID))
		next(w, r)
	}
}
