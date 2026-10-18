package providers

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"

	"shanhaigateway/internal/service/auth"
)

// AdminName 注册表里的管理员登录标识。
const AdminName = "admin"

// AdminProvider 管理员密码登录。
// pwd 是启动配置（ADMIN_PASSWORD），恒定时间比较防逐字节试探。
type AdminProvider struct {
	svc *auth.Service
	pw  string
}

func (p AdminProvider) Name() string  { return AdminName }
func (p AdminProvider) Enabled() bool { return p.pw != "" }

func (p AdminProvider) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/auth/admin-login", p.handle)
}

func (p AdminProvider) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}
	// 恒定时间比较：防逐字节试探时序。公版原版是普通 !=，这里收紧。
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(p.pw)) != 1 {
		http.Error(w, `{"error":"wrong password"}`, http.StatusUnauthorized)
		return
	}
	token, userID, exp, err := p.svc.IssueSession(auth.Identity{
		Subject: "admin", Nickname: "Admin", IsAdmin: true,
		TTL: 30 * 24 * time.Hour,
	})
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"token": token, "user_id": userID, "nickname": "Admin", "expires_at": exp,
	})
}
