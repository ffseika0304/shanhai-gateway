package store

import (
	"time"
)

// ── 常量：留存队列参数（与 attempts 同源纪律，改前先读）─────────────
const (
	// captureQueueCap 留存通道容量：通道满则丢（随手记账，不拖请求热路径）。
	// 与 attemptQueueCap 同思想，但留存是旁路，绝不阻塞 Emit 的调用者。
	captureQueueCap = 5000
	// captureFlushBatch 留存批量阈值：正文可能到 MB 级（tee 上限），批次得取
	// 得比 attempts(200) 小得多，避免单事务在内存里攒过多大 body。
	captureFlushBatch = 25

	// usage_source 枚举（NOT NULL DEFAULT 'unavailable'，见 store.go schemas）。
	// 值只有两个：upstream（从上游响应解析到真实用量）与 unavailable（上游没给/
	// 解析失败）。**刻意不做 estimated** —— Token 用量留 0 不估，见 Capture 注释。
	// 导出给 proxy 层在挂钩点直接引用，维持单一定义源。
	UsageSourceUpstream    = "upstream"
	UsageSourceUnavailable = "unavailable"
)

// Capture 记一次请求的「输入输出留存快照」，与 attempts 彻底分离：
//
// attempts = 轻量统计（rid/backend/lat/tokens，dashboard/sidecar 的事实源）；
// captures = 重载原文（脱敏后的 req_body/resp_body），供 OpenViking/RAG 消费。
// 二者用 **Rid 关联**，同一声互相独立，互不污染（与 config.Store.Snapshot
// 的快照纪律同一条）。
//
// 【模型口径】Model 必须记 attempts 同口径的 **effModel**（handler 算出的
// 实际使用模型，会被 SWRR 覆盖），**不能**记客户端原始 model —— capture 与
// attempts 要靠 rid join，如果两边 model 一边是原始值一边是 effModel，join
// 就会错行。ClsSkipCap（skip_capacity）这类行记的是客户端原始 model，capture
// 不应照抄那个分裂。
//
// 【语义字段的默认值】PartialRatio/Usable/Truncated 在 DDL 里写死了默认值
// （partial_ratio NOT NULL DEFAULT 1.0、usable NOT NULL DEFAULT 0、
// truncated NOT NULL DEFAULT 0）—— 若写方漏填或下游漏读，默认值兜住「这一行
// 是完整响应」的语义，消费方永远只面对两态而非三态 NULL。
type Capture struct {
	Rid     string
	Backend string
	Account string
	Model   string // effModel（与 attempts 同口径），见上方注释
	Stream  bool
	Status  int // 上游返回码；0=本地失败(网络/超时等)
	LatMs   int64
	FtMs    int64 // 首字延迟

	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int

	ReqBody  string // 脱敏后请求原文
	RespBody string // 脱敏后响应原文（流式=[DONE]前累积；断连=partial 前缀）

	TaskType string // SCF 任务类型（能读到则有；否则留空）
	JobID    string // SCF 任务 id（能读到则有；否则留空）

	// 语义字段（默认值写死在 DDL，见 store.go schemas）：
	PartialRatio float64 // 已累积程度：完整=1.0；断连部分=实际比例
	Usable       bool    // 完整响应 true；partial 一律 false，判据留给消费层
	Truncated    bool    // tee 累积超上限被截断

	// UsageSource 说明 captures 这行的 token 用量从哪来，供 OpenViking 聚合
	// 时 `WHERE usage_source='upstream'` 排除"0 是假数"的行。**不做 estimated**
	// 这个值——既然流式不上游给 usage 就留 0 不估，"estimated"就是一条没落的
	// 承诺。将来真要估，加它是一个新决策，不是悄悄改语义。
	// 值：upstream / unavailable
	UsageSource string

	Ts float64 // 捕获时刻（秒级 unix，写库时不重写）
}

// EmitCapture 送一条留存进后台落盘队列。非阻塞：通道满则丢并计数。
// 留存是旁路，丢了也不该拖住请求热路径 —— 与 Attempt.Emit 同一句纪律。
func (s *Store) EmitCapture(c Capture) {
	if c.Ts == 0 {
		c.Ts = float64(time.Now().UnixNano()) / 1e9
	}
	// partial_ratio 漏填时兜底为「完整响应 1.0」（与 DDL 默认值同语义）。
	// 真正 partial 的行由写方给非零比例；零字节 partial 本就是噪声，交由
	// usable=false 判不可消费。usable 不在这里兜底 —— 它 DDL 默认 0，
	// 必须由写方显式标完整才算可用（防"静默标可用"）。
	if c.PartialRatio == 0 {
		c.PartialRatio = 1.0
	}
	// usage_source 漏填兜底为 'unavailable'（DDL 同默认值）：宁缺毋假——
	// 消费方永远不用靠猜区分"0 是从 upstream 来的还是没给"。
	if c.UsageSource == "" {
		c.UsageSource = UsageSourceUnavailable
	}
	select {
	case s.captureCh <- c:
	default:
		// 通道满：丢留存不留空等。计数暴露给 A8 丢弃日志，避免"静默少了一类"。
		s.captureDiscard.Add(1)
	}
}

// CaptureDiscarded 返回因通道满被丢弃的留存条数（A8 丢弃监控读它）。
func (s *Store) CaptureDiscarded() int64 {
	if s == nil {
		return 0
	}
	return s.captureDiscard.Load()
}

// writeCaptures 批量写 captures（单事务），由 worker 调用，请求路径不直接写库。
// 镜像 attempts.writeAttempts：写失败静默吞，晚写/不写不影响请求路径。
func (s *Store) writeCaptures(batch []Capture) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO captures
		(ts,rid,backend,account,model,stream,status,lat_ms,ft_ms,
		 prompt_tokens,completion_tokens,total_tokens,cached_tokens,
		 req_body,resp_body,task_type,job_id,partial_ratio,usable,truncated,usage_source)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, c := range batch {
		streamInt := 0
		if c.Stream {
			streamInt = 1
		}
		usableInt := 0
		if c.Usable {
			usableInt = 1
		}
		truncInt := 0
		if c.Truncated {
			truncInt = 1
		}
		src := c.UsageSource
		if src == "" {
			src = UsageSourceUnavailable
		}
		if _, err := stmt.Exec(c.Ts, c.Rid, c.Backend, c.Account, c.Model,
			streamInt, c.Status, c.LatMs, c.FtMs,
			c.PromptTokens, c.CompletionTokens, c.TotalTokens, c.CachedTokens,
			c.ReqBody, c.RespBody, c.TaskType, c.JobID, c.PartialRatio, usableInt, truncInt, src); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
