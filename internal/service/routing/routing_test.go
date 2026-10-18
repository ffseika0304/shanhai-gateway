package routing

import (
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shanhaigateway/internal/core/errclass"
	"shanhaigateway/internal/core/upstream"
)

// ── 测试脚手架 ──────────────────────────────────────────────
// 本包的逻辑全是零 IO 的决策，所以测试也不起服务、不打桩网络、不真实 sleep：
// 隔离窗口是 10 分钟，把 fallbackStrikeWin 拨小，用可控时间戳走完整个窗口。

// TestMain 把 log 扔掉：路由决策在生产里每请求都打日志，压测时不收走的话
// 几千行日志会把 -v 输出淹掉，掩盖真正的测试输出。
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// newRouter 造一批后端并建路由器。
type bspec struct {
	name   string
	weight float64
	prim   bool
	fb     bool
	enab   bool
}

func newRouter(t *testing.T, specs ...bspec) (*Router, []*upstream.Backend) {
	t.Helper()
	var bs []*upstream.Backend
	for _, s := range specs {
		b := &upstream.Backend{
			Name: s.name, Weight: s.weight, Enabled: s.enab,
			IsPrimary: s.prim, IsFallback: s.fb,
			Concurrency: 4, BaseURL: "http://upstream.invalid", Model: "m",
		}
		b.Init(nil)
		b.Weight = s.weight // Init 会把 0 补成 1.0，这里显式写回以免覆盖测试意图
		bs = append(bs, b)
	}
	return New(bs), bs
}

// ── SWRR：等权时与严格轮转逐字等价（这条不许退化）─────────────
//
// 这是本包最重要的一条不变量。接线前首发分配是 rrSeq%N，
// 2026-09-28 换成 SWRR 之后，等权场景下的序列必须与之一模一样 ——
// 否则就是一次全量回归：所有等权后端的流量分布、亲和性、日志顺序全变。
// 注释里写的「与旧 rrSeq%N 严格轮转逐字等价——零回归」，这里用测试钉死。

func TestSWRREqualWeightsMatchStrictRoundRobin(t *testing.T) {
	rt, _ := newRouter(t,
		bspec{name: "a", weight: 1, enab: true},
		bspec{name: "b", weight: 1, enab: true},
		bspec{name: "c", weight: 1, enab: true},
	)
	usable := []string{"a", "b", "c"}

	// 前 3 轮：逐字等于 a,b,c
	want := []string{"a", "b", "c", "a", "b", "c", "a", "b", "c"}
	for i, w := range want {
		if got := rt.SWRRPick(usable); got != w {
			t.Fatalf("第 %d 次首发 = %q, 期望 %q（等权必须等于严格轮转）", i+1, got, w)
		}
	}

	// 长跑：第 i 次必须是 usable[i%len]，跑满 30 轮不能错位
	for i := 0; i < 30; i++ {
		if got, exp := rt.SWRRPick(usable), usable[i%len(usable)]; got != exp {
			t.Fatalf("第 %d 轮 = %q, 期望 %q（严格轮转序列错位了）", i+1, got, exp)
		}
	}
}

// 反面：权重不等时就**不该**是严格轮转，否则这条测试是空转的。
func TestSWRRUnequalWeightsAreNotStrictRoundRobin(t *testing.T) {
	rt, _ := newRouter(t,
		bspec{name: "heavy", weight: 9, enab: true},
		bspec{name: "light", weight: 1, enab: true},
	)
	strict := 0
	for i := 0; i < 10; i++ {
		if rt.SWRRPick([]string{"heavy", "light"}) == []string{"heavy", "light"}[i%2] {
			strict++
		}
	}
	if strict == 10 {
		t.Fatal("权重 9:1 却仍按严格轮转走 —— 加权没生效")
	}
}

// 权重低的后端按比例少领回合（1000 次采样，比例误差在容差内）。
func TestSWRRLongTermRatioFollowsWeight(t *testing.T) {
	const rounds = 1000
	rt, _ := newRouter(t,
		bspec{name: "w5", weight: 5, enab: true},
		bspec{name: "w3", weight: 3, enab: true},
		bspec{name: "w2", weight: 2, enab: true},
	)
	usable := []string{"w5", "w3", "w2"}
	want := map[string]float64{"w5": 0.5, "w3": 0.3, "w2": 0.2}

	got := map[string]int{}
	for i := 0; i < rounds; i++ {
		got[rt.SWRRPick(usable)]++
	}
	for name, exp := range want {
		share := float64(got[name]) / rounds
		if diff := share - exp; diff > 0.01 || diff < -0.01 {
			t.Errorf("%s 实际份额 %.3f, 期望 %.3f（误差 %.3f > 0.01）",
				name, share, exp, diff)
		}
	}
}

// weight<=0 不参与首发。注：负权重在这套语义下没有意义（Init 只把 0 补成 1），
// 但仍然照 SWRRPick 原判定被跳过 —— 搬入前也是这样，写下来钉住。
func TestSWRRZeroAndNegativeWeightNeverPicked(t *testing.T) {
	for _, w := range []float64{0, -1, -100} {
		rt, _ := newRouter(t,
			bspec{name: "normal", weight: 1, enab: true},
			bspec{name: "muted", weight: w, enab: true},
		)
		usable := []string{"normal", "muted"}
		for i := 0; i < 40; i++ {
			if got := rt.SWRRPick(usable); got != "normal" {
				t.Fatalf("weight=%v 的后端参与了首发（第 %d 次给了 %q）", w, i+1, got)
			}
		}
	}
}

// 全员零权重 → 退回严格轮转 rrSeq，防饿死。
func TestSWRRAllZeroWeightsFallsBackToStrictRotation(t *testing.T) {
	rt, _ := newRouter(t,
		bspec{name: "a", weight: 0, enab: true},
		bspec{name: "b", weight: 0, enab: true},
	)
	usable := []string{"a", "b"}
	for i := 0; i < 6; i++ {
		if got, exp := rt.SWRRPick(usable), usable[i%2]; got != exp {
			t.Fatalf("全员零权重应退回严格轮转：第 %d 次 = %q, 期望 %q", i+1, got, exp)
		}
	}
}

// 候选里混进不在册的名字（ByName 返回 nil）时被跳过，不能被选中。
func TestSWRRSkipsUnknownNames(t *testing.T) {
	rt, _ := newRouter(t, bspec{name: "a", weight: 1, enab: true})
	for i := 0; i < 20; i++ {
		if got := rt.SWRRPick([]string{"a", "ghost"}); got != "a" {
			t.Fatalf("不在册的名字被选中了：%q", got)
		}
	}
}

// 候选缺席期冻结、回归不被饿死。
//
// 「缺席期冻结」是白盒断言：缺席期间 swrrCur 里那个名字的值一个字节都不能动。
// 如果让它继续累加，缺席 100 轮就攒下 100 轮的债，回归后要连着霸占几十轮
// 才能还完 —— 真正的现象是「回来一个大爆发」，比饿死更糟。
//
// 「回归不被饿死」是黑盒断言：回来之后最多补发一次，随后立刻回到正常比例。
func TestSWRRAbsentCandidateFreezesAndIsNotStarved(t *testing.T) {
	rt, _ := newRouter(t,
		bspec{name: "big", weight: 5, enab: true},
		bspec{name: "small", weight: 1, enab: true},
	)
	// 先正常转 8 轮，给 small 攒下它的累加值
	for i := 0; i < 8; i++ {
		rt.SWRRPick([]string{"big", "small"})
	}

	// small 缺席 50 轮
	rt.swrrMu.Lock()
	frozen := rt.swrrCur["small"]
	rt.swrrMu.Unlock()
	if frozen == 0 {
		t.Fatal("small 的累加值本该非零（5:1 下它领过回合），先确认前置条件")
	}
	for i := 0; i < 50; i++ {
		if got := rt.SWRRPick([]string{"big"}); got != "big" {
			t.Fatalf("缺席名单里的 big 之外的成员不该出现：%q", got)
		}
	}
	rt.swrrMu.Lock()
	still := rt.swrrCur["small"]
	rt.swrrMu.Unlock()
	if still != frozen {
		t.Fatalf("缺席期累加值被改了：%v → %v（缺席期间必须冻结，否则回归会爆发）", frozen, still)
	}

	// small 回归：最多补发一次（≤2 轮内必须被领到），随后回到 1:5 的比例
	var back []string
	for i := 0; i < 2; i++ {
		back = append(back, rt.SWRRPick([]string{"big", "small"}))
	}
	if back[0] == "small" && back[1] == "small" {
		t.Fatalf("回归后连着霸占两轮：%v —— 缺席期不该攒债", back)
	}
	firstSmall := -1
	for i := 0; i < 2; i++ {
		if back[i] == "small" {
			firstSmall = i
		}
	}
	if firstSmall == -1 {
		t.Fatalf("回归后两轮都没轮到 small：%v —— 被饿死了", back)
	}

	// 稳态比例必须回到 5:1（缺席 50 轮没有在账上留下任何影响）
	got := map[string]int{}
	for i := 0; i < 600; i++ {
		got[rt.SWRRPick([]string{"big", "small"})]++
	}
	if share := float64(got["small"]) / 600; share < 0.14 || share > 0.19 {
		t.Fatalf("回归后 small 稳态份额 = %.3f, 期望约 0.167（缺席的 50 轮不该被追补）", share)
	}
}

// ── 候选选择 ────────────────────────────────────────────────

// 主轮转（is_primary）与兜底（is_fallback）分开：既是 primary 又是 fallback
// 的后端，只进兜底名单，不进首发名单。
func TestCandidatesSplitPrimaryAndFallback(t *testing.T) {
	rt, _ := newRouter(t,
		bspec{name: "p1", weight: 1, prim: true, enab: true},
		bspec{name: "p2", weight: 1, prim: true, enab: true},
		bspec{name: "f1", weight: 1, fb: true, enab: true},
		bspec{name: "both", weight: 1, prim: true, fb: true, enab: true},
	)

	if got := rt.Candidates(); !eq(got, []string{"p1", "p2"}) {
		t.Errorf("首发候选 = %v, 期望 [p1 p2]（兜底与 both 都不该进首发）", got)
	}
	if got := rt.GetFallbackCandidates(); !eq(got, []string{"f1", "both"}) {
		t.Errorf("兜底候选 = %v, 期望 [f1 both]", got)
	}
}

// 一个 is_primary 都没配时，所有非兜底后端当主轮转（老配置兼容）。
func TestCandidatesFallBackToNonFallbackWhenNoPrimary(t *testing.T) {
	rt, _ := newRouter(t,
		bspec{name: "x", weight: 1, enab: true},
		bspec{name: "y", weight: 1, enab: true},
		bspec{name: "z", weight: 1, fb: true, enab: true},
	)
	if got := rt.Candidates(); !eq(got, []string{"x", "y"}) {
		t.Fatalf("兼容路径的候选 = %v, 期望 [x y]", got)
	}
}

// 候选名单去重、剔除不在册的名字。
func TestCandidatesDedupAndDropUnknown(t *testing.T) {
	rt, _ := newRouter(t, bspec{name: "p1", weight: 1, prim: true, enab: true})
	rt.primary = append(rt.primary, "ghost", "p1", "p1")
	if got := rt.Candidates(); !eq(got, []string{"p1"}) {
		t.Fatalf("候选 = %v, 期望 [p1]（ghost 不在册、p1 重复）", got)
	}
}

// 空路由的候选与兜底候选都是空 —— 但注意这是两个不同的信号：
//   - Candidates 空 = 配置上就没有首发后端；
//   - GetFallbackCandidates 空 = 此刻没有可用兜底，调用方该转去排队。
//
// 后者由本包过滤得出，前者不是（见 TestCandidatesDoesNotFilterAvailability）。
func TestCandidatesEmptyRouter(t *testing.T) {
	rt := New(nil)
	if got := rt.Candidates(); len(got) != 0 {
		t.Errorf("空路由的候选 = %v, 期望空", got)
	}
	if got := rt.GetFallbackCandidates(); got != nil {
		t.Errorf("空路由的兜底候选 = %v, 期望 nil", got)
	}
}

// ── 契约：Candidates 不查可用性，过滤是调用方的活 ──────────────────
//
// 这条分工在 Candidates 的注释里写着「不查 SkipReason」，但注释防不住 bug：
// 后人很容易顺手「补全」成也过滤一遍，看着更正确，实际会让两件事打架 ——
// ① 跳过原因（冷却/熔断/禁用）统计不进 stats 了，那是 upstream 的账；
// ② 候选集变小后 SWRR 的累加值语义变了，权重分布跟着变。
//
// 所以这里把契约用测试钉死：**不先过滤，SWRRPick 会把正在冷却的后端原样端出去。**
// 哪天有人给 Candidates 加上过滤，这条立刻红给他看。

func TestCandidatesDoesNotFilterAvailability(t *testing.T) {
	rt, bs := newRouter(t,
		bspec{name: "good", weight: 1, prim: true, enab: true},
		// cooling 权重更高，让「没过滤 → 首发真的落到它身上」成为确定结果，
		// 而不是靠等权时的先后顺序（平手时 SWRR 取先出现的那个）。
		bspec{name: "cooling", weight: 5, prim: true, enab: true},
	)
	bs[1].SetCooldown(time.Minute, errclass.RateLimit429)

	// 前置条件：cooling 此刻确实不可用
	if got := bs[1].SkipReason(time.Now()); got != errclass.SkipCooldown {
		t.Fatalf("前置条件：cooling 该在冷却中，得到 %q", got)
	}

	// 契约本身：Candidates 照给不误
	if got := rt.Candidates(); !eq(got, []string{"good", "cooling"}) {
		t.Fatalf("Candidates = %v, 期望 [good cooling] —— 它不查可用性，这是分工不是 bug", got)
	}

	// 而且它真的会一路把冷却中的后端端出去：绕开过滤直接挑首发，
	// 首发就落在 cooling 上。这正是「调用方必须过滤」的理由。
	if got := rt.SWRRPick(rt.Candidates()); got != "cooling" {
		t.Fatalf("不先过滤时首发 = %q, 期望 cooling —— 这条断言失败说明 Candidates 已经被改成会过滤了，"+
			"请先确认是不是有意改动那条分工，再改这条测试", got)
	}

	// 调用方按契约过滤后，首发就只剩健康后端了
	usable := []string{}
	for _, n := range rt.Candidates() {
		if b := rt.ByName(n); b == nil || b.SkipReason(time.Now()) != "" {
			continue
		}
		usable = append(usable, n)
	}
	if !eq(usable, []string{"good"}) {
		t.Fatalf("过滤后的可用首发 = %v, 期望 [good]", usable)
	}
	for i := 0; i < 5; i++ {
		if got := rt.SWRRPick(usable); got != "good" {
			t.Fatalf("过滤后首发 = %q, 期望恒为 good", got)
		}
	}
}

// ── 契约：空候选必须响亮地炸，而不是除零 ────────────────────────
//
// 生产里「全站冷却」就意味着候选列表为空。搬入前这会在 SWRR 的 `% len(usable)`
// 上除零 panic，栈顶是一个跟原因毫无关系的算术运算。现在在入口就炸，
// 且 panic 消息里写清「先查 skipReason」。
//
// 为什么是 panic 而不是 error，见 requireUsable 的注释。

func TestSWRRPickPanicsOnEmptyCandidates(t *testing.T) {
	rt, _ := newRouter(t, bspec{name: "a", weight: 1, enab: true})

	for _, tc := range []struct {
		name  string
		input []string
	}{
		{"nil", nil},
		{"空切片", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("空候选应 panic，却正常返回了")
				}
				msg, ok := r.(string)
				if !ok {
					t.Fatalf("panic 值不是 string：%#v", r)
				}
				// 消息必须自带排查方向，否则和 "integer divide by zero" 没区别
				for _, want := range []string{"候选列表为空", "SkipReason", "冷却"} {
					if !strings.Contains(msg, want) {
						t.Errorf("panic 消息缺少 %q：%s", want, msg)
					}
				}
			}()
			rt.SWRRPick(tc.input)
		})
	}
}

// 全站冷却的现场：所有后端都进冷却 → 过滤后列表为空 → 交给 SWRRPick 就炸。
func TestSWRRPickPanicsWhenWholeSiteIsCooling(t *testing.T) {
	rt, bs := newRouter(t,
		bspec{name: "a", weight: 1, prim: true, enab: true},
		bspec{name: "b", weight: 1, prim: true, enab: true},
	)
	for _, b := range bs {
		b.SetCooldown(60*time.Second, errclass.RateLimit429)
	}

	usable := []string{}
	for _, n := range rt.Candidates() {
		if b := rt.ByName(n); b.SkipReason(time.Now()) != "" {
			continue
		}
		usable = append(usable, n)
	}
	if len(usable) != 0 {
		t.Fatalf("前置条件：全员冷却后可用列表应为空，得到 %v", usable)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("全站冷却时交出空列表应 panic —— 让它落进 SWRR 就是除零")
		}
	}()
	rt.SWRRPick(usable)
}

// panic 不能污染下一次调用：炸过一次之后路由照常工作。
func TestSWRRPickRecoversAfterPanic(t *testing.T) {
	rt, _ := newRouter(t,
		bspec{name: "a", weight: 1, prim: true, enab: true},
		bspec{name: "b", weight: 1, prim: true, enab: true},
	)
	func() {
		defer func() { _ = recover() }()
		rt.SWRRPick(nil)
	}()

	usable := []string{"a", "b"}
	for i := 0; i < 6; i++ {
		if got, want := rt.SWRRPick(usable), usable[i%2]; got != want {
			t.Fatalf("panic 之后第 %d 轮 = %q, 期望 %q（路由被 panic 搞坏了）", i+1, got, want)
		}
	}
}

// GetFallbackCandidates 返回 nil 是**正常信号**（该去排队了），不该 panic。
// 它与 SWRRPick 的空列表语义不同，别把两处混起来。
func TestGetFallbackCandidatesEmptyIsNotPanic(t *testing.T) {
	// 只配了首发、没配兜底
	rt, _ := newRouter(t, bspec{name: "p1", weight: 1, prim: true, enab: true})
	if got := rt.GetFallbackCandidates(); got != nil {
		t.Fatalf("无兜底时 = %v, 期望 nil", got)
	}

	// 配了兜底但全在冷却：同样返回 nil，让调用方去排队
	rt, bs := newRouter(t,
		bspec{name: "p1", weight: 1, prim: true, enab: true},
		bspec{name: "f1", weight: 1, fb: true, enab: true},
	)
	bs[1].SetCooldown(time.Minute, errclass.RateLimit429)
	if got := rt.GetFallbackCandidates(); got != nil {
		t.Fatalf("兜底全冷却时 = %v, 期望 nil（这是排队信号，不是崩溃信号）", got)
	}
}

// 处于冷却/熔断/禁用的后端被排除出兜底候选。
//
// ⚠️ 分工要说清楚（搬入前就是这样，本包原样保留）：
//   - **兜底候选由本包自己过滤**（GetFallbackCandidates 查 SkipReason + 隔离）。
//     因为兜底是「主线满了才去挤」，必须当场就排除掉不可用的，否则调用方
//     逐个 tryAcquire 时会在一个明知冷却的后端上空转一轮。
//   - **首发候选不查 skipReason**：Candidates 只回答「配置上谁是首发」，
//     「此刻能不能接」由调用方拿 b.SkipReason(now) 判（它还要把跳过原因
//     记进 stats，那是 upstream 的事，不该在路由层做）。
//
// 下面两条分别钉住这两半。
func TestFallbackCandidatesExcludeUnusableBackends(t *testing.T) {
	rt, bs := newRouter(t,
		bspec{name: "ok", weight: 1, fb: true, enab: true},
		bspec{name: "cooling", weight: 1, fb: true, enab: true},
		bspec{name: "disabled", weight: 1, fb: true, enab: false},
	)
	byName := map[string]*upstream.Backend{}
	for _, b := range bs {
		byName[b.Name] = b
	}
	byName["cooling"].SetCooldown(time.Minute, errclass.RateLimit429)

	now := time.Now()
	if got := byName["ok"].SkipReason(now); got != "" {
		t.Fatalf("前置条件：ok 不该被排除，却得到 %q", got)
	}
	if got := byName["cooling"].SkipReason(now); got != errclass.SkipCooldown {
		t.Fatalf("前置条件：cooling 该冷却，却得到 %q", got)
	}
	if got := byName["disabled"].SkipReason(now); got != errclass.SkipDisabled {
		t.Fatalf("前置条件：disabled 该被禁用，却得到 %q", got)
	}

	if got := rt.GetFallbackCandidates(); !eq(got, []string{"ok"}) {
		t.Fatalf("兜底候选 = %v, 期望只剩 [ok]", got)
	}
}

// 首发候选不做可用性过滤，但调用方拿着名字 + SkipReason 就能把用不了的摘掉
// （这正是请求路径上的分工，见上条注释）。
func TestPrimaryCandidatesFilteredBySkipReasonAtCallSite(t *testing.T) {
	rt, bs := newRouter(t,
		bspec{name: "good", weight: 1, prim: true, enab: true},
		bspec{name: "cooling", weight: 1, prim: true, enab: true},
	)
	bs[1].SetCooldown(time.Minute, errclass.RateLimit429)

	// Candidates 给的是配置意义上的全部首发
	if got := rt.Candidates(); !eq(got, []string{"good", "cooling"}) {
		t.Fatalf("首发候选 = %v, 期望两个都在", got)
	}
	// 调用方按 skipReason 过滤后的效果
	now := time.Now()
	usable := []string{}
	for _, n := range rt.Candidates() {
		b := rt.ByName(n)
		if b == nil || b.SkipReason(now) != "" {
			continue
		}
		usable = append(usable, n)
	}
	if !eq(usable, []string{"good"}) {
		t.Fatalf("过滤后的可用首发 = %v, 期望 [good]", usable)
	}
}

// 兜底候选每次调用都换轮转起点，兜底之间负载依旧公平。
func TestFallbackCandidatesRotateStart(t *testing.T) {
	rt, _ := newRouter(t,
		bspec{name: "f1", weight: 1, fb: true, enab: true},
		bspec{name: "f2", weight: 1, fb: true, enab: true},
		bspec{name: "f3", weight: 1, fb: true, enab: true},
	)
	want := [][]string{
		{"f1", "f2", "f3"},
		{"f2", "f3", "f1"},
		{"f3", "f1", "f2"},
		{"f1", "f2", "f3"},
	}
	for i, w := range want {
		if got := rt.GetFallbackCandidates(); !eq(got, w) {
			t.Fatalf("第 %d 次兜底候选 = %v, 期望 %v", i+1, got, w)
		}
	}
}

// ── 兜底隔离 ────────────────────────────────────────────────

// 连续 fallbackMaxFails 次「上游真错」后进入隔离，且只隔离肇事的那一个。
func TestFallbackQuarantinesAfterMaxFails(t *testing.T) {
	rt, _ := newRouter(t,
		bspec{name: "f1", weight: 1, fb: true, enab: true},
		bspec{name: "f2", weight: 1, fb: true, enab: true},
	)
	now := time.Now()

	for i := 1; i < int(fallbackMaxFails); i++ {
		rt.RecordFallbackFailure("f1", errclass.ServerError5xx)
		if rt.FallbackQuarantined("f1", now) {
			t.Fatalf("第 %d 次就隔离了，门槛是 %d 次", i, fallbackMaxFails)
		}
	}
	rt.RecordFallbackFailure("f1", errclass.ServerError5xx)

	if !rt.FallbackQuarantined("f1", now) {
		t.Fatalf("连续 %d 次真错后 f1 仍未被隔离", fallbackMaxFails)
	}
	if rt.FallbackQuarantined("f2", now) {
		t.Fatal("f2 没出错却被隔离 —— 隔离必须是按后端记的")
	}
	if got := rt.GetFallbackCandidates(); !eq(got, []string{"f2"}) {
		t.Fatalf("隔离后兜底候选 = %v, 期望只剩 [f2]", got)
	}
}

// 隔离窗口 fallbackStrikeWin 过后自动放回（不是终身拉黑，2026-09-24 废除）。
func TestFallbackQuarantineExpiresAfterWindow(t *testing.T) {
	rt, _ := newRouter(t, bspec{name: "f1", weight: 1, fb: true, enab: true})

	for i := 0; i < int(fallbackMaxFails); i++ {
		rt.RecordFallbackFailure("f1", errclass.ServerError5xx)
	}
	base := time.Now()
	if !rt.FallbackQuarantined("f1", base) {
		t.Fatal("前置条件：f1 应处于隔离中")
	}

	// 窗口内不放回
	if !rt.FallbackQuarantined("f1", base.Add(fallbackStrikeWin-time.Second)) {
		t.Fatal("窗口还没到就放回了")
	}
	// 过窗自动放回 + 顺手清账
	if rt.FallbackQuarantined("f1", base.Add(fallbackStrikeWin+time.Second)) {
		t.Fatal("过窗后仍被隔离 —— 那就成终身拉黑了，只能重启恢复")
	}
	f := rt.fallbackFaults["f1"]
	if f.strike != 0 || f.last != 0 {
		t.Fatalf("过窗放回时应顺手清账，得到 strike=%d last=%d", f.strike, f.last)
	}
	// 清账后重新回到轮转
	if got := rt.GetFallbackCandidates(); !eq(got, []string{"f1"}) {
		t.Fatalf("过窗后兜底候选 = %v, 期望 [f1]", got)
	}
}

// 过窗前的一次新故障不该和上一次的旧账滚雪球（临时抖动不累积）。
func TestFallbackFailureAfterWindowResetsStrike(t *testing.T) {
	rt, _ := newRouter(t, bspec{name: "f1", weight: 1, fb: true, enab: true})

	rt.RecordFallbackFailure("f1", errclass.ServerError5xx)
	rt.RecordFallbackFailure("f1", errclass.ServerError5xx)
	if got := rt.fallbackFaults["f1"].strike; got != 2 {
		t.Fatalf("strike = %d, 期望 2", got)
	}
	// 把最后一次故障时间拨到两个窗口之前（RecordFallbackFailure 内部用 time.Now，
	// 这里只能直接改内部状态来模拟「上次故障很久以前」）
	rt.fallbackFaults["f1"].last = time.Now().Add(-2 * fallbackStrikeWin).Unix()

	rt.RecordFallbackFailure("f1", errclass.ServerError5xx)
	if got := rt.fallbackFaults["f1"].strike; got != 1 {
		t.Fatalf("过窗后的新故障 strike = %d, 期望 1（旧账应清掉，不能滚雪球）", got)
	}
	if rt.FallbackQuarantined("f1", time.Now()) {
		t.Fatal("一次过窗后的单发故障就隔离了 —— 旧账没清干净")
	}
}

// ⚠️ 软失败不计入隔离 —— 10-01 那场 403 自锁风暴的教训。
//
// 空气泡（max_tokens 太小把预算烧光）、客户端断开、400、skip_* 全都不该记账：
// 我方/下游的错不能让上游背锅。否则一次小预算请求就能把健康兜底踢下线，
// 而兜底是主线全满时唯一的去处 —— 兜底被误杀 = 全网无去处。
func TestFallbackSoftFailuresDoNotCountTowardQuarantine(t *testing.T) {
	soft := []errclass.Class{
		errclass.EmptyOutput,   // 空气泡
		errclass.NoContent,     // 空气泡
		errclass.Network,       // 下游取消 / 本地抖动
		errclass.BadRequest400, // 客户端的锅
		errclass.SkipCooldown,
		errclass.SkipCapacity,
		errclass.SkipCircuit,
		errclass.SkipDisabled,
	}
	for _, c := range soft {
		rt, _ := newRouter(t, bspec{name: "f1", weight: 1, fb: true, enab: true})
		// 记到远超门槛的次数
		for i := 0; i < int(fallbackMaxFails)*5; i++ {
			rt.RecordFallbackFailure("f1", c)
		}
		if rt.fallbackFaults["f1"] != nil {
			t.Fatalf("%q 被记账了（strike=%d）—— 软失败不该计入隔离",
				c, rt.fallbackFaults["f1"].strike)
		}
		if rt.FallbackQuarantined("f1", time.Now()) {
			t.Fatalf("%q 把 f1 隔离了 —— 软失败不该触发隔离", c)
		}
	}
}

// 真错仍然必须记账（与上面那条成对，防止「都不记」这种更蠢的实现蒙混过关）。
func TestFallbackRealFaultsStillCount(t *testing.T) {
	real := []errclass.Class{
		errclass.ServerError5xx, errclass.RateLimit429, errclass.Auth401,
		errclass.Forbidden403, errclass.Timeout, errclass.FirstTokenTimeout,
		errclass.ModelUnavailable, errclass.MidStreamFail, errclass.ErrorBody,
	}
	for _, c := range real {
		if !countsAsFault(c) {
			t.Errorf("%q 是上游真错，却不算故障 —— 那兜底隔离永远不会触发", c)
		}
	}
}

// 成功一次即清零，与健康度的 cooldownStrike 语义对齐（"密码输对了"）。
func TestFallbackSuccessClearsStrikes(t *testing.T) {
	rt, _ := newRouter(t, bspec{name: "f1", weight: 1, fb: true, enab: true})
	now := time.Now()

	for i := 0; i < int(fallbackMaxFails)-1; i++ {
		rt.RecordFallbackFailure("f1", errclass.ServerError5xx)
	}
	rt.RecordFallbackSuccess("f1")
	if got := rt.fallbackFaults["f1"].strike; got != 0 {
		t.Fatalf("成功后 strike = %d, 期望 0", got)
	}

	// 清零后重新攒不满门槛 → 不该隔离
	rt.RecordFallbackFailure("f1", errclass.ServerError5xx)
	if rt.FallbackQuarantined("f1", now) {
		t.Fatal("清零后一次故障就隔离了 —— 清零没生效")
	}

	// 没在册的后端记成功也不能 panic（nil 检查）
	rt.RecordFallbackSuccess("never-seen")
}

// ── 并发安全 ────────────────────────────────────────────────

// 这张表（fallbackFaults）被并发请求 goroutine 同时读写，裸 map 会触发 Go 进程级
// "fatal error: concurrent map writes" 直接崩掉网关。所以它必须有锁 ——
// 用 -race 跑这个用例，锁没了这里就会炸。
func TestConcurrentFallbackRecording(t *testing.T) {
	rt, _ := newRouter(t,
		bspec{name: "f1", weight: 1, fb: true, enab: true},
		bspec{name: "f2", weight: 1, fb: true, enab: true},
	)
	const goroutines, rounds = 16, 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			name := []string{"f1", "f2"}[g%2]
			for i := 0; i < rounds; i++ {
				rt.RecordFallbackFailure(name, errclass.ServerError5xx)
				_ = rt.FallbackQuarantined(name, time.Now())
				_ = rt.GetFallbackCandidates()
				if i%3 == 0 {
					rt.RecordFallbackSuccess(name)
				}
			}
		}(g)
	}
	wg.Wait()
}

// SWRR 累加值 + 两个轮转游标同样要抗并发：swrrMu 保护 swrrCur，
// rrSeq/fallbackSeq 走 sync/atomic。
func TestConcurrentSWRRPick(t *testing.T) {
	rt, _ := newRouter(t,
		bspec{name: "a", weight: 3, enab: true},
		bspec{name: "b", weight: 2, enab: true},
		bspec{name: "c", weight: 1, enab: true},
		bspec{name: "zero", weight: 0, enab: true},
	)
	usable := []string{"a", "b", "c", "zero"}

	const goroutines, rounds = 16, 500
	var wg sync.WaitGroup
	counts := make([]map[string]*int64, goroutines)
	for g := 0; g < goroutines; g++ {
		counts[g] = map[string]*int64{}
		for _, n := range usable {
			counts[g][n] = new(int64)
		}
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				pick := rt.SWRRPick(usable)
				atomic.AddInt64(counts[g][pick], 1)
			}
		}(g)
	}
	wg.Wait()

	total := map[string]int64{}
	for _, m := range counts {
		for n, c := range m {
			total[n] += atomic.LoadInt64(c)
		}
	}
	if total["zero"] != 0 {
		t.Fatalf("零权重后端在 %d 次并发首发里被选中 %d 次", goroutines*rounds, total["zero"])
	}
	n := float64(goroutines * rounds)
	// 3:2:1 → 50% / 33% / 17%，容差 3%
	for name, exp := range map[string]float64{"a": 0.5, "b": 1.0 / 3, "c": 1.0 / 6} {
		if share := float64(total[name]) / n; share < exp-0.03 || share > exp+0.03 {
			t.Errorf("%s 并发份额 %.3f, 期望 %.3f", name, share, exp)
		}
	}
}

// ── 小工具 ──────────────────────────────────────────────────

func eq(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
