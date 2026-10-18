package store

import (
	"database/sql"
	"time"
)

// ── 看板聚合查询（只读）─────────────────────────────────────────
//
// 看板（internal/api/dashboard.go）的数据源只有一张 attempts 表 —— 计数器
// 合并时刻就砍掉了（counters/requests 是 attempts 的功能子集，本包刻意不建，
// 见 store.go 的表注释）。这里的查询全部是**只读**聚合：send 进入/写路径
// 依然只有 Emit / Start，读侧在这里集中，谁要看什么数字不用再各写一份 SQL。

// AttemptCounts 全库聚合计数。today 的口径照搬公版 metrics.go：
// `time.Now().Truncate(24*time.Hour).Unix()`（按 UTC 截断，不是本地日界）。
func (s *Store) AttemptCounts() (total, ok, fail, recent60, today int64, err error) {
	now := time.Now().Unix()
	todayStart := time.Now().Truncate(24 * time.Hour).Unix()
	if err = s.db.QueryRow("SELECT COUNT(*) FROM attempts").Scan(&total); err != nil {
		return
	}
	if err = s.db.QueryRow("SELECT COUNT(*) FROM attempts WHERE ok=1").Scan(&ok); err != nil {
		return
	}
	if err = s.db.QueryRow("SELECT COUNT(*) FROM attempts WHERE ok=0").Scan(&fail); err != nil {
		return
	}
	if err = s.db.QueryRow("SELECT COUNT(*) FROM attempts WHERE ts > ?", now-60).Scan(&recent60); err != nil {
		return
	}
	err = s.db.QueryRow("SELECT COUNT(*) FROM attempts WHERE ts >= ?", todayStart).Scan(&today)
	return
}

// BackendAttempt 单个后端的持久化聚合（attempts GROUP BY backend）。
type BackendAttempt struct {
	Backend  string `json:"backend"`
	Attempts int64  `json:"attempts"`
	Ok       int64  `json:"ok"`
	Fail     int64  `json:"fail"`
}

// AttemptsByBackend 按后端聚合 attempts（全部历史，跨重启持久）。
func (s *Store) AttemptsByBackend() ([]BackendAttempt, error) {
	rows, err := s.db.Query(`SELECT backend, COUNT(*),
			COALESCE(SUM(ok),0), COALESCE(SUM(CASE WHEN ok=0 THEN 1 ELSE 0 END),0)
		FROM attempts GROUP BY backend ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BackendAttempt{}
	for rows.Next() {
		var r BackendAttempt
		if err := rows.Scan(&r.Backend, &r.Attempts, &r.Ok, &r.Fail); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TokenTotals token 用量合计（total_tokens 之和），四个窗口：
// 全部 / 今日 / 本月 / 本小时。口径与 seika_llm_router metrics.go 的
// TokenUsage 一致（本地时区日界与整点）。
func (s *Store) TokenTotals() (all, today, month, hour int64, err error) {
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Unix()
	hourStart := now.Truncate(time.Hour).Unix()

	sumSince := func(since int64) (int64, error) {
		query := "SELECT COALESCE(SUM(total_tokens),0) FROM attempts"
		var args []any
		if since > 0 {
			query += " WHERE ts >= ?"
			args = append(args, since)
		}
		var v int64
		if err := s.db.QueryRow(query, args...).Scan(&v); err != nil {
			return 0, err
		}
		return v, nil
	}
	if all, err = sumSince(0); err != nil {
		return
	}
	if today, err = sumSince(dayStart); err != nil {
		return
	}
	if month, err = sumSince(monthStart); err != nil {
		return
	}
	hour, err = sumSince(hourStart)
	return
}

// TokenRow 单个后端的 token 明细（本月窗口，供金额与缓存命中卡片汇总）。
type TokenRow struct {
	Backend          string `json:"backend"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	CachedTokens     int64  `json:"cached_tokens"`
}

// TokensByBackend 本月按后端的 token 明细（对齐 slr TokenUsage 的 by_backend）。
func (s *Store) TokensByBackend() ([]TokenRow, error) {
	monthStart := time.Date(time.Now().Year(), time.Now().Month(), 1, 0, 0, 0, 0, time.Now().Location()).Unix()
	rows, err := s.db.Query(`SELECT backend,
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(cached_tokens),0)
		FROM attempts WHERE ts >= ? GROUP BY backend ORDER BY 4 DESC`, monthStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenRow{}
	for rows.Next() {
		var r TokenRow
		if err := rows.Scan(&r.Backend, &r.PromptTokens, &r.CompletionTokens,
			&r.TotalTokens, &r.CachedTokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// HourTokens 今日某个整点的 token 明细。
type HourTokens struct {
	Hour             int64 `json:"hour"` // 整点 unix 秒
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
}

// TokensHourlyToday 今日分小时 token 明细（对齐 slr TokenUsage 的 hourly_today）。
func (s *Store) TokensHourlyToday() ([]HourTokens, error) {
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	rows, err := s.db.Query(`SELECT (CAST(ts / 3600 AS INTEGER)) * 3600 AS h,
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(total_tokens),0), COALESCE(SUM(cached_tokens),0)
		FROM attempts WHERE ts >= ? GROUP BY h ORDER BY h`, dayStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HourTokens{}
	for rows.Next() {
		var r HourTokens
		if err := rows.Scan(&r.Hour, &r.PromptTokens, &r.CompletionTokens,
			&r.TotalTokens, &r.CachedTokens); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Tables 返回库里实际存在的表名（来自 sqlite_master，按字母序）。
// 供冒烟测试与运维核对表集合 —— 合并后的库**必须没有**
// qr_tickets / counters / requests，这条断言就是用它成立的。
func (s *Store) Tables() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DB 暴露底层连接池。
//
// 唯一用途：接线层把同一个句柄交给 runtime.Housekeeper（WAL checkpoint /
// PRAGMA optimize 需要直接对连接发 PRAGMA）。除接线层与测试外，任何代码
// 都不得绕过 Store 的方法用它做读写 —— 否则「存储层唯一出口」就此失守。
func (s *Store) DB() *sql.DB {
	return s.db
}
