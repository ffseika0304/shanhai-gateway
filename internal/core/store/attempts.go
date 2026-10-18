package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ── 常量搬入即定：三个数字来自旧实现，不要顺手改 ──────────────────
const (
	// attemptQueueCap 尝试通道容量：通道满则丢统计不丢请求（emitAttempt 原语义）。
	attemptQueueCap = 20000
	// recentRingSize 环形缓冲条数（recentLog 原为 200）。
	recentRingSize = 200
	// flushInterval 后台批量落盘的间隔（attemptWorker 的 ticker）。
	flushInterval = 250 * time.Millisecond
	// flushBatch 批量阈值：攒够这么多条不等 ticker 直接刷。
	flushBatch = 200
)

// Attempt 记录一次「请求 × 某后端某 key」的完整尝试（成功与失败都记）。
// 结构体照搬 seika_llm_router metrics.go 原文，字段名一字不改 —— json 序列化
// 的键名就是它们的下游契约（磁盘 JSONL 与 sidecar）。
//
// 为什么记失败而不仅仅是成功：sidecar/自调控要看失败率与失败类型分布，只记
// 成功会让被限流的上游在统计里永远"干净"（本机实测踩过）。
//
// 边界说明：旧 logAttempt 写 stderr 的那行人类可读日志**不属于存储**，留在
// proxy 层 —— store 的职责是落库与环形缓冲，不决定日志观感；把 stderr 输出
// 搬进来会把 store 和 log 包的格式耦合死，看板换行格式就得改数据库包。
type Attempt struct {
	Rid     string
	Backend string
	KeyIdx  int
	Model   string
	Stream  bool
	Seq     int // 本请求第几次尝试（1=首次）
	Status  int // 上游返回码；0=本地失败(网络/超时等)
	LatMs   int64
	FtMs    int64  // 首字延迟
	Class   string // 错误分类
	Err     string // 错误摘要（截断）
	OK      bool

	// token 用量（来自上游 usage；仅成功响应有值）
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int

	// 亲和键（X-Job-Id 优先，cleanJobID 消毒后的值；2026-10-18 加，与 SLR
	// attempts 的 job_id 列同义同源）。空=请求没带键。加字段是 JSONL 契约的
	// 追加而非破坏——按行取键的消费方读不到就是空串。
	JobID string
}

// Emit 记一次尝试：写环形缓冲 + 投递给后台落盘 worker。
// 对应旧 emitAttempt，语义逐位一致：通道满则丢（统计不该拖住请求热路径）。
func (s *Store) Emit(a Attempt) {
	select {
	case s.attemptCh <- a:
	default: // 通道满则丢统计不丢请求
	}
	s.recentMu.Lock()
	s.recent[s.recentPos] = a
	s.recentPos = (s.recentPos + 1) % len(s.recent)
	s.recentMu.Unlock()
}

// Recent 返回最近不超过 200 条尝试，最新在前。
// 对应旧 getRecentLog，锁与遍历语义不变；返回的是副本，调用方随便改。
func (s *Store) Recent() []Attempt {
	s.recentMu.Lock()
	defer s.recentMu.Unlock()
	out := make([]Attempt, 0, recentRingSize)
	// 从最新到最旧遍历；空槽（Rid 为空 = 尚未被写过）跳过
	for i := 0; i < len(s.recent); i++ {
		idx := (s.recentPos - 1 - i + len(s.recent)) % len(s.recent)
		if s.recent[idx].Rid == "" {
			continue
		}
		out = append(out, s.recent[idx])
		if len(out) >= recentRingSize {
			break
		}
	}
	return out
}

// Start 是后台落盘 worker（原 attemptWorker）。
//
// 阻塞直到 ctx 结束 —— 由调用方决定放几个 goroutine、归谁管生命周期；
// 旧版本是永不退出的裸 for，包化后用 ctx 谢幕，**退出前把残余 batch 刷掉**，
// 不丢最后一批统计。
//
// w 是 JSONL 出口（旧 logFH：每行一个 Attempt 的 JSON，含换行），nil 则不写。
// decision 字段不会出现在这里：本方法只做「搬运 attempts」，读什么日志、
// 排什么格式是调用方的事。
func (s *Store) Start(ctx context.Context, w io.Writer) {
	batch := []Attempt{}
	cbatch := []Capture{}
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	flush := func() {
		if len(batch) != 0 {
			// db 写失败静默吞掉：统计晚写入库/不入库不影响请求路径，
			// 与旧 metrics.writeAttempts 的行为一致。
			_ = s.writeAttempts(batch)
			if w != nil {
				for _, a := range batch {
					line, _ := json.Marshal(a)
					fmt.Fprintf(w, "%s\n", line)
				}
			}
			batch = batch[:0]
		}
	}
	flushCaps := func() {
		// 留存是旁路：写失败静默吞（丢留存不影响请求路径），不清空会重复写——
		// 所以无论成功失败都释放这批，避免写入失败时无限重试同一批占住内存。
		if len(cbatch) == 0 {
			return
		}
		_ = s.writeCaptures(cbatch)
		cbatch = cbatch[:0]
	}
	for {
		select {
		case a := <-s.attemptCh:
			batch = append(batch, a)
			if len(batch) >= flushBatch {
				flush()
			}
		case c := <-s.captureCh:
			cbatch = append(cbatch, c)
			if len(cbatch) >= captureFlushBatch {
				flushCaps()
			}
		case <-ticker.C:
			flush()
			flushCaps()
		case <-ctx.Done():
			// 结算已入通道但未刷盘的剩余条目，然后收工。
			// 不再 drain 通道：谢幕时刻允许通道里有未处理项
			// （Close 之前的正常退出路径已是「尽力而为」）。
			// 同一轮把 attempts 与 captures 都 drain，留存谢幕不丢。
			drain := true
			for drain {
				select {
				case a := <-s.attemptCh:
					batch = append(batch, a)
				case c := <-s.captureCh:
					cbatch = append(cbatch, c)
				default:
					drain = false
				}
			}
			flush()
			flushCaps()
			return
		}
	}
}

// writeAttempts 批量写 attempts（单条事务），由 worker 调用，避免请求路径
// 直接写 SQLite。照搬旧 metrics.writeAttempts。
func (s *Store) writeAttempts(batch []Attempt) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO attempts
		(ts,rid,backend,key_idx,model,stream,seq,status,lat_ms,ft_ms,class,err,ok,prompt_tokens,completion_tokens,total_tokens,cached_tokens,job_id)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	now := float64(time.Now().UnixNano()) / 1e9
	for _, a := range batch {
		okInt := 0
		if a.OK {
			okInt = 1
		}
		streamInt := 0
		if a.Stream {
			streamInt = 1
		}
		if _, err := stmt.Exec(now, a.Rid, a.Backend, a.KeyIdx, a.Model,
			streamInt, a.Seq, a.Status, a.LatMs, a.FtMs, a.Class, a.Err, okInt,
			a.PromptTokens, a.CompletionTokens, a.TotalTokens, a.CachedTokens, a.JobID); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
