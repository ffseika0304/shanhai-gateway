// usercenter.go —— /api/user/* 用户中心（自公版 user.go **原样搬入**）。
//
// 唯一的改动：原文里全部走包级 `db` 全局变量的 SQL（GET me / keys 列表 /
// 生成 / 删除），现在都已经是 *store.Store 的方法（internal/core/store/users.go，
// SQL 一字未动），HTTP 形状（掩码、状态码、错误文案）保持与公版逐位一致。
//
// 掩码规则（key[:12]+"..."+key[-4:]）是展示层的决定，所以留在这里 ——
// 原 user.go 的注释也这么划界（见 store.UserKey）。
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"shanhai-gateway/internal/service/auth"
)

// userIDFrom 取鉴权层（auth.Service.AuthMiddleware）写入的 X-User-ID。
func userIDFrom(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.Header.Get("X-User-ID"), 10, 64)
}

// handleGetUser GET /api/user/me
func (h *Handlers) handleGetUser(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFrom(r)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	u, err := h.Store.GetUser(userID)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"user_id":   u.ID,
		"nickname":  u.Nickname,
		"avatar":    u.Avatar,
		"quota":     u.Quota,
		"used":      u.Used,
		"remaining": u.Quota - u.Used,
	})
}

// handleListKeys GET /api/user/keys
func (h *Handlers) handleListKeys(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFrom(r)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	keys, err := h.Store.ListKeys(userID)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}

	var out []map[string]interface{}
	for _, k := range keys {
		masked := k.Key
		if len(k.Key) > 16 {
			masked = k.Key[:12] + "..." + k.Key[len(k.Key)-4:]
		}
		out = append(out, map[string]interface{}{
			"id":         k.ID,
			"key_masked": masked,
			"name":       k.Name,
			"enabled":    k.Enabled,
			"created_at": k.CreatedAt,
		})
	}
	json.NewEncoder(w).Encode(out)
}

// handleGenerateKey POST /api/user/keys/generate
func (h *Handlers) handleGenerateKey(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFrom(r)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Name == "" {
		req.Name = "default"
	}

	count, err := h.Store.CountKeys(userID)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	if count >= 5 {
		http.Error(w, `{"error":"max 5 keys per user"}`, http.StatusBadRequest)
		return
	}

	key := "sk-user-" + auth.GenerateToken()[:32]
	if err := h.Store.GenerateKey(userID, key, req.Name); err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}

	json.NewEncoder(w).Encode(map[string]string{"key": key, "name": req.Name})
}

// handleDeleteKey DELETE /api/user/keys/{id}
func (h *Handlers) handleDeleteKey(w http.ResponseWriter, r *http.Request) {
	userID, err := userIDFrom(r)
	if err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	keyID := strings.TrimPrefix(r.URL.Path, "/api/user/keys/")
	if keyID == "" {
		http.Error(w, `{"error":"missing key id"}`, http.StatusBadRequest)
		return
	}
	if err := h.Store.DeleteKey(keyID, userID); err != nil {
		http.Error(w, `{"error":"db error"}`, http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
