// dashboard.go —— 运维看板 API。自根目录 metrics.go 的 Metrics 结构搬入
// （根目录那份已删除），加上 /api/stats /api/tokens /api/recent /quota.json
// 这几个从 seika_llm_router main.go 端点列表搬过来的口。
//
// ⚠️ 数据源的裁决（接线批 2026-10-11）：
//   - 公版 metrics.go 依赖 counters / requests 两张表 —— 那两张表是 attempts
//     的功能子集，合并时刻就**刻意不建**了（见 core/store/store.go 注释）。
//     所以这里的统计全部改走 **store 的 attempts 表** + store.Recent() 环形
//     缓冲，绝不去捏造那两张表。
//   - "started" 时刻来自进程内存（Handlers.StartedAt）：没有 counters 表之后
//     跨重启的历史本轮先记进程时刻（基础版本），后面批再从 attempts 首条推。
//   - 本轮是**基础版本**：路由先全通、数据只接真正接得上的部分 ——
//     /api/stats 的每后端字段来自 core/upstream 的运行态（内存），tokens
//     来自 attempts 聚合，费用按后端 Pricing 现算。慢探测指标
//     （slow_probe_remain_s）、失败直方图（?hist）等留待看板打磨批次。
package api

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"time"

	"shanhaigateway/internal/core/errclass"
	"shanhaigateway/internal/core/store"
	"shanhaigateway/internal/core/upstream"
	"shanhaigateway/internal/service/affinity"
)

// Metrics 看板聚合器（原公版 metrics.go 的 Metrics，数据源换成 store）。
type Metrics struct {
	// StartedAt 进程启动时刻（uptime 起点）。
	StartedAt time.Time
}

// NewMetrics 组装看板聚合器。
func NewMetrics(startedAt time.Time) *Metrics {
	return &Metrics{StartedAt: startedAt}
}

// Snapshot 轻量聚合（基础版）：started / uptime / 总量 / 近60秒 / 今日，
// 全部来自 attempts（公版同名方法里的 requests/counters 部分不再存在）。
func (m *Metrics) Snapshot(st *store.Store) map[string]interface{} {
	total, ok, fail, recent60, today, _ := st.AttemptCounts()
	startedStr := m.StartedAt.Format("2006-01-02 15:04:05")
	return map[string]interface{}{
		"started":        startedStr,
		"uptime_sec":     time.Now().Unix() - m.StartedAt.Unix(),
		"recent_60s":     recent60,
		"today_requests": today,
		"total_requests": total,
		"total_ok":       ok,
		"total_fail":     fail,
	}
}

// handleStats GET /api/stats —— 细粒度实时统计（内存态，搬自 slr main.go）。
// shape 对齐前端 dashboard/index.html：{"backends":[...],"now":...,"rpm":n}。
func (h *Handlers) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	now := time.Now()
	rt := h.Router.Load()

	out := []map[string]any{}
	var rpm int64
	// 会话亲和观测（2026-10-11 接线）：全局计数 + 每后端 sticky_count。
	// 表不存在（SESSION_AFFINITY=off）时全部回零值，JSON 形状稳定。
	stickyStats := affinity.Stats{}
	stickyCount := map[string]int{}
	// pinned_jobs 与 SLR 口径一致 = 表内条目裸长度（不清 TTL，未到期前读不到
	// 的条目也算"钉着"）。必须先取 Len 再取 CountPerBackend——后者顺带清理
	// 过期条目，顺序反了 pinned_jobs 就比 SLR 同时刻的值小。
	affPinned := 0
	if h.Affinity != nil {
		affPinned = h.Affinity.Len()
		stickyStats = h.Affinity.Stats()
		stickyCount = h.Affinity.CountPerBackend()
	}
	for _, name := range rt.Candidates() {
		bk := rt.ByName(name)
		if bk == nil {
			continue
		}
		st := bk.Stats()
		breakdown := map[string]int64{}
		var totalFail int64
		for _, cls := range errclass.List {
			c := st.Count(cls)
			if c > 0 {
				// breakdown 全量保留（skip_* 是熔断/冷却的诊断信号，有观察价值），
				// 但失败数只累计真发过请求的类——否则 attempts ≠ success + failed。
				breakdown[string(cls)] = c
				if !cls.IsSkip() {
					totalFail += c
				}
			}
		}
		lastCls, lastSt, lastMsg, lastAt := st.LastError()
		acct := bk.BoundAccount()
		out = append(out, map[string]any{
			"name": bk.Name, "model": bk.EffectiveModel(now), "enabled": bk.Enabled,
			"weight": bk.Weight, "account": acct.Name, "keys": len(acct.APIKeys),
			"is_primary": bk.IsPrimary, "is_fallback": bk.IsFallback,
			"concurrency": bk.Concurrency, "in_flight": bk.InFlight(),
			"waiters": bk.Waiters(), "timeout": bk.Timeout,
			"attempts": st.Attempts(), "success": st.Success(),
			"failed":         totalFail,
			"fail_breakdown": breakdown,
			"circuit_open":   bk.CircuitOpen(),
			// 慢探测指标（slow_probe_remain_s）本轮不接：内部状态没有只读口，
			// 前端取不到就显示 "-"（基础版本，见文件头）。
			"cooldown_remain_s": math.Round(bk.CooldownRemain(now)*10) / 10,
			"last_error":        lastMsg, "last_error_class": lastCls,
			"last_error_status": lastSt,
			"last_error_at": func() any {
				if lastAt.IsZero() {
					return nil
				}
				return lastAt.Format(time.RFC3339)
			}(),
			// 该后端当前钉着多少个会话（亲和表 CountPerBackend；nil 表→0）
			"sticky_count": stickyCount[bk.Name],
		})
		rpm += st.Recent60s()
	}

	json.NewEncoder(w).Encode(map[string]any{
		"backends": out,
		"now":      now.Format(time.RFC3339),
		"rpm":      rpm,
		// 会话亲和全局计数（nil 表 → 0；加在顶层，看板与 sidecar 同源同形）
		"sticky_hits":      stickyStats.Hits,
		"sticky_misses":    stickyStats.Misses,
		"sticky_evictions": stickyStats.Evictions,
		// affinity 对象（2026-10-18 加）：与 SLR /api/stats 的 affinity 同形同名
		// （hits/misses/puts/pinned_jobs），迁移期消费方可以无差别读两边。
		// sticky_* 三键是本网关历史键名，看板还读着，保留；新消费方一律读这里。
		"affinity": map[string]any{
			"hits":        stickyStats.Hits,
			"misses":      stickyStats.Misses,
			"puts":        stickyStats.Puts,
			"pinned_jobs": affPinned,
		},
	})
}

// handleMetrics GET /api/metrics —— 持久化聚合视图（attempts 表）。
func (h *Handlers) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	snap := h.Metrics.Snapshot(h.Store)
	by, err := h.Store.AttemptsByBackend()
	if err == nil {
		snap["by_backend"] = by
	}
	json.NewEncoder(w).Encode(snap)
}

// handleTokens GET /api/tokens —— token 用量专项接口（看板卡片用）。
// 数据源 attempts；费用按各后端 Pricing 现算，没配单价的后端不回费用字段
// （公版 TokenUsage 的金额公式逐字对应）。
func (h *Handlers) handleTokens(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	all, today, month, hour, _ := h.Store.TokenTotals()
	rows, _ := h.Store.TokensByBackend()
	hourly, _ := h.Store.TokensHourlyToday()

	// pricing：后端名 → 单价。取当前配置快照（config 热改后即新价目）。
	pricing := map[string]*upstream.Pricing{}
	if h.Config != nil {
		for _, b := range h.Config.Snapshot().Backends {
			if b.Pricing != nil {
				pricing[b.Name] = b.Pricing
			}
		}
	}

	// 统一金额公式：非缓存in*Input + 缓存in*CachedRead + out*Output（每 1M tokens）
	costOf := func(name string, p, c, cached int64) (float64, bool) {
		pr := pricing[name]
		if pr == nil {
			return 0, false
		}
		nonCached := p - cached
		if nonCached < 0 {
			nonCached = 0
		}
		return float64(nonCached)*pr.Input/1e6 +
			float64(cached)*pr.CachedRead/1e6 +
			float64(c)*pr.Output/1e6, true
	}
	round4 := func(v float64) float64 { return math.Round(v*10000) / 10000 }

	byBackend := []map[string]any{}
	totalCost := 0.0
	for _, row := range rows {
		m := map[string]any{
			"backend": row.Backend, "prompt_tokens": row.PromptTokens,
			"completion_tokens": row.CompletionTokens, "total_tokens": row.TotalTokens,
			"cached_tokens": row.CachedTokens,
		}
		if cost, ok := costOf(row.Backend, row.PromptTokens, row.CompletionTokens, row.CachedTokens); ok {
			m["cost_usd"] = round4(cost)
			totalCost += cost
		}
		byBackend = append(byBackend, m)
	}

	hourlyOut := []map[string]any{}
	for _, hr := range hourly {
		hourlyOut = append(hourlyOut, map[string]any{
			"hour":              time.Unix(hr.Hour, 0).Format("15:00"),
			"prompt_tokens":     hr.PromptTokens,
			"completion_tokens": hr.CompletionTokens,
			"total_tokens":      hr.TotalTokens,
			"cached_tokens":     hr.CachedTokens,
		})
	}

	json.NewEncoder(w).Encode(map[string]any{
		"total_tokens": all, "today_tokens": today,
		"month_tokens": month, "hour_tokens": hour,
		"by_backend":     byBackend,
		"hourly_today":   hourlyOut,
		"month_cost_usd": round4(totalCost),
	})
}

// handleRecent GET /api/recent —— 最近请求（store.Recent() 环形缓冲，≤200 条，
// 最新在前）。JSON 字段名就是 store.Attempt 的导出字段（Rid/Backend/...），
// 与磁盘 JSONL 同源同形 —— 前端 dashboard 按 .Rid / .OK / .Status 读。
func (h *Handlers) handleRecent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h.Store.Recent())
}

// handleQuota GET /quota.json —— 配额指挥舱数据（外部采集脚本写的磁盘文件，
// Paths.QuotaFile）。公版原文（dashauth.go 注释）就交代过：这个文件带
// Last-Modified、是磁盘静态文件，直接用 http.ServeFile 供（缺则 404）。
func (h *Handlers) handleQuota(w http.ResponseWriter, r *http.Request) {
	if _, err := os.Stat(h.QuotaFile); err != nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"quota data not available"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, h.QuotaFile)
}
