package routing

import (
	"sync/atomic"
)

// ── AtomicRouter：Router 指针的原子换装 ──────────────────────────
//
// 背景（2026-10-11 接线批）：
//
//   - admin 热改（/admin/backend/{name}）改的是**内存里的 backend 实例状态**
//     （concurrency / weight / enabled），立即可见，进程重启后回落配置。
//   - config 热加载（config.Store.ReloadIfChanged）换的是**整份后端列表**
//     （新 / 改 / 删都在文件里），需要**重建一个 Router** 再上线。
//
// 重建的**崩溃保护**：新 Router 必须完整建好（routing.New 返回成功）之后
// 才允许替换指针，绝不在半建状态下切流量。atomic.Pointer 保证读者永远只看
// 到旧对象的完整状态或新对象的完整状态，不可能读到中间态 —— 这条性质由
// router_atomic_test.go 钉死（读者随时能查到全部已配置后端）。
//
// 读侧是 O(1) 无锁：每次请求 Load 一次指针，热重载不影响请求热路径。
// 代价：热重载瞬间，开在旧 Router 里的请求继续用旧 backend 实例跑完
// （它们持有指针），新请求走新 Router —— 两批并存，各自的熔断/冷却计数
// 互不污染（与 config.Store.Snapshot 的快照语义同一条纪律）。

// AtomicRouter 持有一个「当前生效」的 *Router，支持无锁读取与原子换装。
type AtomicRouter struct {
	p atomic.Pointer[Router]
}

// NewAtomicRouter 以 initial 作为当前 Router。initial 由调用方提前建好
// （本类型不做懒建——nil 状态只会把 panic 推迟到第一个请求）。
func NewAtomicRouter(initial *Router) *AtomicRouter {
	a := &AtomicRouter{}
	a.Store(initial)
	return a
}

// Load 取当前 Router。任何时刻返回的都是**完整可用**的对象（见上）。
func (a *AtomicRouter) Load() *Router { return a.p.Load() }

// Store 原子换装。调用方契约：next 必须已经完整建好（New 返回），
// 且**已包含**全部需要的后端 —— 换进去的是成品，不是毛坯。
func (a *AtomicRouter) Store(next *Router) { a.p.Store(next) }
