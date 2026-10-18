package errclass

// 本文件把「一类错误该怎么处置」从状态机的动作里剥出来，变成可以单独提问的判定。
//
// 为什么值得单独抽一层：搬入前这些决策全部内联在 upstream 的 noteFailure 里，
// 混在「判断 + 改状态 + 打日志」三件事中间。改策略（换一家上游的语义、调整
// 熔断口径）就必须去动状态机，而状态机里每一行都压着生产事故调出来的常量。
// 抽出来之后：改策略只碰本文件，碰不到任何一把锁、任何一个计时器。
//
// **行为等价性是硬要求**：本文件的每个函数都必须与搬入前的内联判断逐位一致。
// 抽出来不是为了拆而拆，是为了让「独立演进的策略」有一个可以独立演进的位置。

// CooldownKind 一类错误要哪种冷却。
type CooldownKind int

const (
	// CooldownNone 不冷却。只记账、可能涨 failStreak。
	CooldownNone CooldownKind = iota
	// CooldownTransient 固定 60 秒。网络/超时这类临时问题，冷一下再来，
	// 不做指数退避 —— 退避对临时性抖动是错的（抖一下退 4 小时）。
	CooldownTransient
	// CooldownQuotaExponential 配额/权限类指数退避（锁屏密码逻辑）：
	// 429/403/401 连续第 n 次 → 基础时长 × 2^(n-1)，封顶 4 小时。
	CooldownQuotaExponential
)

func (k CooldownKind) String() string {
	switch k {
	case CooldownTransient:
		return "transient"
	case CooldownQuotaExponential:
		return "quota_exponential"
	default:
		return "none"
	}
}

// CooldownKindOf 判定这一类错误该配哪种冷却。
//
// 注意 429/401/403 在 noteFailure 里**不设冷却** —— 它们的指数退避由调用方在
// 试完全部 key 之后统一施加（upstream 的 quotaCooldown），因为「这个后端整体
// 配额打满」这件事只有把该后端所有 key 都试过一遍才能确认。
// 这里仍然把它们归到 CooldownQuotaExponential，是为了让本包成为
// 「哪类需要配额级退避」的唯一答案来源，调用方据此决定何时施加。
func CooldownKindOf(c Class) CooldownKind {
	switch c {
	case Network, Timeout:
		return CooldownTransient
	case RateLimit429, Auth401, Forbidden403:
		return CooldownQuotaExponential
	default:
		return CooldownNone
	}
}

// CountsAsFault 判断兜底故障是否计入隔离（只数「上游真错」）：
//   - 400 = 客户端请求本身有问题（下游的锅），后端是健康的
//   - 空气泡类 = 软失败（多为 max_tokens 太小），切后端但不该记账
//   - network = 下游取消 / 本地网络抖动（临时问题）
//   - skip_* = 没真正发请求
//
// 与 sidecar、SCF breaker 同一原则：我方/下游的错不让上游背锅。
func CountsAsFault(c Class) bool {
	switch c {
	case BadRequest400, EmptyOutput, NoContent, Network:
		return false
	}
	return !c.IsSkip()
}

// SoftFailure 空气泡类（内容为空）。多为请求侧原因（如 max_tokens 太小导致
// reasoning 模型把预算烧光），不代表后端不可用。仍然切后端兜底，
// 但不计入连续失败、不跳闸 —— 否则一次小预算请求就能把健康后端踢下线。
func SoftFailure(c Class) bool {
	switch c {
	case EmptyOutput, NoContent:
		return true
	}
	return false
}

// ClientFault 请求本身的错。后端是健康的，换后端兜底是对的，
// 但绝不能计入后端 failStreak —— 否则客户端的 400 会把健康后端熔断掉。
//
// ⚠️ ModelUnavailable 不在此列：它也回 400，但那是**后端**的模型没了，
// 必须计入（2026-10-06 改判，就是为了不再出现静默黑洞）。
func ClientFault(c Class) bool {
	return c == BadRequest400
}

// Transience 网络/超时类：临时性问题，固定冷却但不涨 failStreak。
// 真正的连续失败留给配额类和硬错误去判断 —— 一次网络抖动不该把后端熔断掉。
func Transience(c Class) bool {
	switch c {
	case Network, Timeout:
		return true
	}
	return false
}

// SlowProbeTrigger 是否喂给「慢速观察窗」。
// 首字超时是限流/慢流的典型表现：连续慢不该立刻跳闸（后端还在响应），
// 但也不该无限制地等 50 秒 —— 攒够次数就切短探针，快速确认它是不是真的慢了。
func SlowProbeTrigger(c Class) bool {
	return c == FirstTokenTimeout
}

// BreaksKeyLoop 判定「这一类错误不该再在同后端换 key 试了，直接换后端」。
//
// 搬入前是请求路径里两段并列的 if：
//
//	if res.class == cls400 || res.class == cls404 || res.class == clsModelGone { break }
//	if !keyRotatable(res.class) { break }
//
// 合成一个判定：KeyRotatable 的补集 ∪ {400,404,model_unavailable}。
// 由于 400/404/model_unavailable 本来就不在可轮转集合里，
// 结果与搬入前逐位一致 —— 但意图变得可读了：不是「key 不可换」，
// 而是「这类错误换 key 是白费力气」。
func BreaksKeyLoop(c Class) bool {
	switch c {
	case BadRequest400, NotFound404, ModelUnavailable:
		return true
	}
	return !KeyRotatable(c)
}

// ImmediateCooldown 判定「一票否决」：不管同后端还有没有别的 key 要试，
// 立刻跳出 key 循环并立刻冷却。只有 429 是这样 —— 配额已经打满了，
// 换 key 也是 429，不如直接换后端别浪费往返。
func ImmediateCooldown(c Class) bool {
	return c == RateLimit429
}

// ReleaseAffinity 返回「这一次失败是否应该放弃该会话的粘性」。
//
// 背景：会话亲和把同一对话的后续请求优先钉回同一 backend（救回 provider
// 侧的前缀缓存命中）。钉得有多松多紧，与「这次失败算谁的错」必须**同源**
// 判定 —— 本函数只消费已有的分类，不发明第二个健康层。
//
// 与表的能力对应：
//   - ImmediateCooldown 类（429）                     → 放弃，逐出
//     （配额，不是本轮网络；钉在限流的后端就是把会话钉在坏地方）
//   - ClientFault（400）                              → 不动
//     （客户端的锅，别迁怒 backend）
//   - Transience（network/timeout）                   → 不动
//     （60 秒冷却自己会回来，逐掉 locality 是损失；请求会走兜底，
//     但表保持在原 backend——下一轮回来接着命中前缀缓存）
//   - ClientGone / Cancelled（class 上表现为 network） → 不动
//     （响应都没开始，连失败都算不上；mid-stream 已吐字后断流 → 亲和
//     完全不受影响——响应已经开始，根本没切换 backend，谈不上拆。
//     都归到「不动」这一侧，接线层另有 !res.wrote 双保险）
//   - 软失败（NoContent / EmptyOutput 空气泡）         → 不动
//     （与 SoftFailure 同源：多为请求侧原因，比如 max_tokens 太小，
//     后端本来是健康的）
//   - 其它（5xx / 408/504 / 401 / 403 / 404 / model_unavailable / 200+error_body
//     / 首字超时）                                     → 放弃
//     （这些是后端侧的错——模型下架是后端的锅，5xx 是后端打歪了，
//     配额类与 429 同理；继续把会话钉在它们身上 = 钉死在坏后端）
//   - skip_* 类（路由决策，没真正发请求）              → 不动
//     （路由决策不是失败；满载的逐出走滞回 contention 计数，不走这里）
//
// ⚠️ 本判定与 CountsAsFault / CooldownKindOf 同源不同问：它们问「要不要罚
// 后端 / 怎么罚」，本函数问「这次失败要不要换地方钉」。判罚轻轻放过的
// （400、瞬时网络、空气泡）在这里也一律不拆钉——一条贯穿的口径。
func ReleaseAffinity(c Class) bool {
	switch c {
	case BadRequest400, Network, Timeout,
		MidStreamFail, NoContent, EmptyOutput:
		return false
	}
	if c.IsSkip() {
		return false
	}
	return true
}
