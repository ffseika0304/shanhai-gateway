// dashauth_test.go —— 公网门两条过门路子（session cookie / 静态 key）的回归锁。
//
// 为什么值得写：门一旦漏，公网口上 /v1 的配额和 /admin 的并发就等于白送。
// 这几个用例把「错 key 必须 401」「DASH_TOKEN 配空 = 通道关闭」「换了 key
// 旧的必须失效」「cookie 老路子不许被新 key 挤掉」四件事钉死。
// slr 此前没有 Go 测试，这是第一个（2026-09-28）。

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shanhai-gateway/internal/service/proxy"
)

type gateResp struct {
	code int
	body string
	loc  string
}

// newGate 起一个装了哑 handler 的门，返回能直接打的服务器
func newGate(t *testing.T) *httptest.Server {
	t.Helper()
	h := dashGate(&Handlers{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// do 打一发；不跟随重定向（否则 302 会被自动跳成 /login 的 200，测不出 302）
func do(t *testing.T, method, url, bearer, cookie string, html bool) gateResp {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		req.Header.Add("Cookie", dashCookieName+"="+cookie)
	}
	if html {
		req.Header.Set("Accept", "text/html")
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return gateResp{code: resp.StatusCode, body: string(b), loc: resp.Header.Get("Location")}
}

// 静态 key 主路子：带对了直接过，不碰 cookie
func TestDashGateStaticKey(t *testing.T) {
	t.Setenv("DASH_TOKEN", dashTokenDefault)
	s := newGate(t)

	if r := do(t, "GET", s.URL+"/api/stats", dashTokenDefault, "", false); r.code != 200 || !strings.Contains(r.body, `"ok":true`) {
		t.Fatalf("对的 key 应放行，got %d %s", r.code, r.body)
	}
	if r := do(t, "POST", s.URL+"/v1/chat/completions", dashTokenDefault, "", false); r.code != 200 {
		t.Fatalf("POST 也应放行，got %d", r.code)
	}
	// 页面导航带 key 同样放行（key 是完整凭据，不只是 /v1 的门票）
	if r := do(t, "GET", s.URL+"/", dashTokenDefault, "", true); r.code != 200 {
		t.Fatalf("带 key 的页面导航应放行，got %d", r.code)
	}
}

// 错 key / 无凭据一律 401；页面导航无凭据仍 302 去登录页（老行为不许变）
func TestDashGateRejects(t *testing.T) {
	t.Setenv("DASH_TOKEN", dashTokenDefault)
	s := newGate(t)

	if r := do(t, "GET", s.URL+"/api/stats", "slr_00000000000000000000000000000000", "", false); r.code != 401 || !strings.Contains(r.body, "unauthorized") {
		t.Fatalf("错 key 应 401 JSON，got %d %s", r.code, r.body)
	}
	if r := do(t, "GET", s.URL+"/api/stats", "", "", false); r.code != 401 {
		t.Fatalf("裸请求应 401，got %d", r.code)
	}
	if r := do(t, "GET", s.URL+"/", "", "", true); r.code != 302 || !strings.Contains(r.loc, "/login") {
		t.Fatalf("裸页面导航应 302 /login，got %d loc=%s", r.code, r.loc)
	}
	// 非 Bearer 方案（Basic）不算数——只认 Bearer，免得被别的 scheme 骗过门
	req, _ := http.NewRequest("GET", s.URL+"/api/stats", nil)
	req.Header.Set("Authorization", "Basic "+dashTokenDefault)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("Basic 方案应 401，got %d", resp.StatusCode)
	}
}

// DASH_TOKEN 配成空串 = 整条通道关闭（systemd 里 `Environment=DASH_TOKEN=`）
func TestDashTokenDisabled(t *testing.T) {
	t.Setenv("DASH_TOKEN", "")
	s := newGate(t)

	if got := (&Handlers{}).dashToken(); got != "" {
		t.Fatalf("显式置空应得到空 key，got %q", got)
	}
	if r := do(t, "GET", s.URL+"/api/stats", dashTokenDefault, "", false); r.code != 401 {
		t.Fatalf("通道关闭后默认 key 也不该过门，got %d", r.code)
	}
	// 门关了，cookie 老路子必须照常工作
	sid := (&Handlers{}).dashNewSession()
	if r := do(t, "GET", s.URL+"/api/stats", "", sid, false); r.code != 200 {
		t.Fatalf("key 通道关闭不影响 cookie，got %d", r.code)
	}
	// 自取口要如实回报 enabled=false，前端才好显示「未启用」。
	// 这里直打 handler：测试用的门里挂的是哑 handler，没有注册真路由。
	rr := httptest.NewRecorder()
	(&Handlers{}).dashKeyHandler(rr, httptest.NewRequest("GET", "/api/dashkey", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"enabled":false`) {
		t.Fatalf("dashkey 应回报关闭态，got %d %s", rr.Code, rr.Body.String())
	}
}

// 换 key 后旧 key 必须立刻失效，且 cookie 老路子全程不受影响
func TestDashTokenRotationAndCookieLane(t *testing.T) {
	t.Setenv("DASH_TOKEN", "slr_rotated_value")
	s := newGate(t)

	if r := do(t, "GET", s.URL+"/api/stats", "slr_rotated_value", "", false); r.code != 200 {
		t.Fatalf("新 key 应放行，got %d", r.code)
	}
	if r := do(t, "GET", s.URL+"/api/stats", dashTokenDefault, "", false); r.code != 401 {
		t.Fatalf("换 key 后默认 key 应失效，got %d", r.code)
	}
	sid := (&Handlers{}).dashNewSession()
	if r := do(t, "GET", s.URL+"/api/stats", "", sid, false); r.code != 200 {
		t.Fatalf("cookie 通道应全程可用，got %d", r.code)
	}
	if r := do(t, "GET", s.URL+"/api/stats", "slr_rotated_value", sid, false); r.code != 200 {
		t.Fatalf("key + cookie 同时给也该过，got %d", r.code)
	}
}

// Bearer 解析：大小写、空格、非 Bearer 方案
func TestDashBearer(t *testing.T) {
	cases := []struct{ hdr, want string }{
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"},
		{"BEARER   abc  ", "abc"},
		{"Basic abc", ""},
		{"abc", ""},
		{"", ""},
		{"Bearer", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		if c.hdr != "" {
			r.Header.Set("Authorization", c.hdr)
		}
		if got := dashBearer(r); got != c.want {
			t.Errorf("dashBearer(%q) = %q, want %q", c.hdr, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 面板改 key（2026-10-19）：文件层解析 + POST 端点
// ---------------------------------------------------------------------------

// dashToken 解析优先级：env（含空串）> 持久化文件 > 内置默认。
func TestDashTokenResolution(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "dash-token")
	h := &Handlers{DashTokenFile: f}

	// env、文件都缺 → 内置默认
	if got := h.dashToken(); got != dashTokenDefault {
		t.Fatalf("env/文件都缺应回落内置默认，got %q", got)
	}
	// 文件有值（含首尾空白）→ 文件胜出且 trim
	if err := os.WriteFile(f, []byte("  file-key-1 \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := h.dashToken(); got != "file-key-1" {
		t.Fatalf("文件层应生效且 trim，got %q", got)
	}
	// env 显式设置 → 压过文件
	t.Setenv("DASH_TOKEN", "env-key")
	if got := h.dashToken(); got != "env-key" {
		t.Fatalf("env 应压过文件，got %q", got)
	}
	// env 空串 → 通道关闭（文件里的值也不救）
	t.Setenv("DASH_TOKEN", "")
	if got := h.dashToken(); got != "" {
		t.Fatalf("env 空串 = 关通道，got %q", got)
	}
	// env 撤掉、文件空内容 → 持久化关闭
	os.Unsetenv("DASH_TOKEN")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := h.dashToken(); got != "" {
		t.Fatalf("文件空内容 = 持久化关闭，got %q", got)
	}
	// 文件被删 → 回落内置默认
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	if got := h.dashToken(); got != dashTokenDefault {
		t.Fatalf("文件删除应回落内置默认，got %q", got)
	}
	// 文件路径未接线 → 依旧默认（旧配置兼容）
	if got := (&Handlers{}).dashToken(); got != dashTokenDefault {
		t.Fatalf("未接线应回落默认，got %q", got)
	}
}

// POST /api/dashkey 改 key：落盘 0600、即时生效、空串关通道、env 优先拒改。
func TestDashKeyChange(t *testing.T) {
	dir := t.TempDir()
	h := &Handlers{DashTokenFile: filepath.Join(dir, "dash-token")}
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.dashKeyHandler(rec, httptest.NewRequest(http.MethodPost, "/api/dashkey", strings.NewReader(body)))
		return rec
	}
	defer proxy.SetDynamicSecret("") // 清场：别把动槽状态漏给别的用例

	// 改 key：200 + 落盘 + 即时生效
	if rec := post(`{"token":"new-secret-1"}`); rec.Code != 200 {
		t.Fatalf("改 key 应 200，got %d body=%s", rec.Code, rec.Body.String())
	}
	if got := h.dashToken(); got != "new-secret-1" {
		t.Fatalf("改完应即时生效，got %q", got)
	}
	b, err := os.ReadFile(h.DashTokenFile)
	if err != nil || string(b) != "new-secret-1" {
		t.Fatalf("落盘内容不符: %q err=%v", b, err)
	}
	if fi, _ := os.Stat(h.DashTokenFile); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key 文件应 0600，got %v", fi.Mode().Perm())
	}

	// GET 反映新 key
	rec := httptest.NewRecorder()
	h.dashKeyHandler(rec, httptest.NewRequest(http.MethodGet, "/api/dashkey", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"token":"new-secret-1"`) {
		t.Fatalf("GET 应反映新 key，got %d %s", rec.Code, rec.Body.String())
	}

	// 方法不允许 → 405
	rec = httptest.NewRecorder()
	h.dashKeyHandler(rec, httptest.NewRequest(http.MethodPut, "/api/dashkey", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT 应 405，got %d", rec.Code)
	}

	// 坏 body → 400
	if rec := post(`not-json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 body 应 400，got %d", rec.Code)
	}

	// 空串（含纯空白）= 持久化关闭
	if rec := post(`{"token":"   "}`); rec.Code != 200 || h.dashToken() != "" {
		t.Fatalf("空串应持久化关闭，got %d key=%q", rec.Code, h.dashToken())
	}

	// env 显式设置 → 409 拒改，文件不被碰
	t.Setenv("DASH_TOKEN", "env-final")
	if err := os.WriteFile(h.DashTokenFile, []byte("persisted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rec := post(`{"token":"hack"}`); rec.Code != http.StatusConflict {
		t.Fatalf("env 设置中应 409，got %d", rec.Code)
	}
	if b, _ := os.ReadFile(h.DashTokenFile); string(b) != "persisted" {
		t.Fatalf("409 时不得落盘，文件=%q", b)
	}
}

// 自签会话（2026-10-20）：网关重启后老 cookie 必须仍有效——
// 这是"会话表在内存里"的老毛病（重启即作废，把用户踢回登录页）。
func TestDashSessionSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "dash-session.key")

	// 第一次"进程"：签发 cookie
	h1 := &Handlers{DashSessionKeyFile: keyFile}
	sid := h1.dashNewSession()

	// 第二次"进程"（同一台机、同一密钥文件 = 重启后的新进程）
	h2 := &Handlers{DashSessionKeyFile: keyFile}
	if !h2.dashValidSession(httptest.NewRecorder(), reqWithCookie(sid)) {
		t.Fatal("重启后同一密钥签的 cookie 应仍然有效")
	}
	// 换一把密钥（另一台机/另一套部署）⇒ 不认
	h3 := &Handlers{DashSessionKeyFile: filepath.Join(dir, "other.key")}
	if h3.dashValidSession(httptest.NewRecorder(), reqWithCookie(sid)) {
		t.Fatal("换密钥后不该认旧 cookie")
	}
}

// 注销要能作废：自签 cookie 天然无服务端状态，靠已作废 id 小表兜住。
func TestDashSessionRevoke(t *testing.T) {
	dir := t.TempDir()
	h := &Handlers{DashSessionKeyFile: filepath.Join(dir, "dash-session.key")}
	sid := h.dashNewSession()
	if !h.dashValidSession(httptest.NewRecorder(), reqWithCookie(sid)) {
		t.Fatal("新 cookie 应有效")
	}
	h.dashRevokeSession(sessionIDOf(sid))
	if h.dashValidSession(httptest.NewRecorder(), reqWithCookie(sid)) {
		t.Fatal("注销后 cookie 应失效")
	}
}

func reqWithCookie(v string) *http.Request {
	r := httptest.NewRequest("GET", "/api/stats", nil)
	r.AddCookie(&http.Cookie{Name: dashCookieName, Value: v})
	return r
}

func sessionIDOf(v string) string {
	id, _, _, ok := dashSessionParts(v)
	if !ok {
		return ""
	}
	return id
}
