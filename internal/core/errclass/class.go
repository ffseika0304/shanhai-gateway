// Package errclass 是「一次失败算什么」的**唯一**判定处。
//
// 从 seika_llm_router 调度内核原样搬入。它只做两件事，且刻意做到零依赖：
//
//  1. 命名：把上游五花八门的失败（HTTP 状态码、流中断、空气泡、客户端断连……）
//     收敛成固定的若干个类别 Class。命名统一之后，「要不要冷却」「算不算故障」
//     这类问题才有地方问。
//  2. 判定：类别 → 状态码映射、「模型不存在」识别、以及各类错误的处置决策
//     （见 policy.go）。
//
// **依赖规则：本包只用标准库，不 import 任何项目内包，不碰 IO。**
// 理由不是洁癖 —— 而是错误语义是独立演进的：换一家上游、改一版网关，它们的
// 「400 到底是客户端的锅还是模型下架」就会变。这块策略不该牵动状态机里任何
// 一行锁、任何一个计时器。把它关进一个没有依赖的盒子里，改它才不会拖垮别人。
//
// 零 IO 还带来一个直接好处：整张策略表可以用表驱动测试逐条钉死，不需要起
// 服务、不需要打桩网络 —— 上游语义一旦改，测试立刻红给你看。
//
// ── 命名对照（搬入前的内部标识 → 本包导出名）────────────────────────────
//
//	clsNetwork→Network          clsTimeout→Timeout
//	clsFirstTok→FirstTokenTimeout   cls429→RateLimit429
//	cls401→Auth401              cls403→Forbidden403
//	cls400→BadRequest400        cls404→NotFound404
//	cls408→UpstreamTimeout      cls5xx→ServerError5xx
//	clsErrBody→ErrorBody        clsNoContent→NoContent
//	clsEmptyOut→EmptyOutput     clsMidStream→MidStreamFail
//	clsModelGone→ModelUnavailable
//	clsSkipCool→SkipCooldown    clsSkipCap→SkipCapacity
//	clsSkipCircuit→SkipCircuit  clsSkipDisable→SkipDisabled
//
// **字符串值一律未改** —— 它们会落进 SQLite 指标表和看板，改了等于历史数据断层。
package errclass

import "strings"

// Class 是一次失败的分类。
//
// 它是 string 而不是 int：这些值会被直接写进指标库、看板和日志，
// 事后翻一条 2026-10-03 的记录，只能靠字面意思看懂。
type Class string

const (
	Network           Class = "network"             // 连不上 / transport error
	Timeout           Class = "timeout"             // 客户端整体超时
	FirstTokenTimeout Class = "first_token_timeout" // 首字超时（限流慢流的典型表现）
	RateLimit429      Class = "rate_limit_429"
	Auth401           Class = "auth_401"
	Forbidden403      Class = "forbidden_403"
	BadRequest400     Class = "bad_request_400"
	NotFound404       Class = "not_found_404"
	UpstreamTimeout   Class = "upstream_timeout" // 上游 408 / 504
	ServerError5xx    Class = "server_error_5xx"
	ErrorBody         Class = "error_body"      // 200 但 body 是 {"error":...}
	NoContent         Class = "no_content"      // 缺 choices / 流里没内容
	EmptyOutput       Class = "empty_output"    // 有 choices 但内容/tool_calls 都空
	MidStreamFail     Class = "mid_stream_fail" // 已吐字后断流（不可再切后端）

	// ModelUnavailable：上游说「这个模型不存在 / 已下架」。**是后端的锅，不是客户端的**，
	// 但它回的是 400 —— 旧代码一律归 BadRequest400 → CountsAsFault=false → 不涨 failStreak、
	// 不跳闸 → 该后端变成静默黑洞（2026-10-06 起 bunny 两条 0 成功率空转 4 天半的根因）。
	ModelUnavailable Class = "model_unavailable"

	// 跳过类（没真正发请求）
	SkipCooldown Class = "skip_cooldown"
	SkipCapacity Class = "skip_capacity"
	SkipCircuit  Class = "skip_circuit"
	SkipDisabled Class = "skip_disabled"
)

// List 是全部错误分类，顺序与搬入前一致。
//
// 用途：给每个后端预分配一份「每类各一个计数器」的零值表，
// 免得每记一笔就去 map 里查一次 key、还得判空。
var List = []Class{
	Network, Timeout, FirstTokenTimeout, RateLimit429, Auth401, Forbidden403,
	BadRequest400, NotFound404, UpstreamTimeout, ServerError5xx, ErrorBody, NoContent,
	EmptyOutput, MidStreamFail, ModelUnavailable,
	SkipCooldown, SkipCapacity, SkipCircuit, SkipDisabled,
}

func (c Class) String() string { return string(c) }

// IsSkip 判断「跳过类」——请求根本没发出去，只是路由决策（禁用/冷却/熔断/满并发）。
// 这类计数不该算进失败数，否则熔断一开就刷出上万条假失败
// （2026-09-30 修：wb_cn_ds41 因 skip_circuit 虚报 20,152 条「失败」，真失败仅 660）。
func (c Class) IsSkip() bool {
	switch c {
	case SkipCooldown, SkipCapacity, SkipCircuit, SkipDisabled:
		return true
	}
	return false
}

// ClassifyStatus 把上游 HTTP 状态码映射到错误分类。
//
// ⚠️ default 兜底是 ServerError5xx 而不是 Unknown：看板和处置逻辑里
// 「说不清是什么错」按最坏情况处理（要冷却、要切后端）比乐观假设安全。
func ClassifyStatus(status int) Class {
	switch {
	case status == 401:
		return Auth401
	case status == 403:
		return Forbidden403
	case status == 429:
		return RateLimit429
	case status == 404:
		return NotFound404
	case status == 408, status == 504:
		return UpstreamTimeout
	case status == 400:
		return BadRequest400
	case status >= 500:
		return ServerError5xx
	default:
		return ServerError5xx
	}
}

// ModelUnavailableBody 判断上游回包是不是「这个模型不存在 / 已下架」。
// 上游看到的一定是**我们配置里写死的 model**（下游 model 覆写逻辑保证：后端配了
// model 就无条件改写请求里的 model 键），所以命中
// 就等于「这个后端的模型没了」——**后端的故障**，换 key 没用、换后端才有用。
func ModelUnavailableBody(body string) bool {
	s := strings.ToLower(body)
	for _, k := range []string{
		"model is unavailable",
		"model not found",
		"model_not_found",
		"model does not exist",
		"unknown model",
		"no such model",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// NoCreditBody 判断 403 body 是否「欠费/额度耗尽」（永久失效，应拉黑）。
// 全局默认词汇；某家要覆盖走 quirk.Classify。
//
// 这份词表原先内联在 config/validate.go 的探测 403 分支里（overdue/balance/
// insufficient/quota/欠费/额度）——挪进来是为了让它和 ModelUnavailableBody
// 一样成为**全局默认词汇**：errclass 保持「纯策略 + 默认词表」，quirk 是覆盖层。
// 挪动只改归属，不改判定：逐个子串、ToLower 之后 Contains，语义与内联版逐位一致。
func NoCreditBody(body string) bool {
	s := strings.ToLower(body)
	for _, k := range []string{
		"overdue",
		"balance",
		"insufficient",
		"quota",
		"欠费",
		"额度",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// KeyRotatable 这些类才是「换同后端的另一个 key 可能有效」。
//
// 理由：401/403 是这一个 key 的问题，429 是配额的，5xx/408 大概率是后端这一下
// 打歪了 —— 换个 key 还有救。而 400/404 是**请求本身**不被接受，
// 换一万个 key 结果都一样，只能换后端。
func KeyRotatable(c Class) bool {
	switch c {
	case Auth401, Forbidden403, RateLimit429, ServerError5xx, UpstreamTimeout:
		return true
	}
	return false
}
