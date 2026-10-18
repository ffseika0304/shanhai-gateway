package api

// router_test.go —— 控制面用 httptest 的最小冒烟（不打真实上游）。
//
// 打桩约定与 service/proxy 的测试一致：后端指到 http://127.0.0.1:1（准对
// 端口 1，连接立刻被拒）—— 代理层会把它判成网络错误并整链失败回 502，
// 我们只断言「请求到达了代理层」（不是 401/404），不关心上游语义。
//
// env 纪律：所有凭据（ADMIN_PASSWORD / DASH_* / DEV_MODE）都用 t.Setenv
// 注进来，测试零真实凭据；keys.json 写进 t.TempDir()。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shanhai-gateway/internal/config"
	"shanhai-gateway/internal/core/store"
	"shanhai-gateway/internal/core/upstream"
	"shanhai-gateway/internal/runtime"
	"shanhai-gateway/internal/service/apikey"
	"shanhai-gateway/internal/service/auth"
	_ "shanhai-gateway/internal/service/auth/providers" // 触发登录方式注册（admin/dev）
	"shanhai-gateway/internal/service/proxy"
	"shanhai-gateway/internal/service/routing"
	"shanhai-gateway/web"
)

// deadBack 一个死后端名（127.0.0.1:1，发包即拒）。
const deadURL = "http://127.0.0.1:1/v1"

// newGateway 组装一套完整的 Handlers + LAN/Public 两个 handler。
// keysFile 传 "" 时用 TempDir 里的缺失文件（keystore 空转，日志记 load error）。
func newGateway(t *testing.T) (*API, *store.Store, *upstream.Backend) {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// stub backend：只当路由表里的一根占位（请求打出去立刻被拒）。
	b := upstream.New("stub", "m", deadURL, map[string]*upstream.Account{})
	b.Enabled = true

	paths := &runtime.Paths{KeysFile: filepath.Join(dir, "keys.json")}
	keys, err := apikey.NewKeyStore(paths, st)
	if err != nil {
		t.Fatal(err)
	}
	asvc, err := auth.NewService(st)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.NewStoreFrom(&runtime.Paths{BackendsFile: filepath.Join(dir, "backends.json")},
		&config.Snapshot{Backends: []*upstream.Backend{b}})

	rt := routing.NewAtomicRouter(routing.New([]*upstream.Backend{b}))
	proxyH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.Handler(rt.Load(), st, proxy.Options{QueueWait: time.Second})(w, r)
	})
	started := time.Now()
	h := &Handlers{
		Router:      rt,
		Proxy:       proxyH,
		Store:       st,
		Auth:        asvc,
		Keys:        keys,
		Config:      cfg,
		Metrics:     NewMetrics(started),
		QuotaFile:   filepath.Join(dir, "quota.json"),
		ConsoleFS:   consoleFSTest(t),
		DashboardFS: dashboardFSTest(t),
		StartedAt:   started,
	}
	api := New(h)
	return api, st, b
}

// call 测试直打 handler 的薄封装（不跟随 302，要看的就是码）。
// 传 form 体时按真实客户端（curl -d）的做法带表单 Content-Type。
func call(t *testing.T, h http.Handler, method, target, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Result()
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// ── 用户中心 + 登录三套 ──────────────────────────────────────

func TestAdminLoginAndUserCenter(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "cutover-test")
	apiEx, st, _ := newGateway(t)
	lan := apiEx.LAN()

	// 错密码 → 401
	resp := call(t, lan, "POST", "/api/auth/admin-login", `{"password":"wrong"}`)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错密码应 401，got %d %s", resp.StatusCode, body(t, resp))
	}
	// 对密码 → 200 + token
	resp = call(t, lan, "POST", "/api/auth/admin-login", `{"password":"cutover-test"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("对密码应 200，got %d %s", resp.StatusCode, body(t, resp))
	}
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body(t, resp)), &lr); err != nil || lr.Token == "" {
		t.Fatalf("登录回包应有 token：%v %s", err, body(t, resp))
	}

	// /api/user/me：header token → 200
	req := httptest.NewRequest("GET", "/api/user/me", nil)
	req.Header.Set("X-Session-Token", lr.Token)
	rr := httptest.NewRecorder()
	lan.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"user_id"`) {
		t.Fatalf("me 应 200，got %d %s", rr.Code, rr.Body.String())
	}

	// ⚠️ query token 401：统一策略回归 —— 需要在 header 里，query 一律不认。
	req = httptest.NewRequest("GET", "/api/user/me?token="+url.QueryEscape(lr.Token), nil)
	rr = httptest.NewRecorder()
	lan.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("?token= 必须被拒（401），got %d %s", rr.Code, rr.Body.String())
	}
	// 无 token 401
	rr = httptest.NewRecorder()
	lan.ServeHTTP(rr, httptest.NewRequest("GET", "/api/user/keys", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("无 token 应 401，got %d", rr.Code)
	}

	// 生成 → 列出（掩码）→ 删除
	req = httptest.NewRequest("POST", "/api/user/keys/generate", strings.NewReader(`{"name":"t1"}`))
	req.Header.Set("X-Session-Token", lr.Token)
	rr = httptest.NewRecorder()
	lan.ServeHTTP(rr, req)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `sk-user-`) {
		t.Fatalf("generate key 应回 sk-user-*，got %d %s", rr.Code, rr.Body.String())
	}
	ks, err := st.ListKeys(1)
	if err != nil || len(ks) != 1 {
		t.Fatalf("生成失败：%v %v", err, ks)
	}
	req = httptest.NewRequest("DELETE", fmt.Sprintf("/api/user/keys/%d", ks[0].ID), nil)
	req.Header.Set("X-Session-Token", lr.Token)
	rr = httptest.NewRecorder()
	lan.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("delete key 应 200，got %d %s", rr.Code, rr.Body.String())
	}
	if ks2, _ := st.ListKeys(1); len(ks2) != 0 {
		t.Fatalf("删除后列表应为空")
	}
}

func TestDevLogin(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		t.Setenv("ADMIN_PASSWORD", "x")
		apiEx, _, _ := newGateway(t)
		if r := call(t, apiEx.LAN(), "POST", "/api/auth/dev-login", ""); r.StatusCode != http.StatusNotFound {
			t.Fatalf("DEV_MODE 未开 dev-login 应 404（provider 未注册），got %d", r.StatusCode)
		}
	})
	t.Run("enabled", func(t *testing.T) {
		t.Setenv("DEV_MODE", "1")
		apiEx, _, _ := newGateway(t)
		r := call(t, apiEx.LAN(), "POST", "/api/auth/dev-login", "")
		if r.StatusCode != 200 {
			t.Fatalf("dev-login 应 200，got %d %s", r.StatusCode, body(t, r))
		}
	})
}

// ── LAN 口免鉴权 ─────────────────────────────────────────────

func TestLANChatFreeNoAuth(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "x")
	apiEx, _, _ := newGateway(t)
	lan := apiEx.LAN()

	// 不带任何凭据也要摸到代理层 —— 502（后端死桩全败），不是 401。
	r := call(t, lan, "POST", "/v1/chat/completions", `{"model":"auto","messages":[]}`)
	bd := body(t, r)
	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("LAN /v1 免鉴权应直达代理（后端死 → 502），got %d %s", r.StatusCode, bd)
	}
	if !strings.Contains(bd, "all backends failed") {
		t.Fatalf("应回报代理层的全败语义：%s", bd)
	}
	// /v1/models 同样免鉴权直达（死桩 → 502 no backend available）
	r = call(t, lan, "GET", "/v1/models", "")
	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("LAN /v1/models 应直达代理，got %d", r.StatusCode)
	}
}

// ── 公网口：dashGate + key 双通道 ────────────────────────────

func TestPublicGate(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "x")
	t.Setenv("DASH_USER", "cutover")
	t.Setenv("DASH_PASS", "cutovertest")
	t.Setenv("DASH_TOKEN", "pub-token")
	apiEx, _, _ := newGateway(t)
	pub := apiEx.Public()

	// 1) 未带凭据 /v1 → 401 JSON
	r := call(t, pub, "POST", "/v1/chat/completions", `{"model":"m"}`)
	if r.StatusCode != http.StatusUnauthorized || !strings.Contains(body(t, r), "unauthorized") {
		t.Fatalf("公网裸 /v1 应 401，got %d %s", r.StatusCode, body(t, r))
	}
	// 2) 页面导航无凭据 → 302 /login
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept", "text/html")
	rr := httptest.NewRecorder()
	pub.ServeHTTP(rr, req)
	if rr.Code != http.StatusFound || !strings.Contains(rr.Header().Get("Location"), "/login") {
		t.Fatalf("裸页面应 302 /login，got %d loc=%s", rr.Code, rr.Header().Get("Location"))
	}

	// 3) DASH_TOKEN 过 /v1（到达代理层 = 502，不是 401）
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer pub-token")
	rr = httptest.NewRecorder()
	pub.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("DASH_TOKEN 过门应直达代理（死桩 502），got %d %s", rr.Code, rr.Body.String())
	}

	// 4) keys.json 的 key 也过门 —— 在 TestPublicGateWithKeysFile 里单独做
	//    （这里 newGateway 的 keys.json 在 TempDir 不存在，keystore 为空）。

	// 5) 登录页表单：错密 → 302 /login?err=1；对密 → 302 / + cookie
	r = call(t, pub, "POST", "/login", "user=cutover&pass=wrong")
	if r.StatusCode != http.StatusFound || !strings.Contains(r.Header.Get("Location"), "err=1") {
		t.Fatalf("错密应 302 /login?err=1，got %d loc=%s", r.StatusCode, r.Header.Get("Location"))
	}
	r = call(t, pub, "POST", "/login", "user=cutover&pass=cutovertest")
	if r.StatusCode != http.StatusFound {
		t.Fatalf("对密应 302 /，got %d", r.StatusCode)
	}
	var ck *http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == dashCookieName {
			ck = c
		}
	}
	if ck == nil || ck.Value == "" {
		t.Fatalf("登录成功应下发 cookie")
	}
	// cookie 上 /api/stats → 200
	req = httptest.NewRequest("GET", "/api/stats", nil)
	req.AddCookie(ck)
	rr = httptest.NewRecorder()
	pub.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("登录 cookie 应放行看板，got %d %s", rr.Code, rr.Body.String())
	}
}

func TestPublicGateWithKeysFile(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "x")
	t.Setenv("DASH_USER", "cutover")
	t.Setenv("DASH_PASS", "cutovertest")
	t.Setenv("DASH_TOKEN", "pub-token")
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// keys.json 里放一把管理员静态 key —— 公网口用它过门调 /v1
	kf := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(kf, []byte(
		`[{"name":"tester","key":"sk-static-test","quota":-1,"used":0,"enabled":true}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	b := upstream.New("stub", "m", deadURL, map[string]*upstream.Account{})
	b.Enabled = true
	paths := &runtime.Paths{KeysFile: kf}
	keys, err := apikey.NewKeyStore(paths, st)
	if err != nil {
		t.Fatal(err)
	}
	asvc, _ := auth.NewService(st)
	cfg := config.NewStoreFrom(&runtime.Paths{BackendsFile: filepath.Join(dir, "backends.json")},
		&config.Snapshot{Backends: []*upstream.Backend{b}})
	rt := routing.NewAtomicRouter(routing.New([]*upstream.Backend{b}))
	proxyH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.Handler(rt.Load(), st, proxy.Options{QueueWait: time.Second})(w, r)
	})
	started := time.Now()
	h := &Handlers{Router: rt, Proxy: proxyH, Store: st, Auth: asvc, Keys: keys,
		Config: cfg, Metrics: NewMetrics(started), QuotaFile: filepath.Join(dir, "q.json"),
		ConsoleFS: consoleFSTest(t), DashboardFS: dashboardFSTest(t), StartedAt: started}

	pub := New(h).Public()

	// keys.json key 过门 → 直达代理（死桩 502）
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-static-test")
	rr := httptest.NewRecorder()
	pub.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("keys.json key 应过门直达代理，got %d %s", rr.Code, rr.Body.String())
	}
	// 扣额度：used+1（近似的 consume-on-end，见 public.go 注释）
	ks, _ := keys.Auth("sk-static-test")
	if ks == nil {
		t.Fatalf("Auth 失败")
	}
	if ks.Used != 1 {
		t.Fatalf("keys.json key 应扣一次额度，used=%d", ks.Used)
	}
	// 伪造的 sk-user-*（DB 无此 key）→ 不算下游 key → 落到 dashGate → 401
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-user-notexist")
	rr = httptest.NewRecorder()
	pub.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("伪造 sk-user key 应 401，got %d", rr.Code)
	}
}

// ── admin 热改（内存态，立即生效；重启/Router 重建回落）────────

func TestAdminHotChange(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "x")
	apiEx, _, b := newGateway(t)
	lan := apiEx.LAN()

	b.Concurrency = 4
	b.Weight = 1

	// GET：初始 4 / 1 / enabled
	r := call(t, lan, "GET", "/admin/backend/stub", "")
	var g struct {
		Concurrency int     `json:"concurrency"`
		Weight      float64 `json:"weight"`
		Enabled     bool    `json:"enabled"`
	}
	if r.StatusCode != 200 || json.Unmarshal([]byte(body(t, r)), &g) != nil {
		t.Fatalf("admin GET 应 200 JSON，got %d %s", r.StatusCode, body(t, r))
	}
	if g.Concurrency != 4 || g.Weight != 1 || !g.Enabled {
		t.Fatalf("初始值 4/1/true，got %+v", g)
	}

	// POST：热改 concurrency / weight / enabled
	r = call(t, lan, "POST", "/admin/backend/stub", `{"concurrency":2,"weight":3.5,"enabled":false}`)
	if r.StatusCode != 200 || !strings.Contains(body(t, r), `"status":"ok"`) {
		t.Fatalf("admin POST 应 200 ok，got %d %s", r.StatusCode, body(t, r))
	}
	r = call(t, lan, "GET", "/admin/backend/stub", "")
	if json.Unmarshal([]byte(body(t, r)), &g) != nil {
		t.Fatal("GET 失败")
	}
	if g.Concurrency != 2 || g.Weight != 3.5 || g.Enabled {
		t.Fatalf("热改未生效：got %+v", g)
	}
	if b.Concurrency != 2 || b.Weight != 3.5 || b.Enabled {
		t.Fatalf("改的不是同一个实例（内存态语义破）：got %+v", g)
	}
	// 未知后端 404
	if r := call(t, lan, "GET", "/admin/backend/nope", ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("未知后端应 404，got %d", r.StatusCode)
	}
	// /clear 分支
	if r := call(t, lan, "POST", "/admin/backend/stub/clear", ""); r.StatusCode != 200 {
		t.Fatalf("clear 应 200，got %d %s", r.StatusCode, body(t, r))
	}
}

// ── 看板 API（attempts 有数据时回合法 JSON）───────────────────

func TestDashboardEndpoints(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "x")
	apiEx, st, b := newGateway(t)
	lan := apiEx.LAN()

	// 写入 attempts：起 worker，Emit 几条，等冲盘
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go st.Start(ctx, nil)
	st.Emit(store.Attempt{Rid: "r1", Backend: b.Name, Model: "m", Seq: 1, Status: 200,
		LatMs: 10, FtMs: 5, Class: "", OK: true, TotalTokens: 100, PromptTokens: 60,
		CompletionTokens: 40, CachedTokens: 20})
	st.Emit(store.Attempt{Rid: "r2", Backend: b.Name, Model: "m", Seq: 2, Status: 0,
		Class: "network", OK: false, Err: "conn refused"})
	time.Sleep(600 * time.Millisecond) // flushInterval 250ms，等冲盘

	// /api/metrics
	r := call(t, lan, "GET", "/api/metrics", "")
	bd := body(t, r)
	if r.StatusCode != 200 || !strings.Contains(bd, `"total_requests":2`) {
		t.Fatalf("metrics 应回 attempts 合计，got %d %s", r.StatusCode, bd)
	}
	if !strings.Contains(bd, "stub") {
		t.Fatalf("by_backend 应含 stub：%s", bd)
	}
	// /api/stats
	r = call(t, lan, "GET", "/api/stats", "")
	if r.StatusCode != 200 || !strings.Contains(body(t, r), `"backends"`) {
		t.Fatalf("stats 形状坏了：%d %s", r.StatusCode, body(t, r))
	}
	// /api/recent —— JSON 数组、Attempt 字段名
	r = call(t, lan, "GET", "/api/recent", "")
	bd = body(t, r)
	if !strings.Contains(bd, `"Rid"`) || !strings.Contains(bd, `"OK"`) {
		t.Fatalf("recent 应回 attempt JSON（Rid/OK 字段），got %s", bd)
	}
	// /api/tokens
	r = call(t, lan, "GET", "/api/tokens", "")
	bd = body(t, r)
	if !strings.Contains(bd, `"total_tokens"`) || !strings.Contains(bd, `"month_cost_usd"`) {
		t.Fatalf("tokens 形状坏了：%s", bd)
	}
}

// ── 表裁决回归：qr_tickets / counters / requests 必须不存在 ─────

func TestExcludedTablesNeverExist(t *testing.T) {
	apiEx, st, _ := newGateway(t)
	_ = apiEx
	tables, err := st.Tables()
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{"qr_tickets": true, "counters": true, "requests": true}
	for _, name := range tables {
		if banned[name] {
			t.Fatalf("合并后的库不允许存在表 %s，got tables=%v", name, tables)
		}
	}
	if len(tables) == 0 {
		t.Fatal("表显得有点空，检查 store.Open")
	}
}

// ── quota.json：有文件 200 / 无文件 404 ───────────────────────

func TestQuotaFileServed(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "x")
	apiEx, _, _ := newGateway(t)
	lan := apiEx.LAN()

	if r := call(t, lan, "GET", "/quota.json", ""); r.StatusCode != http.StatusNotFound {
		t.Fatalf("缺文件应 404，got %d", r.StatusCode)
	}
}

func TestQuotaFileExists(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "x")
	dir := t.TempDir()
	st, _ := store.Open(filepath.Join(dir, "t.db"))
	t.Cleanup(func() { st.Close() })

	qf := filepath.Join(dir, "update.json")
	os.WriteFile(qf, []byte(`{"volc":{"coding":{"status":"ok"}}}`), 0o600)

	b := upstream.New("stub", "m", deadURL, map[string]*upstream.Account{})
	b.Enabled = true
	h := &Handlers{
		Proxy:       http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		Router:      routing.NewAtomicRouter(routing.New([]*upstream.Backend{b})),
		Store:       st,
		Auth:        func() *auth.Service { s, _ := auth.NewService(st); return s }(),
		Metrics:     NewMetrics(time.Now()),
		QuotaFile:   qf,
		ConsoleFS:   consoleFSTest(t),
		DashboardFS: dashboardFSTest(t),
	}
	if r := call(t, New(h).LAN(), "GET", "/quota.json", ""); r.StatusCode != 200 {
		t.Fatalf("有文件应 200，got %d", r.StatusCode)
	}
}

// ── 前端挂载：/ → dashboard，/console/ → console（都免登录）────

func TestStaticMounts(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "x")
	apiEx, _, _ := newGateway(t)
	lan := apiEx.LAN()

	r := call(t, lan, "GET", "/", "")
	bd := body(t, r)
	if r.StatusCode != 200 || !strings.Contains(bd, "<!DOCTYPE") {
		t.Fatalf("/ 应回 dashboard html，got %d", r.StatusCode)
	}
	// dashboard 版调用 /api/stats；console 版调用 /api/user/me+localStorage。
	if !strings.Contains(bd, "/api/stats") {
		t.Fatal("/ 应挂运维看板（引用 /api/stats 的那份）")
	}
	r = call(t, lan, "GET", "/console/", "")
	bd = body(t, r)
	if r.StatusCode != 200 || !strings.Contains(bd, "<!DOCTYPE") {
		t.Fatalf("/console/ 应回 console html，got %d", r.StatusCode)
	}
	if !strings.Contains(bd, "session_token") {
		t.Fatalf("/console/ 应是用户中心（localStorage session_token 那份）")
	}
}

// ── 测试装配助手：embed 子树 ────────────────────────────────

func consoleFSTest(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(web.Console, "console")
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

func dashboardFSTest(t *testing.T) fs.FS {
	t.Helper()
	sub, err := fs.Sub(web.Dashboard, "dashboard")
	if err != nil {
		t.Fatal(err)
	}
	return sub
}
