// package auth_test 所以放外面：测试要 blank import providers 子包以验证
// 注册/接线全链路 —— 内部测试包会构成 import 环。
package auth_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shanhai-gateway/internal/core/store"
	"shanhai-gateway/internal/service/auth"
	_ "shanhai-gateway/internal/service/auth/providers"
)

func newTestService(t *testing.T) *auth.Service {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/auth.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	svc, err := auth.NewService(s)
	if err != nil {
		t.Fatalf("auth.NewService: %v", err)
	}
	return svc
}

// wiredMux 按 env 接线（模拟 main 启动流程）。
func wiredMux(t *testing.T, svc *auth.Service) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	svc.RegisterEnabledLogins(mux)
	return mux
}

func postJSON(t *testing.T, mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)
	return resp
}

// loginAndGetToken 调 admin-login 拿到（且校验过会话存在的）token。
func loginAndGetToken(t *testing.T, mux *http.ServeMux, svc *auth.Service) string {
	t.Helper()
	resp := postJSON(t, mux, "/api/auth/admin-login", `{"password":"pw"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("admin login = %d", resp.Code)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil || out.Token == "" {
		t.Fatalf("decode token: %v %q", err, resp.Body.String())
	}
	return out.Token
}

func TestAdminLogin(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "s3cret")
	t.Setenv("DEV_MODE", "")
	svc := newTestService(t)
	mux := wiredMux(t, svc)

	// 正确密码 → 200 + 会话
	resp := postJSON(t, mux, "/api/auth/admin-login", `{"password":"s3cret"}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("admin login = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	var out struct {
		Token     string `json:"token"`
		UserID    int64  `json:"user_id"`
		Nickname  string `json:"nickname"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Token == "" || out.UserID == 0 || out.Nickname != "Admin" {
		t.Errorf("登录响应缺字段: %+v", out)
	}
	// token 确实可用会话
	uid, ok := svc.ValidateSession(out.Token)
	if !ok || uid != out.UserID {
		t.Errorf("token 未生效: uid=%d ok=%v", uid, ok)
	}

	// 错误密码 → 401
	resp = postJSON(t, mux, "/api/auth/admin-login", `{"password":"wrong"}`)
	if resp.Code != http.StatusUnauthorized {
		t.Errorf("错误密码应 401, got %d", resp.Code)
	}
	// 坏请求体 → 400
	resp = postJSON(t, mux, "/api/auth/admin-login", `{`)
	if resp.Code != http.StatusBadRequest {
		t.Errorf("坏请求应 400, got %d", resp.Code)
	}
}

func TestAdminDisabledWithoutPassword(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "")
	svc := newTestService(t)
	mux := wiredMux(t, svc)
	if code := postJSON(t, mux, "/api/auth/admin-login", `{"password":"x"}`).Code; code != http.StatusNotFound {
		t.Errorf("未配置密码时 admin 路由不该被挂载, got %d", code)
	}
}

func TestDevLoginToggle(t *testing.T) {
	// DEV_MODE=1 → 可用
	t.Setenv("DEV_MODE", "1")
	t.Setenv("ADMIN_PASSWORD", "")
	svc := newTestService(t)
	mux := wiredMux(t, svc)

	resp := postJSON(t, mux, "/api/auth/dev-login", ``)
	if resp.Code != http.StatusOK {
		t.Fatalf("dev login = %d, want 200; body=%s", resp.Code, resp.Body.String())
	}
	var out struct {
		Token     string `json:"token"`
		UserID    int64  `json:"user_id"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Token == "" || out.UserID == 0 {
		t.Errorf("dev login 响应缺字段: %+v", out)
	}

	// DEV_MODE 未设 → 路由不存在（404，正是「未启用不挂路由」的直接后果）
	t.Setenv("DEV_MODE", "")
	mux = wiredMux(t, svc)
	if code := postJSON(t, mux, "/api/auth/dev-login", ``).Code; code != http.StatusNotFound {
		t.Errorf("DEV_MODE 关闭后 dev 路由应 404, got %d", code)
	}
}

func TestAuthMiddlewareHeaderOnly(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "pw")
	svc := newTestService(t)
	mux := wiredMux(t, svc)
	token := loginAndGetToken(t, mux, svc)

	mw := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	want := svc.AuthMiddleware(mw)

	// 无 token → 401
	req := httptest.NewRequest(http.MethodGet, "/api/user/me", nil)
	resp := httptest.NewRecorder()
	want(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Errorf("无 token 应 401, got %d", resp.Code)
	}
	// header token → 过
	req = httptest.NewRequest(http.MethodGet, "/api/user/me", nil)
	req.Header.Set("X-Session-Token", token)
	resp = httptest.NewRecorder()
	want(resp, req)
	if resp.Code != http.StatusOK {
		t.Errorf("header token 应通过, got %d", resp.Code)
	}
	if got := req.Header.Get("X-User-ID"); got == "" {
		t.Error("中间件没注入 X-User-ID")
	}
	// query 里的 ?token= 不予承认（token 泄漏路径），即使 token 本身有效也 401
	req = httptest.NewRequest(http.MethodGet, "/api/user/me?token="+token, nil)
	resp = httptest.NewRecorder()
	want(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Errorf("只认 header 不认 query: query token 应 401, got %d", resp.Code)
	}
}
