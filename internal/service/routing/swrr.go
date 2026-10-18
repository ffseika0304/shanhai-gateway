package routing

import "sync/atomic"

// SWRRPick 平滑加权轮转（smooth weighted round-robin，nginx 同款）：
// 每轮给所有候选各累加自己的权重，取当前值最大者首发，再从它头上扣掉总权重。
//   - 全等权重且候选集稳定 → 严格轮转，与接线前 rrSeq%N 行为逐字一致（零回归）；
//   - weight 低的后端按比例少领首发回合——这是 weight 真正参与路由的唯一入口
//     （2026-09-28 前 weight 只存不报不读，是摆设；"压并发当降权"会被
//     「首发满载→分流兜底」规则反噬成烧代金券/按量，故明确不走那条路）；
//   - weight<=0 不参与首发（重试路径与 ROTATE_START=0 严格优先级原样不动）；
//   - 候选缺席期间其累加值冻结，回归时最多优先补发一次，不会饿死。
//
// ⚠️ usable 必须非空，否则 panic（见 requireUsable 的理由）。
// 这是本函数**唯一新增**的边界行为：搬入前空列表会在下面
// `% len(usable)` 上除零 panic，报的是 "integer divide by zero"。
// 非空输入的行为逐字未变。
func (rt *Router) SWRRPick(usable []string) string {
	// 在取锁之前炸：持锁 panic 虽然也会被 defer 解开，但没必要让别的
	// goroutine 的轮转陪着一起等。
	requireUsable(usable)

	rt.swrrMu.Lock()
	defer rt.swrrMu.Unlock()
	if rt.swrrCur == nil {
		rt.swrrCur = map[string]float64{}
	}
	total, pick := 0.0, ""
	pickCur := 0.0
	for _, n := range usable {
		b := rt.ByName(n)
		if b == nil || b.Weight <= 0 {
			continue
		}
		total += b.Weight
		rt.swrrCur[n] += b.Weight
		if pick == "" || rt.swrrCur[n] > pickCur {
			pick, pickCur = n, rt.swrrCur[n]
		}
	}
	if pick == "" || total <= 0 {
		// 全员零权重 → 退回严格轮转，防饿死
		k := int(atomic.AddUint64(&rt.rrSeq, 1)-1) % len(usable)
		return usable[k]
	}
	rt.swrrCur[pick] -= total
	return pick
}
