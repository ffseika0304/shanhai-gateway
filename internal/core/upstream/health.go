package upstream

import (
	"log"
	"math"
	"sync/atomic"
	"time"

	"shanhai-gateway/internal/core/errclass"
	"shanhai-gateway/internal/runtime"
)

// ===================== 熔断 / 冷却 / 慢探测 =====================
//
// ⚠️ 本文件的每个数字都是生产事故调出来的，**不要顺手优化**：
//
//   - cooldown429=60s / cooldown403=5m / cooldown401=10m：三种配额类错误的基础冷却
//   - cooldownCap=4h：指数退避封顶（锁屏密码逻辑，见 quotaCooldown）
//     历史：10-01 的 403 自锁风暴 —— strike 只在成功时清零，锁住 → 无流量
//     → 无成功 → 永不清零 → 再锁。加封顶 + 指数阶梯就是为了打断这个正反馈。
//   - circuitCooldown=2m：跳闸后半开
//   - slowWindow=2m / slowFailsToOpen=2：连续 2 次首字超时进慢速观察窗
//   - failsToTrip=5：连续 5 次失败跳闸（临时性错误不计）
//   - queueWait=60s：并发位排队上限（满了先排队，不是降级）

var (
	cooldown429      = runtime.EnvDuration("COOLDOWN_429", 60*time.Second)      // 限流后跳过该后端的窗口
	cooldown403      = runtime.EnvDuration("COOLDOWN_403", 300*time.Second)     // 所有 key 都 403 → 大概率套餐/权限问题
	cooldown401      = runtime.EnvDuration("COOLDOWN_401", 600*time.Second)     // 所有 key 都 401 → key 全失效
	cooldownCap      = runtime.EnvDuration("COOLDOWN_MAX", 4*time.Hour)         // 指数冷却封顶（锁屏密码逻辑，见 quotaCooldown）
	circuitCooldown  = runtime.EnvDuration("CIRCUIT_COOLDOWN", 120*time.Second) // 熔断后120秒恢复
	slowWindow       = runtime.EnvDuration("SLOW_WINDOW", 120*time.Second)      // 首字连续超时后进入"慢速观察窗"
	queueWait        = runtime.EnvDuration("QUEUE_WAIT", 60*time.Second)        // 并发位排队上限（满了先排队，不是降级）
	baseFirstTokSec  = runtime.EnvFloat("FIRST_TOKEN_TIMEOUT", 50.0)            // 常规首字超时上限
	probeFirstTokSec = runtime.EnvFloat("FIRST_TOKEN_PROBE", 8.0)               // 观察窗内的短探针超时
	slowFailsToOpen  = int64(2)                                                 // 连续几次首字超时进观察窗
	failsToTrip      = int64(5)                                                 // 连续几次失败跳闸（临时性错误不计）
)

// fallbackMaxFails / fallbackStrikeWin 已于 2026-10-11 搬到
// internal/service/routing/fallback.go：它们是**兜底隔离链**的策略，
// 不是后端健康度的参数，放在这里既无人引用也说不清归属。
//
// 上面这一组 env 名的默认值原样保留：运维靠这些开关应急调参，
// 搬丢了等于把它们从生产里删掉。durEnv/numEnv 两个私有副本已于 2026-10-11
// 删掉，改用 runtime.EnvDuration/EnvFloat（见 internal/runtime/env.go 的注释：
// 解析逻辑逐字照搬，一行都没"改进"）。

// clock 是包内「当前时间」的可替换来源，默认就是 time.Now。
//
// 唯一存在的理由是可测性：退避阶梯是 5m→10m→…→4h，真等一次这个测试就跑不动了。
// 生产路径上它恒等于 time.Now，行为与搬入前逐位一致。
var clock = time.Now

// Trip 熔断跳闸。
func (b *Backend) Trip(now time.Time) {
	atomic.StoreInt64(&b.cbTripped, now.Unix())
	log.Printf("[circuit] %s TRIPPED (%d consecutive fails)", b.Name, atomic.LoadInt64(&b.failStreak))
}

// IsAvailable 跳闸后 circuitCooldown 秒半开
func (b *Backend) IsAvailable(now time.Time) bool {
	t := atomic.LoadInt64(&b.cbTripped)
	if t == 0 {
		return true
	}
	if now.Sub(time.Unix(t, 0)) > circuitCooldown {
		atomic.StoreInt64(&b.cbTripped, 0)
		log.Printf("[circuit] %s HALF-OPEN", b.Name)
		return true
	}
	return false
}

// SetCooldown 设冷却。已有更长的冷却时不缩短（避免短冷却覆盖长冷却）。
func (b *Backend) SetCooldown(d time.Duration, reason Class) {
	t := clock().Add(d).UnixNano()
	if t > atomic.LoadInt64(&b.cooldownUntil) {
		atomic.StoreInt64(&b.cooldownUntil, t)
	}
	b.setCooldownReason(reason)
	log.Printf("[cooldown] %s set %v reason=%s", b.Name, d, reason)
}

// quotaCooldown 配额/权限类错误的冷却 —— 指数退避（锁屏密码逻辑）：
// 连续第 n 次被 429/403/401 → 冷却 = 基础时长 × 2^(n-1)，封顶 COOLDOWN_MAX（默认 4h）；
// 成功一次立即归零（NoteSuccessMetrics 重置），配额恢复后自动回到短冷却。
// 背景（2026-09-24 用户定调）：周配打满时固定 60s 重试 = 每分钟白烧一个探测请求，
// 打进去的排队请求也白等；翻倍后越试越稀，且冷却中的后端在排队**之前**就被跳过。
// 网络/超时不走这里（临时问题，固定 60s，见 NoteFailure），只有配额类才翻倍。
//
// 保持私有的理由：它只有一个调用点 —— 上面那条「配额类先不冷却，试完全部 key
// 之后由调用方统一施加」的编排还没有搬进来（那部分属于请求执行路径，还没到本包）。
// 等编排搬进来时，它会被那个调用点直接使用；现在导出一个没人调用的方法，
// 只会让人以为可以绕过 NoteFailure 直接施冷却。
func (b *Backend) quotaCooldown(c Class) {
	base := cooldown429
	switch c {
	case cls403:
		base = cooldown403
	case cls401:
		base = cooldown401
	}
	n := atomic.AddInt64(&b.cooldownStrike, 1)
	d := base
	for i := int64(1); i < n; i++ {
		d *= 2
		if d <= 0 || d >= cooldownCap { // 到封顶或（异常时）溢出 → 一律取封顶
			d = cooldownCap
			break
		}
	}
	if d > cooldownCap {
		d = cooldownCap
	}
	b.SetCooldown(d, c)
	log.Printf("[cooldown] %s quota-strike=%d → %v (exponential)", b.Name, n, d)
}

func (b *Backend) setCooldownReason(r Class) {
	// 用独立字段存 reason（不引入额外锁，reason 只在日志/统计读）
	b.cooldownReason = r
}

// ClearCooldown 管理员手工解除：冷却、熔断、慢探测窗口一起清。
func (b *Backend) ClearCooldown() {
	atomic.StoreInt64(&b.cooldownUntil, 0)
	atomic.StoreInt64(&b.slowUntil, 0)
	atomic.StoreInt64(&b.cbTripped, 0)
	atomic.StoreInt64(&b.failStreak, 0)
	atomic.StoreInt64(&b.slowStreak, 0)
	atomic.StoreInt64(&b.cooldownStrike, 0)
	b.cooldownReason = ""
	log.Printf("[admin] %s cleared (cooldown/circuit/slow)", b.Name)
}

// CooldownReason 当前冷却的原因（空=没在冷却）。
func (b *Backend) CooldownReason() Class { return b.cooldownReason }

// FailStreak 当前连续失败数（熔断计数器）。
func (b *Backend) FailStreak() int64 { return atomic.LoadInt64(&b.failStreak) }

// SkipReason 判断该后端此刻能否接活（空串 = 可用）。
//
// 这是「能不能接活」的唯一入口：调用方（service/routing 拿候选名去试、
// 看板算健康度）都问它。搬入时曾拆成小写本体 + 导出版两份，现已合并。
func (b *Backend) SkipReason(now time.Time) Class {
	if !b.Enabled {
		return clsSkipDisable
	}
	if !b.IsAvailable(now) {
		return clsSkipCircuit
	}
	if now.UnixNano() < atomic.LoadInt64(&b.cooldownUntil) {
		return clsSkipCool
	}
	return ""
}

// FirstTokenTimeout 本次请求允许等首字多久；观察窗内会用短探针，
// 避免每请求白等 50s。
func (b *Backend) FirstTokenTimeout(now time.Time) time.Duration {
	base := time.Duration(math.Min(b.Timeout, baseFirstTokSec)) * time.Second
	if b.Timeout <= 0 {
		base = time.Duration(baseFirstTokSec) * time.Second
	}
	if now.UnixNano() < atomic.LoadInt64(&b.slowUntil) {
		p := time.Duration(probeFirstTokSec) * time.Second
		if p < base {
			return p
		}
	}
	return base
}

// NoteFailure 把一次上游失败记进后端状态机。
//
// 本函数的结构与搬入前逐条对应；只是把「哪一类该怎么处置」的问句提到了
// errclass/policy.go（CountsAsFault / CooldownKindOf / SoftFailure /
// Transience / ClientFault / SlowProbeTrigger），执行动作仍留在本文件。
//
// 注意 failStreak 的口径**不是** CountsAsFault：熔断看的是「连续失败」，
// 而 CountsAsFault 看的是「兜底隔离是否记账」。搬入前两者就是两套口径，
// 这里保持原样，不合并。
//
// clientGone = 下游 context 已 done（客户端主动断开/连接断了）。此时上游那句
// "context canceled" 只是"我们不再读了"，不是后端故障，不该罚它冷却。
func (b *Backend) NoteFailure(c Class, now time.Time, msg string, status int, clientGone bool) {
	// 软失败：空气泡类（内容为空）多为请求侧原因（如 max_tokens 太小导致
	// reasoning 模型把预算烧光），不代表后端不可用。仍然切后端兜底，
	// 但不计入连续失败、不跳闸——否则一次小预算请求就能把健康后端踢下线。
	if errclass.SoftFailure(c) {
		atomic.StoreInt64(&b.slowStreak, 0)
		b.stats.Bump(c)
		b.stats.SetLastError(c, status, msg)
		return
	}
	// 网络/超时类错误：临时性问题，但进入1分钟冷却，别硬挤
	// context canceled 是客户端取消，context deadline exceeded 是超时
	if errclass.Transience(c) {
		atomic.StoreInt64(&b.slowStreak, 0)
		b.stats.Bump(c)
		b.stats.SetLastError(c, status, msg)
		// 客户端自己走了：后端一句错都没说过，照罚它冷却是冤枉。
		// 历史上就是这么干的 → 一次客户端断连让所有在飞后端同时冷却 60s，
		// 候选列表被清空 → 整网 1 分钟 503（2026-10-03 00:12 实证 1720 次）。
		// 这条规则原先挂在一个恒返回 true 的 errclass.ClientGoneSkipCooldown()
		// 开关后面，2026-10-11 已删掉那个开关（clientGone && f() ≡ clientGone，纯噪音），
		// 规则本身原样留在这里，回归测试见 upstream_test.go TestClientGoneSkipsCooldown。
		if clientGone {
			log.Printf("[downstream-error] backend=%s class=%s 客户端已断开，跳过 60s 冷却（不计入后端故障）", b.Name, c)
			return
		}
		// 进入1分钟冷却，别硬挤
		b.SetCooldown(60*time.Second, c)
		return
	}
	// 400 = 请求本身有问题（客户端锅：body/model/tool_choice 不合口味），后端是健康的。
	// 换后端是对的，但绝不能计入后端 failStreak —— 否则客户端的 400 会把健康后端熔断掉。
	if errclass.ClientFault(c) {
		atomic.StoreInt64(&b.slowStreak, 0)
		b.stats.Bump(c)
		b.stats.SetLastError(c, status, msg)
		return
	}
	// 首字超时喂慢速观察窗（不是熔断：后端还在响应，只是慢）
	if errclass.SlowProbeTrigger(c) {
		if atomic.AddInt64(&b.slowStreak, 1) >= slowFailsToOpen {
			t := now.Add(slowWindow).UnixNano()
			atomic.StoreInt64(&b.slowUntil, t)
			log.Printf("[slow] %s enter probe window (%v): repeated first-token timeouts",
				b.Name, slowWindow)
		}
	} else {
		atomic.StoreInt64(&b.slowStreak, 0)
	}
	if atomic.AddInt64(&b.failStreak, 1) >= failsToTrip && atomic.LoadInt64(&b.cbTripped) == 0 {
		b.Trip(now)
	}
	b.stats.Bump(c)
	b.stats.SetLastError(c, status, msg)
}

// NoteSuccessMetrics 成功一次：所有惩罚性计数归零（"密码输对了"），
// 并记下这次的延迟。
func (b *Backend) NoteSuccessMetrics(latMs, ftMs int64) {
	atomic.StoreInt64(&b.failStreak, 0)
	atomic.StoreInt64(&b.slowStreak, 0)
	atomic.StoreInt64(&b.slowUntil, 0)
	atomic.StoreInt64(&b.cooldownStrike, 0) // 成功一次 = "密码输对了解锁"：指数冷却归零
	b.stats.noteSuccess()
	b.stats.NoteLatency(latMs)
	if ftMs > 0 {
		b.stats.NoteFirstToken(ftMs)
	}
}
