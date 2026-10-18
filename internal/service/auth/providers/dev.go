package providers

import (
	"encoding/json"
	"net/http"
	"time"

	"shanhaigateway/internal/service/auth"
)

// DevName 注册表里的开发登录标识。
const DevName = "dev"

// DevProvider 开发登录：每次生成一个全新 subject = 每次都是一个新用户。
// 仅 DEV_MODE=1 时启用，绝不与生产混用（enabled 由 env 在接线时定性）。
type DevProvider struct {
	svc     *auth.Service
	enabled bool
}

func (p DevProvider) Name() string  { return DevName }
func (p DevProvider) Enabled() bool { return p.enabled }

func (p DevProvider) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/auth/dev-login", func(w http.ResponseWriter, r *http.Request) {
		// 每次生成一个全新 subject = 每次都是一个新用户，不复用旧额度。
		token, userID, exp, err := p.svc.IssueSession(auth.Identity{
			Subject:  "dev_openid_" + auth.GenerateToken()[:8],
			Nickname: "Dev",
			TTL:      7 * 24 * time.Hour,
		})
		if err != nil {
			http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"token": token, "user_id": userID, "expires_at": exp,
		})
	})
}
