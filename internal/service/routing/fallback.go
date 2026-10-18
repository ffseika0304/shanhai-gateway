package routing

import (
	"log"
	"strings"
	"sync/atomic"
	"time"

	"shanhaigateway/internal/core/errclass"
	"shanhaigateway/internal/runtime"
)

var (
	// fallbackMaxFails 兜底后端在滑动窗口内的上游故障次数上限，达到则隔离一个窗口
	//
	// 从 core/upstream 搬到这里（2026-10-11）：这两个数字是**兜底隔离链**的策略，
	// 不是后端健康度的参数 —— 它们回答的是「兜底后端连错几次先别用了」，
	// 而不是「这个后端熔断要连续错几次」。放在 upstream 里既没人引用，
	// 也说不清归属；搬到 routing 它们才和用它们的 recordFallbackFailure /
	// fallbackQuarantined 待在一起。
	fallbackMaxFails = int64(3)

	// fallbackStrikeWin 兜底故障聚拢窗 = 隔离窗（过窗自动放回）
	fallbackStrikeWin = runtime.EnvDuration("FALLBACK_STRIKE_WINDOW", 10*time.Minute)
)

// durEnv 的私有副本已于 2026-10-11 删掉，改用 runtime.EnvDuration。
// 搬入时它和 core/upstream/health.go 里的同名函数逐字相同（两处各抄一份是
// 照搬的结果，不是重构没做完）。现在两处都只有一个来源：internal/runtime/env.go。

// fbFault 一个兜底后端在隔离窗口内的记账。
type fbFault struct {
	strike int64 // 窗口内累计的上游故障次数
	last   int64 // 最后一次故障时间（unix 秒）
}

// countsAsFault 判断兜底故障是否计入隔离（只数"上游真错"）：
//   - 400 = 客户端请求本身有问题（下游的锅），后端是健康的
//   - 空气泡类 = 软失败（多为 max_tokens 太小），切后端但不该记账
//   - network = 下游取消 / 本地网络抖动（临时问题）
//   - skip_* = 没真正发请求
//
// 与 sidecar、SCF breaker 同一原则：我方/下游的错不让上游背锅。
func countsAsFault(c errclass.Class) bool {
	switch c {
	case errclass.BadRequest400, errclass.EmptyOutput, errclass.NoContent, errclass.Network:
		return false
	}
	return !strings.HasPrefix(string(c), "skip_")
}

// RecordFallbackFailure 记录兜底上游故障（滑动窗口聚拢，过窗自动清账）。
//
// 软失败（空气泡 / 客户端断开 / 没真发出去的 skip_*）由 countsAsFault 挡在门外。
func (rt *Router) RecordFallbackFailure(name string, c errclass.Class) {
	if !countsAsFault(c) {
		return
	}
	now := time.Now()
	rt.mu.Lock()
	f := rt.fallbackFaults[name]
	if f == nil {
		f = &fbFault{}
		rt.fallbackFaults[name] = f
	}
	if f.last > 0 && now.Sub(time.Unix(f.last, 0)) > fallbackStrikeWin {
		f.strike = 0 // 上次故障已过窗：之前的账清掉，临时抖动不滚雪球
	}
	f.strike++
	f.last = now.Unix()
	n, hit := f.strike, f.strike >= fallbackMaxFails
	rt.mu.Unlock()
	if hit {
		log.Printf("[router] fallback %s QUARANTINED: %d upstream faults in %v (auto-return after window)", name, n, fallbackStrikeWin)
	}
}

// RecordFallbackSuccess 兜底成功 → 立即清账（"密码输对了"）。
//
// 与健康度的 cooldownStrike 语义对齐：成功一次 = "密码输对了解锁"。
func (rt *Router) RecordFallbackSuccess(name string) {
	rt.mu.Lock()
	if f := rt.fallbackFaults[name]; f != nil {
		f.strike, f.last = 0, 0
	}
	rt.mu.Unlock()
}

// FallbackQuarantined 是否处于隔离中；已过窗则顺手清账并放行（锁屏到期自动解锁）。
//
// 顺带回答「隔离窗口还剩多久」这类看板问题。
func (rt *Router) FallbackQuarantined(name string, now time.Time) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	f := rt.fallbackFaults[name]
	if f == nil || f.strike < fallbackMaxFails {
		return false
	}
	if f.last > 0 && now.Sub(time.Unix(f.last, 0)) > fallbackStrikeWin {
		f.strike, f.last = 0, 0
		log.Printf("[router] fallback %s quarantine expired → back in rotation", name)
		return false
	}
	return true
}

// GetFallbackCandidates 获取兜底后端候选（仅在主轮转后端满载时使用）。
//
// 2026-09-24（批次3）：返回**全部**可用兜底、本次从轮转起点开始——原来只回 1 个，
// 第 1 个满载就放弃分流去排队，后面的兜底明明闲着。调用方逐个 tryAcquire，
// 一个顶上就停；全部满载才排队（排队语义不变）。
//
// 与首发候选的分工（搬入前就是这样）：兜底候选**本包自己过滤**（查 SkipReason
// + 隔离状态），因为兜底是「主线满了才去挤」，必须当场就排除掉不可用的，
// 否则调用方逐个 tryAcquire 时会在一个明知冷却的后端上空转一轮。
// 这里返回 nil（而不是 panic）是正常的 —— 全部兜底都不可用正是调用方
// 要转去排队的那个信号。
func (rt *Router) GetFallbackCandidates() []string {
	fallbacks := []string{}
	now := time.Now()
	for _, b := range rt.backends {
		if b.IsFallback && !rt.FallbackQuarantined(b.Name, now) && b.SkipReason(now) == "" {
			fallbacks = append(fallbacks, b.Name)
		}
	}
	if len(fallbacks) == 0 {
		return nil
	}
	// 轮转起点：每次调用后移一位，兜底之间负载依旧公平
	off := int(atomic.AddUint64(&rt.fallbackSeq, 1)-1) % len(fallbacks)
	out := make([]string, 0, len(fallbacks))
	for i := range fallbacks {
		out = append(out, fallbacks[(off+i)%len(fallbacks)])
	}
	return out
}
