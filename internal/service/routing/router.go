// Package routing 管的是「谁该接这个请求」：主轮转后端有哪些、首发给谁、
// 主线全满时往哪分流、哪个兜底刚被隔离。
//
// 从 seika_llm_router 调度内核原样搬入，是调度内核的**第三块**。三块各管各的、
// 独立演进：
//
//   - core/errclass  一次失败算什么（分类 + 处置策略）
//   - core/upstream  后端是谁、能接多少、现在健康不健康（容量 / 健康状态机）
//   - service/routing 本包：后端之间怎么选（加权轮转 / 候选 / 兜底隔离）
//
// 依赖方向是单向的：本包 → core/{errclass,upstream} + runtime + 标准库。
// 对 runtime 只用它的 env 读取器（runtime.EnvDuration），不取路径、不碰日志：
// 路由决策不该依赖运行时设施的具体形状，但「从环境变量读一个时长」是纯函数，
// 没有理由再抄一份（2026-10-11 删掉了本包私有的 durEnv）。runtime 不 import
// 任何项目内包，所以这条依赖不会成环。本包的日志走标准库 log ——
// 与搬入前逐字一致。
//
// ── 搬入时保留了什么 ──────────────────────────────────────────
// 三个数字都是生产事故调出来的，**不要顺手优化**：
//   - fallbackMaxFails=3 / fallbackStrikeWin=10m：兜底隔离链（从 upstream 搬来，
//     它们是「兜底隔离」的策略，不是后端健康度的参数，归属本包）
//   - SWRR「等权时与严格轮转逐字等价」这条性质必须常驻：接线前首发分配是
//     rrSeq%N，改成加权轮转后如果等权场景下的顺序变了，就是一次全量回归。
//     routing_test.go 里有测试钉死它。
//
// 改动前请先读这些注释，它们记的是事故，不是偏好。
package routing

import (
	"log"
	"sync"

	"shanhai-gateway/internal/core/upstream"
)

// Router 一次请求的路由决策器：候选从哪来、首发给谁、失败往哪退。
//
// 它**不含任何请求执行逻辑**（不发请求、不管 key、不管 HTTP）—— 只回答
// 「下一个该试谁」。这样才能用表驱动测试把路由语义逐条钉死。
type Router struct {
	backends []*upstream.Backend
	// 主轮转后端（按配置顺序，来自 is_primary=true 的 backend）
	primary []string
	// 首发分配游标：每个任务按顺序领一个上游，避免全员挤在队头
	rrSeq uint64
	// 兜底模型轮转游标
	fallbackSeq uint64
	// 加权首发轮转（SWRR）状态：后端名 -> 当前权重累加值。
	// 专用锁：与 mu（fallbackFaults）的锁语义互不牵连，避免嵌套持锁。
	swrrMu  sync.Mutex
	swrrCur map[string]float64

	// mu 保护 fallbackFaults。**必须有锁**：这张表被并发的请求 goroutine 同时读写，
	// 裸 map 会触发 Go 进程级 "fatal error: concurrent map writes" 直接崩掉网关。
	mu sync.Mutex
	// 兜底后端的上游故障记录（滑动窗口，不是终身计数）：
	// 窗口内累计 3 次真上游错 → 隔离到窗口结束，到期自动放回再试；
	// 成功一次立即清零。旧行为是终身拉黑、只能重启恢复（2026-09-24 废除）。
	fallbackFaults map[string]*fbFault
}

// New 构建路由器。
//
// backends 是配置解析出来的、已经跑完 upstream.Backend.Init 的后端列表 ——
// 本包**不负责解析配置，也不负责建后端**：那是 internal/config 的活。
// 依赖是显式注入的（没有包级全局可变状态），两个 Router 可以互不干扰地并存
// ——测试里靠这一点把隔离窗口钉在假时间上而互不污染。
func New(backends []*upstream.Backend) *Router {
	// 收集兜底后端与主轮转后端
	fallbacks := []string{}
	primary := []string{}
	for _, b := range backends {
		if b.IsFallback {
			fallbacks = append(fallbacks, b.Name)
		}
		if b.IsPrimary && !b.IsFallback {
			primary = append(primary, b.Name)
		}
	}
	// 兼容旧配置：一个 is_primary 都没写时，把所有「已载入的非兜底后端」当作主轮转。
	// （不再硬编码后端名；名字改成什么都行，完全由配置驱动。）
	if len(primary) == 0 {
		for _, b := range backends {
			if !b.IsFallback {
				primary = append(primary, b.Name)
			}
		}
		if len(primary) > 0 {
			log.Printf("[router] config has no is_primary; treating all non-fallback backends as primary: %v", primary)
		}
	}
	return &Router{
		backends:       backends,
		primary:        primary,
		fallbackFaults: make(map[string]*fbFault),
	}
}

// Names 返回全部在册后端名（含兜底，按配置顺序）。
//
// 这是**只读观测口**，给看板 stats 用：兜底后端不进 Candidates（候选表只含
// 主轮转，这是路由契约），但看板的「兜底模型」表需要看到它们的并发/失败/
// 熔断/冷却——不可用的兜底恰恰最需要可见。路由路径一律不许用这里的结果。
func (rt *Router) Names() []string {
	out := make([]string, 0, len(rt.backends))
	for _, b := range rt.backends {
		out = append(out, b.Name)
	}
	return out
}

// ByName 按名字找后端（线性扫，N 很小；搬入前同样没有建索引）。
//
// 调用方拿到候选名后要拿并发位、要看健康，都在这一层之外完成，
// 本包只负责给出名字和顺序。
func (rt *Router) ByName(name string) *upstream.Backend {
	for _, b := range rt.backends {
		if b.Name == name {
			return b
		}
	}
	return nil
}
