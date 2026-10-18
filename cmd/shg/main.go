// Package main —— cmd/gateway：装配入口。
//
// **只装配，不做业务**。所有业务都在 internal/，这里做的事按序就是：
//
//  1. runtime：EnvString 读环境 → Resolve → EnsureDirs → Sweep →
//     SQLite 打开 → attempts 落盘 worker + Housekeeper（保养）
//  2. config 首次加载 + 启动自检（log.Fatal if 无后端 —— 空-backend 起网关
//     没有意义，起不来比起来发 503 诚实）+ 后台 goroutine 周期 ReloadIfChanged
//  3. routing.New(backends) 包进 AtomicRouter（热重载原子换装）
//  4. Handlers 结构接线（proxy / routing / config / store / auth / apikey / 看板）
//  5. auth.RegisterEnabledLogins 挂登录、apikey.NewKeyStore 建下游 key
//  6. api.New(...).LAN() / .Public() 两个 http.Server（LAN 免鉴权口、
//     公网口整包过 dashGate + 下游 key 双通道；公网不配 LISTEN_PUBLIC 不监听）
//  7. 启动日志：版本、路径、后端数、登录方式启用清单
//  8. 等信号，优雅关闭（server.Shutdown → attempts 冲盘 → Housekeeper.Stop
//     → Store.Close → 日志句柄收尾）
package main

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"shanhai-gateway/internal/api"
	"shanhai-gateway/internal/config"
	"shanhai-gateway/internal/core/store"
	"shanhai-gateway/internal/runtime"
	"shanhai-gateway/internal/service/affinity"
	"shanhai-gateway/internal/service/apikey"
	"shanhai-gateway/internal/service/auth"
	_ "shanhai-gateway/internal/service/auth/providers" // 触发登录方式注册（admin/dev）
	"shanhai-gateway/internal/service/proxy"
	"shanhai-gateway/internal/service/routing"
	"shanhai-gateway/web"
)

// defaultConfigReload 基线热加载的默认轮询周期。配置文件 mtime 没变时
// ReloadIfChanged 只做一次 stat，轮询本身零负担。
const defaultConfigReload = 30 * time.Second

// defaultQueueWait 满载排队上限的默认值（proxy.Options.QueueWait）。
const defaultQueueWait = 60 * time.Second

// 会话亲和的默认参数（2026-10-11 接线；env 可覆盖，见 main 里的装配注释）。
// Max/TTL/K 与 affinity 包的内置默认一致——装配层显式写出来是为了日志可读
// （启动清单里打印生效值），不是第二份策略。
const (
	defaultAffinityMax             = 8192
	defaultAffinityTTL             = 6 * time.Hour
	defaultAffinityContentionLimit = 3
)

// maxShutdown 优雅关闭的最长等待：在飞请求该有收尾时间，但也不能让
// 卡死的连接把关停拖成僵尸。10 秒足够 SSE 掐断 + 冲盘。
const maxShutdown = 10 * time.Second

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("[gateway] %s 启动", runtime.AppID())

	// ── 1) runtime：路径 → 目录 → 清扫 ──────────────────────────
	paths, err := runtime.Resolve()
	if err != nil {
		log.Fatalf("[gateway] 运行时路径解析失败：%v", err)
	}
	if err := paths.EnsureDirs(); err != nil {
		log.Fatalf("[gateway] 创建运行目录失败：%v", err)
	}
	// 启动清扫：清掉上次异常退出留下的临时配档（res 打日志不挡路）。
	if res, serr := runtime.Sweep(paths, runtime.LogOptions{}); serr != nil {
		log.Printf("[gateway] 启动清扫失败（不挡启动）：%v", serr)
	} else {
		log.Printf("[gateway] sweep：%s", res)
	}

	// ── 2) 日志：stderr + 轮转文件 ─────────────────────────────
	appLog, logErr := runtime.NewRotatingWriter(filepath.Join(paths.LogDir, "app.log"), runtime.LogOptions{})
	if logErr != nil {
		log.Printf("[gateway] 轮转日志不可用，退回 stderr：%v", logErr)
	}

	// ── 3) SQLite：控制面库 + attempts 后台冲盘 + 保养 ──────────
	st, err := store.Open(paths.DBFile)
	if err != nil {
		log.Fatalf("[gateway] 打开数据库 %s 失败：%v", paths.DBFile, err)
	}
	if err := st.Ping(); err != nil {
		log.Fatalf("[gateway] 数据库探活失败：%v", err)
	}
	log.Printf("[gateway] db=%s", paths.DBFile)

	// attempts JSONL：store.Start 的第二出口（每行一条 Attempt，
	// 与磁盘环形统计同源；轮转文件放 logs/ 下）。
	attemptsLog, err := runtime.NewRotatingWriter(filepath.Join(paths.LogDir, "attempts.jsonl"), runtime.LogOptions{})
	if err != nil {
		log.Printf("[gateway] attempts JSONL 不可用（DB 仍会记账）：%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go st.Start(ctx, attemptsLog) // 谢幕时冲最后一批，不丢统计

	housekeeper := runtime.NewHousekeeper(st.DB(), runtime.EnvDuration("HOUSEKEEP_INTERVAL", 0))
	housekeeper.Start(ctx)

	// ── 4) config：首次加载 + 自检 + 热加载 ─────────────────────
	cfg, err := config.NewStore(paths)
	if err != nil {
		// 「没有后端」时起网关没有意义：起不来比起来发 503 诚实。
		// 这是装配层唯一的 log.Fatal 型配置错误，其它一切先试着起。
		log.Fatalf("[gateway] 后端配置加载失败：%v", err)
	}
	backends := cfg.Snapshot().Backends
	log.Printf("[config] 首次加载：%d 个后端", len(backends))
	if config.ShouldValidate() {
		kept, verr := config.Validate(paths, cfg.Snapshot(), config.NewHTTPProber())
		if verr != nil {
			// 全部判定为永久失效/黑名单清空 —— 与旧实现一致 fatal：
			// 对一个注定 502 的网关保持进程存活只会掩盖故障。
			log.Fatalf("[gateway] 启动自检后没有可用后端：%v", verr)
		}
		backends = kept
	}
	if len(backends) == 0 {
		log.Fatal("[gateway] no backends configured（修正 backends.json / ROUTER_BACKENDS 后重启）")
	}

	// ── 5) routing + 热加载重建 ────────────────────────────────
	router := routing.NewAtomicRouter(routing.New(backends))
	log.Printf("[router] %d backend(s) · %s · 启动自检完成", len(backends), runtime.AppID())

	// config 热加载后台 goroutine：mtime 变了 → 重建 Router（原子换装）。
	// ⚠️ 与 admin 热改的关系（见 internal/api/admin.go 文件头）：
	// admin 改的是**内存里的 backend 实例**，重建后回落 —— 既有语义，保留。
	go func() {
		interval := runtime.EnvDuration("CONFIG_RELOAD_INTERVAL", defaultConfigReload)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			changed, err := cfg.ReloadIfChanged()
			if err != nil {
				// ReloadIfChanged 失败时旧配置原封不动，只记日志。
				log.Printf("[config] %v", err)
			}
			if !changed {
				continue
			}
			// 基线换新：黑名单/手动标签过滤后重建 Router。重建必须**完整
			// 建好再换**（AtomicRouter.Store 的合同），绝不半成品切流量。
			kept, err := config.Validate(cfg.Paths(), cfg.Snapshot(), nil) // nil=不探测，只过滤
			if err != nil || len(kept) == 0 {
				log.Printf("[config] 热加载后没有可用后端，沿用旧路由（%v）", err)
				continue
			}
			router.Store(routing.New(kept))
			log.Printf("[router] Router 已原子重建：%d 个后端", len(kept))
		}
	}()

	// ── 6) 服务接线 ──────────────────────────────────────────
	asvc, err := auth.NewService(st)
	if err != nil {
		log.Fatalf("[gateway] 认证服务组装失败：%v", err)
	}
	keys, err := apikey.NewKeyStore(paths, st)
	if err != nil {
		log.Fatalf("[gateway] 下游 key 库组装失败：%v", err)
	}

	opt := proxy.Options{
		QueueWait:       runtime.EnvDuration("QUEUE_WAIT", defaultQueueWait),
		DefaultModel:    runtime.EnvString("DEFAULT_MODEL", ""),
		ReasoningEffort: runtime.EnvString("REASONING_EFFORT", "low"),
	}
	// ── 会话亲和（2026-10-11）：开关与默认值只在装配层读，handler 零感知 ──
	// SESSION_AFFINITY=on（默认开）。off 时 tbl 保持 nil —— proxy.Options
	// 与 api.Handlers 都拿 nil，亲和分支全部短路，行为与未接亲和逐位一致。
	// 内测可关，生产默认开。三个参数也从 env 读（默认见下方常量）。
	var tbl *affinity.Table
	if runtime.EnvString("SESSION_AFFINITY", "on") == "on" {
		affOpt := affinity.Options{
			Max:             int(runtime.EnvFloat("AFFINITY_MAX", defaultAffinityMax)),
			TTL:             runtime.EnvDuration("AFFINITY_TTL", defaultAffinityTTL),
			ContentionLimit: int(runtime.EnvFloat("AFFINITY_CONTENTION_LIMIT", defaultAffinityContentionLimit)),
		}
		tbl = affinity.New(affOpt)
		log.Printf("[affinity] 会话亲和开启 max=%d ttl=%v contention_limit=%d",
			affOpt.Max, affOpt.TTL, affOpt.ContentionLimit)
	} else {
		log.Printf("[affinity] SESSION_AFFINITY=off：会话亲和关闭")
	}
	opt.Affinity = tbl
	// ⚠️ Proxy 动态取 router：config 热重建后立刻路由到新列表。
	// proxy.Handler 的签名吃 *routing.Router（闭包会钉死旧指针），
	// 所以在这里包一层每次 Load —— 装配层的活，不动 proxy 的纪律。
	proxyHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy.Handler(router.Load(), st, opt)(w, r)
	})

	started := time.Now()
	handlers := &api.Handlers{
		Router:      router,
		Proxy:       proxyHandler,
		Store:       st,
		Auth:        asvc,
		Keys:        keys,
		Config:      cfg,
		Affinity:    tbl,
		Metrics:     api.NewMetrics(started),
		QuotaFile:   paths.QuotaFile,
		ConsoleFS:   mustSubFS("console"),
		DashboardFS: mustSubFS("dashboard"),
		StartedAt:   started,
	}

	// ── 7) 两个 http.Server ─────────────────────────────────
	lanAddr := runtime.EnvString("LISTEN", ":18006")
	lanSrv := &http.Server{Addr: lanAddr, Handler: api.New(handlers).LAN()}

	srvErr := make(chan error, 2)
	go func() {
		log.Printf("[router] LAN listening on %s (免鉴权定调)", lanAddr)
		if err := lanSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()

	var pubSrv *http.Server
	if pubAddr := runtime.EnvString("LISTEN_PUBLIC", ""); pubAddr != "" {
		pubSrv = &http.Server{Addr: pubAddr, Handler: api.New(handlers).Public()}
		go func() {
			log.Printf("[router] public dash listening on %s（login gate on）", pubAddr)
			if err := pubSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				// 公网口挂了不算全灭：LAN 口照常活（slr 原语义）。
				log.Printf("[router] public listener failed: %v（LAN 口不受影响）", err)
			}
		}()
	} else {
		log.Printf("[router] LISTEN_PUBLIC 未设置：公网口不监听")
	}

	// 启动清单一把输出：路径全走 runtime.Paths，一个绝对字面量都不出现。
	log.Printf("[gateway] dataDir=%s configDir=%s keys=%s quota=%s",
		paths.DataDir, paths.ConfigDir, paths.KeysFile, paths.QuotaFile)
	log.Printf("[gateway] 登录方式清单见上行 [auth] 日志；LAN=%s PUB=%s", lanAddr, pubAddrOr(pubSrv))

	// ── 8) 等信号，优雅关闭 ────────────────────────────────
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-srvErr:
		if err != nil {
			log.Fatalf("[gateway] 监听失败：%v", err)
		}
	case s := <-sig:
		log.Printf("[gateway] 收到 %v，开始优雅关闭", s)
	}

	// 先停入口（在飞请求继续收尾 → attempts 进入队列），再冲盘，再关库。
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), maxShutdown)
	defer shutdownCancel()
	_ = lanSrv.Shutdown(shutdownCtx)
	if pubSrv != nil {
		_ = pubSrv.Shutdown(shutdownCtx)
	}
	cancel() // attempts worker 冲最后一批出队
	housekeeper.Stop()
	if err := st.Close(); err != nil {
		log.Printf("[gateway] Store.Close：%v", err)
	}
	if logErr == nil {
		_ = appLog.Close()
	}
	if attemptsLog != nil {
		_ = attemptsLog.Close()
	}
	log.Printf("[gateway] %s 已停止", runtime.AppID())
}

// pubAddrOr 公网地址的启动日志助手（未监听时显式说明，不给查问题的人留谜）。
func pubAddrOr(srv *http.Server) string {
	if srv == nil {
		return "未监听"
	}
	return srv.Addr
}

// mustSubFS 从 web 包的 embed FS 里取子树。embed 的文件随二进制编进去，
// 失败说明把 web/embed.go 改坏了 —— fatal 让它当场炸，不要捂着。
func mustSubFS(name string) fs.FS {
	var src fs.FS
	switch name {
	case "console":
		src = web.Console
	case "dashboard":
		src = web.Dashboard
	default:
		log.Fatalf("[gateway] 未知前端子树 %q", name)
	}
	sub, err := fs.Sub(src, name)
	if err != nil {
		log.Fatalf("[gateway] embed 子树 %s 不可用：%v", name, err)
	}
	return sub
}
