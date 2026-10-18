package upstream

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ===================== 内存态细粒度统计 =====================
//
// 全部是内存计数，不落盘、不发 Prometheus —— 看板层按需快照。
// 环形缓冲的容量 256 与搬入前一致（照旧）。

// BackendStats 单个后端的内存统计。
type BackendStats struct {
	attempts  int64
	success   int64
	recent60s int64
	counts    map[Class]*int64

	latMu   sync.Mutex
	latBuf  []int64
	latPos  int
	ftMu    sync.Mutex
	ftBuf   []int64
	ftPos   int
	errMu   sync.Mutex
	lastErr string
	lastCls Class
	lastSt  int
	lastAt  time.Time
}

func newBackendStats() *BackendStats {
	s := &BackendStats{counts: make(map[Class]*int64), latBuf: make([]int64, 256), ftBuf: make([]int64, 256)}
	for _, c := range classList {
		v := int64(0)
		s.counts[c] = &v
	}
	return s
}

// Stats 返回该后端的统计对象（搬入前是直接读 b.stats 字段）。
func (b *Backend) Stats() *BackendStats { return b.stats }

// Bump 记一次该类错误（搬入前调用点直接用 stats.bump）。
func (s *BackendStats) Bump(c Class) {
	if p, ok := s.counts[c]; ok {
		atomic.AddInt64(p, 1)
	}
}

// NoteAttempt 记一次请求尝试。
func (s *BackendStats) NoteAttempt() {
	atomic.AddInt64(&s.attempts, 1)
	atomic.AddInt64(&s.recent60s, 1)
}

// noteSuccess 成功计数。它没有导出版本，因为唯一的调用点是
// Backend.NoteSuccessMetrics（成功一次要同时清惩罚计数 + 记延迟，
// 拆开单发这个方法只会让人有机会只记成功、不清冷却）。
func (s *BackendStats) noteSuccess() { atomic.AddInt64(&s.success, 1) }

// Attempts 累计尝试次数。
func (s *BackendStats) Attempts() int64 { return atomic.LoadInt64(&s.attempts) }

// Success 累计成功次数。
func (s *BackendStats) Success() int64 { return atomic.LoadInt64(&s.success) }

// Count 该类错误累计次数。
func (s *BackendStats) Count(c Class) int64 {
	if p, ok := s.counts[c]; ok {
		return atomic.LoadInt64(p)
	}
	return 0
}

// ResetRecent60s 每分钟由看板层调用：把 60 秒速率计数清零（搬入前是裸 atomic.Store）。
func (s *BackendStats) ResetRecent60s() { atomic.StoreInt64(&s.recent60s, 0) }

// Recent60s 近 60 秒的请求数（用于算 RPM）。
func (s *BackendStats) Recent60s() int64 { return atomic.LoadInt64(&s.recent60s) }

// NoteLatency 记一次总延迟（毫秒）。
func (s *BackendStats) NoteLatency(ms int64) {
	s.latMu.Lock()
	s.latBuf[s.latPos] = ms
	s.latPos = (s.latPos + 1) % len(s.latBuf)
	s.latMu.Unlock()
}

// NoteFirstToken 记一次首字延迟（毫秒）。
func (s *BackendStats) NoteFirstToken(ms int64) {
	s.ftMu.Lock()
	s.ftBuf[s.ftPos] = ms
	s.ftPos = (s.ftPos + 1) % len(s.ftBuf)
	s.ftMu.Unlock()
}

// SetLastError 记最后一次错误（看板展示用）。
func (s *BackendStats) SetLastError(c Class, status int, msg string) {
	s.errMu.Lock()
	s.lastCls, s.lastSt, s.lastErr, s.lastAt = c, status, msg, clock()
	s.errMu.Unlock()
}

// LastError 最近一次错误：(分类, HTTP 状态码, 报文, 时间)。
func (s *BackendStats) LastError() (Class, int, string, time.Time) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.lastCls, s.lastSt, s.lastErr, s.lastAt
}

// percentile 取环形缓冲的有效样本（>0）里的第 p 百分位。
//
// 用最近邻取整（ceil 后 -1）而不是插值：延迟看的是「最坏的那批请求有多慢」，
// 插值出来的 p99 是个现实里不存在的数，反而会误导告警阈值。
//
// pos 是搬入前就存在的形参（原签名 percentile(buf, pos, p)），函数体内一直没用它。
// 照旧保留 —— 搬入不改签名，省得日后 diff 里混进与本次无关的改动。
func percentile(buf []int64, pos int, p float64) int64 {
	_ = pos
	var vals []int64
	for _, v := range buf {
		if v > 0 {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	idx := int(math.Ceil(float64(len(vals))*p/100.0)) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(vals) {
		idx = len(vals) - 1
	}
	return vals[idx]
}

func (s *BackendStats) LatencyPcts() (p50, p95, p99 int64) {
	s.latMu.Lock()
	buf, pos := s.latBuf, s.latPos
	s.latMu.Unlock()
	rotated := append([]int64{}, buf[pos:]...)
	rotated = append(rotated, buf[:pos]...)
	return percentile(rotated, 0, 50), percentile(rotated, 0, 95), percentile(rotated, 0, 99)
}

// FirstTokenPcts 首字延迟的 p50/p95/p99。
func (s *BackendStats) FirstTokenPcts() (p50, p95, p99 int64) {
	s.ftMu.Lock()
	buf, pos := s.ftBuf, s.ftPos
	s.ftMu.Unlock()
	rotated := append([]int64{}, buf[pos:]...)
	rotated = append(rotated, buf[:pos]...)
	return percentile(rotated, 0, 50), percentile(rotated, 0, 95), percentile(rotated, 0, 99)
}
