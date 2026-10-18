package upstream

// 2026-10-11：service/proxy（请求执行路径）搬入后需要这四个动作，
// 而它们在 slot.go / health.go 里是私有方法。按搬运纪律**不复制逻辑**，
// 只在原方法之上加导出薄封装 —— 一行都不多。

import (
	"context"
	"sync/atomic"
	"time"
)

// TryAcquire 非阻塞抢占一个并发位（导出薄封装，见 tryAcquire）。
func (b *Backend) TryAcquire() bool { return b.tryAcquire() }

// AcquireWait 等一个并发位（导出薄封装，见 acquireWait）：
// 满了是排队，不是降级；队列上限由调用方传入（QUEUE_WAIT）。
func (b *Backend) AcquireWait(ctx context.Context, wait time.Duration) bool {
	return b.acquireWait(ctx, wait)
}

// Release 归还一个并发位并唤醒一个排队者（导出薄封装，见 release）。
func (b *Backend) Release() { b.release() }

// QuotaCooldown 对配额/权限类错误施加指数退避冷却（导出薄封装，见 quotaCooldown）。
// 调用点：该后端所有 key 都试完并失败之后（见 service/proxy/handler.go）。
func (b *Backend) QuotaCooldown(c Class) { b.quotaCooldown(c) }

// ── 只读观察口（2026-10-11 看板接线加，不改任何状态机逻辑）────────
// 公式与 slr main.go /api/stats 的内联计算逐字对应，只是把读私有字段
// 的那段搬成了方法 —— 看板（internal/api）在 core 之外，不该伸手进
// 私有字段。

// CircuitOpen 熔断是否处于跳闸状态（cbTripped 非零）。
func (b *Backend) CircuitOpen() bool { return atomic.LoadInt64(&b.cbTripped) != 0 }

// CooldownRemain 冷却剩余秒数；未冷却或已到期返回 0。
func (b *Backend) CooldownRemain(now time.Time) float64 {
	remain := float64(atomic.LoadInt64(&b.cooldownUntil)-now.UnixNano()) / 1e9
	if remain < 0 {
		remain = 0
	}
	return remain
}
