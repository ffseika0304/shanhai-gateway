// router.go —— 装配 mux 的工作入口。
//
// New(handlers) 返回 *API；LAN() / Public() 各出一个 http.Handler。
// 共用一份路由表（mux()），区别只在包装：LAN 不包任何门，Public 包
// dashGate + 下游 key 双通道（public.go）。
//
// noCache / firstExistingDir 自 seika_llm_router main.go（~2051/2064 行）
// 原样搬入，注释保留。
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"shanhaigateway/internal/runtime"
)

// API 装配器：持有 Handlers。
type API struct {
	h *Handlers
}

// New 创建装配器。
func New(h *Handlers) *API { return &API{h: h} }

// mux 两个口共用的路由表。
//
// 挂载清单（与 slr main() 端点列表逐条对齐；能接上的全接上）：
//
//	/health              探活（公网口经 dashGate，未登录 401）
//	/v1/chat/completions 数据面代理（免鉴权口的既有客户端零改动）
//	/v1/models           多后端轮询摸 /models
//	/api/auth/*          登录方式（LoginProvider 自注册；启动日志自己打）
//	/api/user/*          用户中心（X-Session-Token 鉴权，不认 ?token=）
//	/api/stats /api/tokens /api/metrics /api/recent   看板 API
//	/quota.json          配额指挥舱磁盘数据
//	/api/dashkey         静态 key 自取（dashauth.go 原文，两口共用）
//	/admin/backend/      运行态调参（内存态热改）
//	/                    运维看板（LAN 免登录定调）
//	/console/            用户中心前端（公版）
func (a *API) mux() *http.ServeMux {
	mux := http.NewServeMux()

	// --- 数据面 ---
	mux.HandleFunc("/health", a.h.handleHealth)
	mux.HandleFunc("/v1/chat/completions", a.h.Proxy)
	mux.HandleFunc("/v1/models", a.h.handleModels)

	// --- 控制面：登录方式 ---
	// 所有登录方式由 providers 通过 LoginProvider 抽象注册。
	// 内置：管理员密码（ADMIN_PASSWORD）、dev-login（DEV_MODE=1）。
	// ⚠️ 公版原有的小程序登录 + PC 扫码登录（qrlogin.go / websocket.go）已移除 ——
	// 那条链路的唯一用途是「小程序替 PC 确认登录」，小程序没了它就没有意义；
	// 连带的缺失函数（handleQRCode / generateShortTicket）和缺失依赖
	// nhooyr.io/websocket 也随之消失，不用补代码。
	// **抽象层留着**：要接入任何新登录方式，写一个 LoginProvider + 一行
	// RegisterLogin 即可，网关其余部分不用改。
	a.h.Auth.RegisterEnabledLogins(mux)

	// 用户中心（需鉴权；不认 ?token= —— 见 auth.AuthMiddleware 的注释）
	mux.HandleFunc("/api/user/me", a.h.Auth.AuthMiddleware(a.h.handleGetUser))
	mux.HandleFunc("/api/user/keys/generate", a.h.Auth.AuthMiddleware(a.h.handleGenerateKey))
	mux.HandleFunc("/api/user/keys/", a.h.Auth.AuthMiddleware(a.h.handleDeleteKey))
	mux.HandleFunc("/api/user/keys", a.h.Auth.AuthMiddleware(a.h.handleListKeys))

	// --- 看板 ---
	mux.HandleFunc("/api/stats", a.h.handleStats)
	mux.HandleFunc("/api/tokens", a.h.handleTokens)
	mux.HandleFunc("/api/metrics", a.h.handleMetrics)
	mux.HandleFunc("/api/recent", a.h.handleRecent)
	mux.HandleFunc("/api/dashkey", dashKeyHandler)
	mux.HandleFunc("/quota.json", a.h.handleQuota)

	// admin 调参（公网口同口也挂 —— 过 dashGate）
	mux.HandleFunc("/admin/backend/", a.h.handleAdminBackend)

	// --- 前端 ---
	// 用户中心（公版 console）：挂 /console/（go:embed）。
	mux.Handle("/console/", noCache(http.StripPrefix("/console",
		http.FileServer(http.FS(a.h.ConsoleFS)))))

	// 运维看板：挂 /。磁盘目录（STATIC_DIR）存在时优先磁盘 —— 改前端只需
	// 刷新浏览器，无需重启（slr 原注释）；找不到目录才回退内嵌资源
	//（部署兜底；发行环境里 web 目录不随二进制铺开，内嵌是常态）。
	if dir := firstExistingDir(os.Getenv("STATIC_DIR")); dir != "" {
		mux.Handle("/", noCache(http.FileServer(http.Dir(dir))))
	} else {
		mux.Handle("/", noCache(http.FileServer(http.FS(a.h.DashboardFS))))
	}
	return mux
}

// LAN 免鉴权口（默认 LISTEN=:18006）。
func (a *API) LAN() http.Handler { return a.mux() }

// Public 公网口（LISTEN_PUBLIC，不开则不监听）：整包过 dashGate。
// 包装顺序见 public.go。
func (a *API) Public() http.Handler { return publicGate(a.h, a.mux()) }

// handleHealth GET /health —— 版本号统一来自 runtime.AppVersion
// （原 gwVersion 单一来源纪律：公版 /health 曾回 0.3.0、死代码回 0.2.0，
// 那种双口径已消灭，这里只认 internal/runtime/identity.go）。
func (h *Handlers) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "version": runtime.AppVersion})
}

// handleModels GET /v1/models —— 逐个候选后端摸 /models，第一个有响应的透传
// （搬自 slr main.go 注册段原样；Router 经 AtomicRouter 动态取，
// config 热重建后立刻走新列表）。
func (h *Handlers) handleModels(w http.ResponseWriter, r *http.Request) {
	rt := h.Router.Load()
	for _, n := range rt.Candidates() {
		b := rt.ByName(n)
		if b == nil || b.SkipReason(time.Now()) != "" {
			continue
		}
		req, _ := http.NewRequestWithContext(r.Context(), "GET", b.BaseURL+"/models", nil)
		if keys := b.BoundAccount().APIKeys; len(keys) > 0 {
			req.Header.Set("Authorization", "Bearer "+keys[0])
		}
		resp, err := b.Client().Do(req)
		if err != nil {
			continue
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		resp.Body.Close()
		return
	}
	http.Error(w, `{"error":"no backend available"}`, http.StatusBadGateway)
}

// noCache 禁掉前端缓存，保证改完刷新即见（否则浏览器会吃旧 index.html）
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// firstExistingDir 返回第一个存在的目录；都没有则返回空串
func firstExistingDir(cands ...string) string {
	for _, d := range cands {
		if d == "" {
			continue
		}
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			return d
		}
	}
	return ""
}
