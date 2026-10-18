package routing

import "fmt"

// Candidates 返回本次请求的候选顺序：主轮转（配置驱动、去重、只留已在册的后端）。
//
// ── 分工：这里只回答「配置上谁是首发」，不回答「此刻能不能接」 ─────────
//
// 冷却 / 熔断 / 禁用的剔除**不在本函数里**，由调用方逐个查
// Backend.SkipReason(now) 决定。搬入前就是这样分的，保留：
//   - 候选是「配置意义上的可用」，可用性是「此刻的可用」，两件事的时间尺度不同；
//   - 跳过原因要记进 stats，那是 upstream 的账，不该在路由层做；
//   - 首发路由还需要按权重挑人（见 SWRRPick），过滤和加权合成一步会让
//     「谁被摘掉」和「谁拿到首发」互相干扰，难排查。
//
// 这条分工是**契约**，用 TestCandidatesDoesNotFilterAvailability 钉死：
// 不先过滤就把列表交给 SWRRPick，它会把一个正在冷却的后端原样端出去。
// 要改这里，先去看那条测试 —— 它会在你动手的那一刻红给你看。
func (rt *Router) Candidates() []string {
	out := make([]string, 0, len(rt.primary)+1)
	seen := map[string]bool{}

	// 主轮转后端（配置驱动）
	for _, n := range rt.primary {
		if rt.ByName(n) != nil && !seen[n] {
			out, seen[n] = append(out, n), true
		}
	}

	return out
}

// PanicOnNoCandidates 交出空候选时用的报错文案。
//
// 导出来是为了让调用方（未来的请求路径接线）能写同一份检查，两边不各写一遍
// 文案 —— 一旦两边措辞不同，排查时看到哪句就说明是哪条路径出的问题。
const PanicOnNoCandidates = "routing: 候选列表为空 —— 所有后端此刻都在冷却/熔断/禁用，" +
	"或配置里一个 is_primary 都没写。" +
	"候选为空不是本层的判定结果（Candidates 不查可用性），" +
	"调用方必须先用 Backend.SkipReason(now) 逐个过滤；" +
	"过滤完为空说明整站当前没有可接活的后端，这是要向上游报 503 的运行期状况，" +
	"不是 panic 该处理的事 —— 在这里炸掉只会把一次可观测的过载变成进程崩溃"

// requireUsable 在 usable 为空时炸出上面那段说明。
//
// 选 panic 而不是 error，理由三条：
//  1. 这是**编程错误**，不是运行期故障。正确的请求路径在把列表交给 SWRRPick
//     之前就该判过空并回 503；能走到这里，说明调用方漏了那一步。
//  2. 真到了这里，SWRR 原本会在 `% len(usable)` 上除零 panic，报出来的是
//     "integer divide by zero" —— 一个把排查引向错误方向的栈帧。在入口
//     炸、并说清「先查 skipReason」，崩的那一刻日志里就有答案。
//  3. 返回 error 会逼每个调用点写 if，而每个调用点都可能写错（忘了处理、
//     处理了却继续往下走）。panic 让漏掉的路径立刻暴露，且不可能被忽略。
//
// ⚠️ 依赖这一条的前提是「调用方一定在 SWRRPick 之前判过空」。接线时若发现
// 某条路径把空列表原样传进来，panic 正是你想要的信号 —— 它比静默 503 更早
// 暴露「我以为我判过了」。
func requireUsable(usable []string) {
	if len(usable) == 0 {
		panic(fmt.Sprintf("%s（传入 len=%d）", PanicOnNoCandidates, len(usable)))
	}
}
