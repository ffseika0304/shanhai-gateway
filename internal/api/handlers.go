// Package api 是接口层 —— 路由装配。**只做装配，不做业务逻辑**：
// 业务在 internal/service/（proxy / routing / auth / apikey），数据在
// internal/core/（store / upstream），本包把它们的 handler 按「两个入口」
// 装成两个 http.Handler：
//
//   - LAN 口（默认 :18006）：免鉴权定调（文档 §九）。SCF 等既有客户端
//     **零改动**直连，/v1 连 key 都不看。
//   - 公网口（LISTEN_PUBLIC，默认不开）：**整包过 dashGate 登录门**
//     （内含 /v1 —— 「公网扫号机不登录连 LLM 都摸不到」）；在门之外，
//     下游 API key（keys.json / sk-user-*）是 /v1 的第三条过门路子。
//
// 文件分工：
//
//	handlers.go    Handlers 结构：聚合路由/服务/消费，零逻辑
//	router.go      装配 mux 的工作入口 + noCache / firstExistingDir
//	lan.go         免鉴权口的路由表挂法
//	public.go      公网口的门（dashGate + 下游 key 双通道）
//	dashboard.go   看板 API（/api/stats /api/tokens /api/metrics /api/recent /quota.json）
//	usercenter.go  /api/user/*（自公版 user.go，db 全局 → store 方法）
//	admin.go       /admin/backend/{name} GET+POST 热改
//	dashgate.go    登录门（自 seika_llm_router dashauth.go，原样）
//
// 依赖方向：本包 → service/* + core/store + runtime + config（只读它的
// Snapshot 取 pricing），**不许**被 core / service 反向依赖。
//
// ── 视觉资源 ────────────────────────────────────────────────
// 看板（运维 dashboard，挂 /）与用户中心（公版 console，挂 /console/）
// 都用 //go:embed 来自 web/ 目录，路径来源同样只有 runtime + 注入。
package api

import (
	"io/fs"
	"net/http"
	"time"

	"shanhai-gateway/internal/config"
	"shanhai-gateway/internal/core/store"
	"shanhai-gateway/internal/service/affinity"
	"shanhai-gateway/internal/service/apikey"
	"shanhai-gateway/internal/service/auth"
	"shanhai-gateway/internal/service/routing"
)

// Handlers 把路由决策、代理执行、登录会话、下游 key、看板数据聚合到一起。
// **零逻辑**：没有方法、没有行为，纯粹是装配材料 —— 谁依赖谁在这里一眼可见，
// 新增一种服务就在这里加一个字段（而不是再造一个包级全局变量）。
type Handlers struct {
	// Router 当前生效的路由决策器。走 AtomicRouter：config 热加载会整只
	// 换装（原子指针），admin 热改是在 backend 实例上就地改内存态。
	Router *routing.AtomicRouter

	// Proxy 数据面代理 handler（/v1/chat/completions），由 service/proxy
	// 装配（已在接线层包好动态取 Router.Load()）。本层不拆它的闭包。
	Proxy http.HandlerFunc

	// Store 控制面存储（users / sessions / user_api_keys / attempts）。
	Store *store.Store
	// Auth 登录会话服务（RegisterEnabledLogins + AuthMiddleware）。
	Auth *auth.Service
	// Keys 下游 API key 鉴权与额度（keys.json + sk-user-*）。
	Keys *apikey.KeyStore
	// Config 配置 Store：看板取 pricing、/api/meta 取后端数量。只读。
	Config *config.Store

	// Affinity 会话亲和表（2026-10-11 接线）：/api/stats 的 sticky_hits /
	// sticky_misses / sticky_evictions 三个全局计数与每后端 sticky_count
	// 从这里读。可 nil —— SESSION_AFFINITY=off 时没有表，看板回零值，
	// JSON 形状保持稳定（前端不必分支）。
	Affinity *affinity.Table

	// Metrics 看板聚合器（attempts 表 + 进程启动时刻）。
	Metrics *Metrics

	// QuotaFile 配额指挥舱数据文件（runtime.Paths.QuotaFile，由外部
	// 采集脚本写入）；/quota.json 直接回这个磁盘文件，缺则 404。
	QuotaFile string

	// DashTokenFile 看板静态 key 持久化文件（runtime.Paths.DashTokenFile）。
	// POST /api/dashkey 原子写这里，dashToken 每次门检现读 → 改 key 即改即生效。
	DashTokenFile string

	// DashSessionKeyFile 看板会话签名密钥（runtime.Paths.DashSessionKeyFile，0600）。
	// 自签 cookie 的 HMAC 密钥落盘 ⇒ 网关重启后老 cookie 仍有效。
	DashSessionKeyFile string

	// ConsoleFS 用户中心前端（web/console 的子树 fs，已在接线层 fs.Sub 好）。
	ConsoleFS fs.FS
	// DashboardFS 运维看板前端（web/dashboard 子树 fs）。磁盘目录
	// STATIC_DIR 存在时优先磁盘（详见 router.go firstExistingDir）。
	DashboardFS fs.FS

	// StartedAt 进程启动时刻（uptime / "started" 的数据源；attempts 没有
	// counters 表，这个时刻由进程自己记住）。
	StartedAt time.Time
}
