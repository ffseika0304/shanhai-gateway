// Package proxy 是数据面代理：/v1/chat/completions 的核心执行路径。
//
// 从 seika_llm_router 调度内核原样搬入 —— 这是最复杂、也是生产事故教训最密集
// 的两段：attempt（上游一次尝试）与 proxyHandler（客户端分发）。
//
// ── 依赖纪律 ────────────────────────────────────────────────
// 本包允许 import：runtime、core/errclass、core/upstream、core/store、
// service/routing、标准库。**不许** import internal/config、internal/api、
// 根目录，也不许 import service/ 其它包 —— apikey/auth 是接口层的事。
// 配置值一律从 Options 进来，不给 config 开门：本包必须能独立测试。
// 例外（2026-10-11 会话亲和接线）：service/affinity —— Options.Affinity
// 拿的是它的表（方向 proxy→affinity，affinity 只依赖 errclass，不回摸
// 任何 service/*；且开关由装配层决定，构造不出表时就传 nil，本层零感知）。
//
// ── 搬运纪律：逐位等价 ──────────────────────────────────────
// attempt / endpoint 的全部分支条件、语句顺序、常量、日志文案
// （[proxy]、[upstream-error]、[attempt]、[downstream-error]）全部保留。
// 注释里的历史事故记录（10-01 的 429 风暴、10-03 的客户端取消误罚、
// 10-06 的模型下架黑洞）逐字保留。结构性改动只有授权的几类：
// 方法→自由函数、调用点导出化、emitAttempt→store.Emit。
package proxy

import (
	"io"
	"time"

	"shanhai-gateway/internal/service/affinity"
)

// Options 是 proxy 的唯一注入点。
type Options struct {
	// QueueWait 满载排队上限（QUEUE_WAIT，搬入前默认 60 秒）。
	// 满载 = 排队，不是降级；排队全部落空才回 429 + Retry-After。
	QueueWait time.Duration
	// DefaultModel 客户端 model == "auto"（或空）时落到后端 model 的最终兜底。
	// 搬入前是 envOrDefault("DEFAULT_MODEL", "")；搬入后由接线层从配置喂进来。
	DefaultModel string
	// ReasoningEffort 思考模型统一压低推理预算的默认值（REASONING_EFFORT，默认 "low"；
	// 空串 = 不注入）。仅在客户端**没显式指定** reasoning_effort 时注入，尊重显式选择。
	// 与 seika_llm_router(main.go) 同源语义；两侧守卫一致，SLR→gateway 串联也不会双注入。
	ReasoningEffort string
	// LogWriter attempts JSONL 输出，nil 则不写。
	// 注意：真正的 JSONL 写出由 store.Start(ctx, LogWriter) 完成（那是接线层的
	// 生命周期管理），proxy 自身的 HANDLER 只通过 store.Emit 记账；本字段声明在此，
	// 是为了与 QUEUE_WAIT / DEFAULT_MODEL 一起构成对 config 的完整替代注入点。
	LogWriter io.Writer
	// Affinity 会话亲和表（2026-10-11 会话亲和接线）。**可 nil**：
	//   - nil 时行为与未接亲和**逐位一致**——所有亲和分支都以 `opt.Affinity != nil`
	//     短路，既有路径一条不碰（开关在装配层：SESSION_AFFINITY=off 就喂 nil）；
	//   - 非 nil 时只做「优先选择」：命中且目标可用 → 把它排到候选序首位，
	//     不替代轮转；满载不逐出，只滞回计数（affinity 格），连续 K 次才迁移。
	// 亲和不读 config、不发 IO：判逐出也只消费 errclass.ReleaseAffinity。
	Affinity *affinity.Table
}
