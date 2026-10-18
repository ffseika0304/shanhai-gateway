package upstream

import (
	"context"
	"sync/atomic"
	"time"
)

// ===================== 并发位（容量）=====================
//
// 搬运纪律：本文件三个数字/常量都是生产实测出来的，照旧。
//   - Concurrency 默认 16（配置缺省）
//   - freeCh 缓冲 256（见 backend.go freeChBuf）
//   - 排队上限 queueWait 默认 60 秒（见 health.go）

// tryAcquire 非阻塞地抢一个并发位。抢不到就立刻返回 false。
//
// 为什么用 CAS 循环而不是 Mutex：这是代理请求的最热路径，每个请求都要过一次。
// CAS 在竞争低时（常态：槽位比请求多）比抢一把全局锁便宜得多，
// 且没有「一个慢请求把整个后端拖住」的可能。
func (b *Backend) tryAcquire() bool {
	for {
		c := atomic.LoadInt64(&b.curConc)
		if c >= int64(b.Concurrency) {
			return false
		}
		if atomic.CompareAndSwapInt64(&b.curConc, c, c+1) {
			return true
		}
	}
}

// acquireWait 等一个并发位。
// 关键语义：**满了不等于降级** —— 任务被分配到哪个上游就该在哪个上游排队等，
// 而不是溢出到下一个上游（那样等于把"容量"当成了故障，会打乱分配、
// 还会让下游上游莫名其妙吃到它不该吃的流量）。只有真报错(429/403/5xx/超时/空气泡)才切走。
// 排队超时(QUEUE_WAIT)才由调用方决定换后端，作为兜底安全阀。
func (b *Backend) acquireWait(ctx context.Context, wait time.Duration) bool {
	if b.tryAcquire() {
		return true
	}
	if wait <= 0 {
		return false
	}
	atomic.AddInt64(&b.waiters, 1)
	defer atomic.AddInt64(&b.waiters, -1)
	deadline := time.Now().Add(wait)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		timer := time.NewTimer(remaining)
		select {
		case <-b.freeCh: // 有人让出并发位 → 重试抢占
			timer.Stop()
			if b.tryAcquire() {
				return true
			}
		case <-timer.C:
			return false
		case <-ctx.Done(): // 客户端断开，不再排队
			timer.Stop()
			return false
		}
	}
}

// release 归还一个并发位，并唤醒一个排队者。
//
// 必须与 tryAcquire/acquireWait 严格配对：请求拿到位之后任何路径都要归还，
// 少还一个就会让这个后端慢慢「漏满」直至彻底不可用。
func (b *Backend) release() {
	atomic.AddInt64(&b.curConc, -1)
	// 唤醒一个排队者（非阻塞，避免释放路径被卡住）
	select {
	case b.freeCh <- struct{}{}:
	default:
	}
}

// InFlight 当前在飞请求数（搬入前的 atomic.LoadInt64(&b.curConc)，封装一下免得调用点散落裸 atomic）。
func (b *Backend) InFlight() int64 { return atomic.LoadInt64(&b.curConc) }

// Waiters 当前排队等并发位的请求数。
func (b *Backend) Waiters() int64 { return atomic.LoadInt64(&b.waiters) }
