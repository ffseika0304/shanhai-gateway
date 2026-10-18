package upstream

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shanhaigateway/internal/core/errclass"
)

// ── 测试脚手架 ──────────────────────────────────────────────
// 健康状态机依赖「当前时间」。这里统一用 setClock 把 clock 钉到假时间，
// 全程不做真实 sleep —— 退避阶梯最远到 4 小时，真等一次这个测试就废了。

var baseTime = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func setClock(t *testing.T, at time.Time) *time.Time {
	t.Helper()
	cur := at
	clock = func() time.Time { return cur }
	t.Cleanup(func() { clock = time.Now })
	return &cur
}

func newTestBackend(t *testing.T, conc int) *Backend {
	t.Helper()
	b := &Backend{
		Name: "test", BaseURL: "http://upstream.invalid", Model: "m",
		Concurrency: conc, Enabled: true,
	}
	b.Init(nil)
	if b.Concurrency != conc {
		t.Fatalf("并发位 = %d, 期望 %d", b.Concurrency, conc)
	}
	return b
}

// ── 容量：并发位 ────────────────────────────────────────────

func TestTryAcquireRespectsLimit(t *testing.T) {
	b := newTestBackend(t, 2)
	for i := 0; i < 2; i++ {
		if !b.tryAcquire() {
			t.Fatalf("第 %d 次抢占应成功", i)
		}
	}
	if b.tryAcquire() {
		t.Fatal("占满后 tryAcquire 应返回 false")
	}
	if got := b.InFlight(); got != 2 {
		t.Fatalf("在飞数 = %d, 期望 2", got)
	}
	// 归位一个后又能抢到
	b.release()
	if !b.tryAcquire() {
		t.Fatal("release 后应能再抢到")
	}
	if got := b.InFlight(); got != 2 {
		t.Fatalf("在飞数 = %d, 期望 2", got)
	}
}

// 并发下抢位必须恰好抢满不超发。
func TestTryAcquireNoOversubscribe(t *testing.T) {
	const conc, racers = 4, 64
	b := newTestBackend(t, conc)
	var got int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if b.tryAcquire() {
				atomic.AddInt64(&got, 1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := atomic.LoadInt64(&got); n != conc {
		t.Fatalf("抢到位 = %d, 期望恰好 %d（不能超发）", n, conc)
	}
	if in := b.InFlight(); in != conc {
		t.Fatalf("在飞数 = %d, 期望 %d", in, conc)
	}
}

// 关键语义：满了不等于降级 —— 排队等的是同一个后端。
func TestAcquireWaitSucceedsAfterRelease(t *testing.T) {
	b := newTestBackend(t, 1)
	if !b.tryAcquire() {
		t.Fatal("先占满唯一并发位")
	}
	done := make(chan bool, 1)
	go func() { done <- b.acquireWait(context.Background(), 5*time.Second) }()

	// 排队者应该确实挂住了（在飞数不变，waiters=1）
	deadline := time.Now().Add(time.Second)
	for b.Waiters() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if w := b.Waiters(); w != 1 {
		t.Fatalf("排队者 = %d, 期望 1", w)
	}
	if in := b.InFlight(); in != 1 {
		t.Fatalf("排队期间在飞数不该变：%d", in)
	}

	b.release() // 有人让出并发位 → 唤醒排队者
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("release 后 acquireWait 应拿到并发位")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acquireWait 没被唤醒")
	}
	if in := b.InFlight(); in != 1 {
		t.Fatalf("交接后在飞数 = %d, 期望 1", in)
	}
	if w := b.Waiters(); w != 0 {
		t.Fatalf("排队者已退出，waiters = %d, 期望 0", w)
	}
}

// 客户端断开时不该继续占着排队位空等。
func TestAcquireWaitRespectsContextCancel(t *testing.T) {
	b := newTestBackend(t, 1)
	if !b.tryAcquire() {
		t.Fatal("先占满唯一并发位")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- b.acquireWait(ctx, 30*time.Second) }()

	deadline := time.Now().Add(time.Second)
	for b.Waiters() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if w := b.Waiters(); w != 1 {
		t.Fatalf("排队者 = %d, 期望 1", w)
	}
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("ctx 取消后 acquireWait 应返回 false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 acquireWait 没及时返回")
	}
	if w := b.Waiters(); w != 0 {
		t.Fatalf("waiters = %d, 期望 0（退出时必须减回去）", w)
	}
}

func TestAcquireWaitZeroWaitFailsImmediately(t *testing.T) {
	b := newTestBackend(t, 1)
	if !b.tryAcquire() {
		t.Fatal("占满")
	}
	if b.acquireWait(context.Background(), 0) {
		t.Fatal("wait<=0 且无空位时应立即返回 false")
	}
}

func TestAcquireWaitSucceedsWhenFree(t *testing.T) {
	b := newTestBackend(t, 1)
	if !b.acquireWait(context.Background(), time.Second) {
		t.Fatal("有空位时 acquireWait 应直接成功")
	}
}

// ── 容量默认值 ──────────────────────────────────────────────

// 16 是搬入前的默认并发位，别改成别的数。
func TestInitDefaults(t *testing.T) {
	b := &Backend{Name: "d", Enabled: true}
	b.Init(nil)
	if b.Concurrency != 16 {
		t.Fatalf("默认并发位 = %d, 期望 16", b.Concurrency)
	}
	if b.Weight != 1.0 {
		t.Fatalf("默认权重 = %v, 期望 1.0", b.Weight)
	}
	if b.freeCh == nil {
		t.Fatal("freeCh 必须初始化")
	}
	if cap(b.freeCh) != 256 {
		t.Fatalf("freeCh 容量 = %d, 期望 256", cap(b.freeCh))
	}
	if b.stats == nil {
		t.Fatal("stats 必须初始化")
	}
}

func TestInitSharesAccountKeyPool(t *testing.T) {
	acct := &Account{Name: "main", BaseURL: "http://a", APIKeys: []string{"k1", "k2"}}
	b1 := &Backend{Name: "b1", Account: "main", Enabled: true}
	b2 := &Backend{Name: "b2", Account: "main", Enabled: true}
	m := map[string]*Account{"main": acct}
	b1.Init(m)
	b2.Init(m)
	if b1.BoundAccount() != acct || b2.BoundAccount() != acct {
		t.Fatal("同账号后端应共享同一个 key 池")
	}
	if b1.BaseURL != "http://a" {
		t.Fatalf("base_url 应从账号继承：%q", b1.BaseURL)
	}
}

func TestInitUnknownAccountFallsBackToInline(t *testing.T) {
	b := &Backend{Name: "b", Account: "nope", BaseURL: "http://self",
		APIKeys: []string{"inline"}, Enabled: true}
	b.Init(map[string]*Account{})
	if b.BoundAccount().Name != "b" {
		t.Fatalf("引用不存在的账号应退化成匿名账号，实得 %q", b.BoundAccount().Name)
	}
	if len(b.BoundAccount().APIKeys) != 1 {
		t.Fatal("内联 key 应并入账号池")
	}
}

// 账号级轮转游标：同账号所有后端共用一个序列。
func TestAccountKeySeqIsShared(t *testing.T) {
	acct := &Account{}
	if got := acct.KeySeqNext(); got != 0 {
		t.Fatalf("首次取号 = %d, 期望 0", got)
	}
	if got := acct.KeySeqNext(); got != 1 {
		t.Fatalf("再次取号 = %d, 期望 1", got)
	}
}

// ── 健康：冷却的设置 / 到期 / 清除 ────────────────────────────

func TestCooldownSetAndExpire(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)
	b.SetCooldown(60*time.Second, errclass.RateLimit429)

	if got := b.SkipReason(*cur); got != errclass.SkipCooldown {
		t.Fatalf("冷却期内跳过原因 = %q, 期望 skip_cooldown", got)
	}
	// 到期瞬间的边界：条件是 now < cooldownUntil，所以等于到期点时已放行
	if got := b.SkipReason(cur.Add(60 * time.Second)); got != "" {
		t.Fatalf("到期后应可用，得到 %q", got)
	}
	if got := b.CooldownReason(); got != errclass.RateLimit429 {
		t.Fatalf("冷却原因 = %q", got)
	}
}

// 短冷却不能覆盖已存在的长冷却（否则一次网络抖动就能把 4 小时锁屏解开）。
func TestCooldownDoesNotShorten(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)
	b.SetCooldown(4*time.Hour, errclass.Forbidden403)
	b.SetCooldown(60*time.Second, errclass.Network)
	if got := b.SkipReason(cur.Add(2 * time.Hour)); got != errclass.SkipCooldown {
		t.Fatal("短冷却不应覆盖长冷却")
	}
}

func TestClearCooldown(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)
	b.SetCooldown(4*time.Hour, errclass.Forbidden403)
	b.Trip(*cur)
	for i := 0; i < 3; i++ {
		b.NoteFailure(errclass.ServerError5xx, *cur, "boom", 500, false)
	}
	b.ClearCooldown()

	if got := b.SkipReason(*cur); got != "" {
		t.Fatalf("清除后应可用，得到 %q", got)
	}
	if b.FailStreak() != 0 {
		t.Fatalf("failStreak = %d, 期望 0", b.FailStreak())
	}
	if b.CooldownReason() != "" {
		t.Fatalf("原因应清空，得到 %q", b.CooldownReason())
	}
	if !b.IsAvailable(*cur) {
		t.Fatal("熔断应被清除")
	}
}

func TestSkipReasonDisabled(t *testing.T) {
	cur := setClock(t, baseTime)
	b := &Backend{Name: "off", Concurrency: 1}
	b.Init(nil)
	if got := b.SkipReason(*cur); got != errclass.SkipDisabled {
		t.Fatalf("未启用的后端跳过原因 = %q, 期望 skip_disabled", got)
	}
}

// ── 健康：配额类指数退避阶梯 ────────────────────────────────
// 403 的基础时长是 5 分钟，所以阶梯是 5m→10m→20m→40m→1h20m→2h40m→4h 封顶。
// 这套数字是 10-01 的 403 自锁风暴调出来的：**不要改成线性或无上限**。

func TestQuotaCooldownExponentialLadder(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)

	want := []time.Duration{
		5 * time.Minute,
		10 * time.Minute,
		20 * time.Minute,
		40 * time.Minute,
		80 * time.Minute,  // 1h20m
		160 * time.Minute, // 2h40m
		4 * time.Hour,     // 封顶
		4 * time.Hour,     // 封顶后不再增长
	}
	for i, w := range want {
		b.quotaCooldown(errclass.Forbidden403)
		got := time.Duration(atomic.LoadInt64(&b.cooldownUntil) - cur.UnixNano())
		if got != w {
			t.Fatalf("第 %d 次配额冷却 = %v, 期望 %v（阶梯 5m→10m→…→4h 封顶）", i+1, got, w)
		}
	}
}

func TestQuotaCooldownPerClassBase(t *testing.T) {
	cur := setClock(t, baseTime)
	cases := []struct {
		c    Class
		want time.Duration
	}{
		{errclass.RateLimit429, 60 * time.Second},
		{errclass.Forbidden403, 300 * time.Second},
		{errclass.Auth401, 600 * time.Second},
	}
	for _, tc := range cases {
		b := newTestBackend(t, 4)
		b.quotaCooldown(tc.c)
		got := time.Duration(atomic.LoadInt64(&b.cooldownUntil) - cur.UnixNano())
		if got != tc.want {
			t.Errorf("%s 首击冷却 = %v, 期望 %v", tc.c, got, tc.want)
		}
	}
}

// ── 健康：strike 的归零语义 ─────────────────────────────────
// strike 只在成功时归零。这是 10-01 自锁风暴的另一半：没有归零就没有解锁。

func TestStrikeOnlyResetsOnSuccess(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)

	b.quotaCooldown(errclass.Forbidden403)
	b.quotaCooldown(errclass.Forbidden403)
	if got := atomic.LoadInt64(&b.cooldownStrike); got != 2 {
		t.Fatalf("strike = %d, 期望 2", got)
	}

	// 失败不该清 strike
	for i := 0; i < 3; i++ {
		b.NoteFailure(errclass.Forbidden403, *cur, "403", 403, false)
	}
	if got := atomic.LoadInt64(&b.cooldownStrike); got != 2 {
		t.Fatalf("失败后 strike = %d, 期望仍是 2", got)
	}

	// 成功一次 = 密码输对了
	b.NoteSuccessMetrics(120, 30)
	if got := atomic.LoadInt64(&b.cooldownStrike); got != 0 {
		t.Fatalf("成功后 strike = %d, 期望 0", got)
	}
	if got := b.FailStreak(); got != 0 {
		t.Fatalf("成功后 failStreak = %d, 期望 0", got)
	}
}

// 退避阶梯在成功之后重新从头开始。
//
// 注意 SetCooldown 只会延长、不会缩短（与搬入前一致：一次短冷却不该把已有的
// 长冷却拉低）。所以这里把时钟推过旧的冷却窗口，才能量到新一轮的真实时长。
func TestCooldownLadderRestartsAfterSuccess(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)
	b.quotaCooldown(errclass.Forbidden403) // 5m
	b.quotaCooldown(errclass.Forbidden403) // 10m
	if got := atomic.LoadInt64(&b.cooldownStrike); got != 2 {
		t.Fatalf("strike = %d, 期望 2", got)
	}

	b.NoteSuccessMetrics(10, 5) // 密码输对了 → strike 归零
	if got := atomic.LoadInt64(&b.cooldownStrike); got != 0 {
		t.Fatalf("成功后 strike = %d, 期望 0", got)
	}

	*cur = cur.Add(cooldownCap) // 跳过旧冷却窗口
	b.quotaCooldown(errclass.Forbidden403)
	if got := time.Duration(atomic.LoadInt64(&b.cooldownUntil) - cur.UnixNano()); got != 5*time.Minute {
		t.Fatalf("成功后再冷却 = %v, 期望回到基础 5m", got)
	}
}

// ── 健康：熔断 ──────────────────────────────────────────────
// failsToTrip=5 是生产事故调出来的。

func TestCircuitTripsAfterFiveFails(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)

	for i := 0; i < int(failsToTrip)-1; i++ {
		b.NoteFailure(errclass.ServerError5xx, *cur, "boom", 500, false)
		if b.IsAvailable(*cur) != true {
			t.Fatalf("第 %d 次失败后不该跳闸（failsToTrip=%d）", i+1, failsToTrip)
		}
	}
	b.NoteFailure(errclass.ServerError5xx, *cur, "boom", 500, false)

	if b.IsAvailable(*cur) {
		t.Fatalf("连续 %d 次失败应跳闸", failsToTrip)
	}
	if got := b.SkipReason(*cur); got != errclass.SkipCircuit {
		t.Fatalf("跳闸后跳过原因 = %q, 期望 skip_circuit", got)
	}
	// 跳闸 2 分钟后半开
	if !b.IsAvailable(cur.Add(circuitCooldown + time.Second)) {
		t.Fatal("超过 circuitCooldown 应半开")
	}
	if got := b.SkipReason(cur.Add(circuitCooldown + time.Second)); got != "" {
		t.Fatalf("半开后应可用，得到 %q", got)
	}
}

func TestSoftFailuresDoNotTrip(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)
	for i := 0; i < 20; i++ {
		b.NoteFailure(errclass.EmptyOutput, *cur, "empty", 200, false)
		b.NoteFailure(errclass.NoContent, *cur, "no choices", 200, false)
	}
	if !b.IsAvailable(*cur) {
		t.Fatal("空气泡类不该跳闸：否则一次 max_tokens 太小的请求就能踢掉健康后端")
	}
	if got := b.FailStreak(); got != 0 {
		t.Fatalf("failStreak = %d, 期望 0", got)
	}
	if got := b.SkipReason(*cur); got != "" {
		t.Fatalf("空气泡类不该冷却，得到 %q", got)
	}
}

func TestClientBadRequestDoesNotTrip(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)
	for i := 0; i < 20; i++ {
		b.NoteFailure(errclass.BadRequest400, *cur, "bad body", 400, false)
	}
	if !b.IsAvailable(*cur) {
		t.Fatal("客户端的 400 不能把健康后端熔断掉")
	}
	if got := b.SkipReason(*cur); got != "" {
		t.Fatalf("客户端的 400 不该冷却，得到 %q", got)
	}
}

// 模型下架必须真的计入熔断 —— 否则就是那条静默黑洞（10-06，空转 4 天半）。
func TestModelUnavailableTrips(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)
	for i := 0; i < int(failsToTrip); i++ {
		b.NoteFailure(errclass.ModelUnavailable, *cur, "model not found", 400, false)
	}
	if b.IsAvailable(*cur) {
		t.Fatal("模型下架连续失败应熔断摘掉该后端（否则是静默黑洞）")
	}
}

// ── 健康：⚠️ 10-03 回归：客户端断连不许罚后端 ─────────────────
// 一次断连让所有在飞后端同时冷却 60s → 候选清空 → 全网 1 分钟 503（1720 次）。

func TestClientGoneSkipsCooldown(t *testing.T) {
	cur := setClock(t, baseTime)
	for _, c := range []Class{errclass.Network, errclass.Timeout} {
		b := newTestBackend(t, 4)
		b.NoteFailure(c, *cur, "context canceled", 0, true)

		if until := atomic.LoadInt64(&b.cooldownUntil); until != 0 {
			t.Fatalf("%s + clientGone 不该设冷却，却设到了 %v", c,
				time.Unix(0, until).Sub(*cur))
		}
		if got := b.SkipReason(*cur); got != "" {
			t.Fatalf("%s + clientGone 后端应仍可用，得到 %q", c, got)
		}
		if got := b.FailStreak(); got != 0 {
			t.Fatalf("%s + clientGone 不该计入连续失败，得到 %d", c, got)
		}
	}
}

// 客户端没断时，60 秒固定冷却照旧（不能因为上面那条就把网络抖动也放过）。
func TestNetworkWithoutClientGoneStillCools(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)
	b.NoteFailure(errclass.Network, *cur, "connection reset", 0, false)

	got := time.Duration(atomic.LoadInt64(&b.cooldownUntil) - cur.UnixNano())
	if got != 60*time.Second {
		t.Fatalf("网络类固定冷却 = %v, 期望 60s", got)
	}
	if got := b.SkipReason(*cur); got != errclass.SkipCooldown {
		t.Fatalf("应处于冷却，得到 %q", got)
	}
}

// 网络/超时类不涨 failStreak：一次网络抖动不该熔断后端。
func TestTransientDoesNotTrip(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)
	for i := 0; i < 20; i++ {
		b.NoteFailure(errclass.Network, *cur, "reset", 0, false)
	}
	if !b.IsAvailable(*cur) {
		t.Fatal("网络抖动不该跳闸（跳闸只留给真故障）")
	}
}

// ── 健康：慢探测窗口 ────────────────────────────────────────
// slowFailsToOpen=2：连续两次首字超时进观察窗，之后用 8 秒短探针。

func TestSlowProbeWindow(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)
	b.Timeout = 300 // 常规首字上限会被 min(300, 50) = 50s 压住

	if got := b.FirstTokenTimeout(*cur); got != 50*time.Second {
		t.Fatalf("常规首字超时 = %v, 期望 50s", got)
	}

	b.NoteFailure(errclass.FirstTokenTimeout, *cur, "ft timeout", 0, false)
	if got := b.FirstTokenTimeout(*cur); got != 50*time.Second {
		t.Fatalf("只慢一次不该进观察窗，得到 %v", got)
	}

	b.NoteFailure(errclass.FirstTokenTimeout, *cur, "ft timeout", 0, false)
	if got := b.FirstTokenTimeout(*cur); got != 8*time.Second {
		t.Fatalf("连续 %d 次首字超时后应换短探针，得到 %v", slowFailsToOpen, got)
	}

	// 观察窗到期 → 恢复常规
	if got := b.FirstTokenTimeout(cur.Add(slowWindow + time.Second)); got != 50*time.Second {
		t.Fatalf("观察窗过期后应恢复 50s，得到 %v", got)
	}
}

// 首字超时也涨 failStreak（后端还在响应，只是慢 —— 但慢到没边也该摘）。
func TestFirstTokenTimeoutCountsTowardTrip(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)
	for i := 0; i < int(failsToTrip); i++ {
		b.NoteFailure(errclass.FirstTokenTimeout, *cur, "ft timeout", 0, false)
	}
	if got := b.FailStreak(); got != failsToTrip {
		t.Fatalf("failStreak = %d, 期望 %d", got, failsToTrip)
	}
}

func TestSuccessClearsSlowWindow(t *testing.T) {
	cur := setClock(t, baseTime)
	b := newTestBackend(t, 4)
	b.Timeout = 300
	for i := 0; i < 2; i++ {
		b.NoteFailure(errclass.FirstTokenTimeout, *cur, "ft", 0, false)
	}
	if got := b.FirstTokenTimeout(*cur); got != 8*time.Second {
		t.Fatalf("应处于慢探测窗，得到 %v", got)
	}
	b.NoteSuccessMetrics(200, 40)
	if got := b.FirstTokenTimeout(*cur); got != 50*time.Second {
		t.Fatalf("成功后应退出慢探测窗，得到 %v", got)
	}
}

// ── 健康：EffectiveModel ────────────────────────────────────

func TestEffectiveModel(t *testing.T) {
	b := &Backend{Name: "m", Model: "old", ModelFallback: "new"}
	if got := b.EffectiveModel(baseTime); got != "old" {
		t.Fatalf("未设过期时间 = %q, 期望 old", got)
	}
	b.ModelExpire = baseTime.Add(time.Hour).Format(time.RFC3339)
	if got := b.EffectiveModel(baseTime); got != "old" {
		t.Fatalf("未过期 = %q, 期望 old", got)
	}
	if got := b.EffectiveModel(baseTime.Add(2 * time.Hour)); got != "new" {
		t.Fatalf("过期后 = %q, 期望 new", got)
	}
	b.ModelExpire = "不是 RFC3339"
	if got := b.EffectiveModel(baseTime); got != "old" {
		t.Fatalf("解析失败应回落到原模型，得到 %q", got)
	}
}

// ── 统计 ────────────────────────────────────────────────────

func TestPercentileBoundaries(t *testing.T) {
	empty := []int64{}
	if got := percentile(empty, 0, 50); got != 0 {
		t.Fatalf("空缓冲 = %d, 期望 0", got)
	}
	zeros := make([]int64, 256) // 全是环形缓冲的未填充槽位
	if got := percentile(zeros, 0, 99); got != 0 {
		t.Fatalf("未填充的环形缓冲 = %d, 期望 0", got)
	}
	one := []int64{0, 42, 0}
	if got := percentile(one, 0, 50); got != 42 {
		t.Fatalf("单样本 p50 = %d, 期望 42", got)
	}
	if got := percentile(one, 0, 99); got != 42 {
		t.Fatalf("单样本 p99 = %d, 期望 42", got)
	}
	// 1..100：最近邻取整 → p50=50 p95=95 p99=99 p100=100
	buf := make([]int64, 256)
	for i := 1; i <= 100; i++ {
		buf[i-1] = int64(i)
	}
	for _, tc := range []struct {
		p    float64
		want int64
	}{
		{50, 50}, {95, 95}, {99, 99}, {100, 100},
	} {
		if got := percentile(buf, 0, tc.p); got != tc.want {
			t.Errorf("p%.0f = %d, 期望 %d", tc.p, got, tc.want)
		}
	}
}

func TestLatencyPcts(t *testing.T) {
	s := newBackendStats()
	for i := 1; i <= 100; i++ {
		s.NoteLatency(int64(i))
	}
	p50, p95, p99 := s.LatencyPcts()
	if p50 != 50 || p95 != 95 || p99 != 99 {
		t.Fatalf("p50/p95/p99 = %d/%d/%d, 期望 50/95/99", p50, p95, p99)
	}
	if p50, _, _ := s.FirstTokenPcts(); p50 != 0 {
		t.Fatalf("没记过首字延迟时 p50 = %d, 期望 0", p50)
	}

	ft := newBackendStats()
	for i := 1; i <= 100; i++ {
		ft.NoteFirstToken(int64(i))
	}
	f50, f95, f99 := ft.FirstTokenPcts()
	if f50 != 50 || f95 != 95 || f99 != 99 {
		t.Fatalf("首字 p50/p95/p99 = %d/%d/%d, 期望 50/95/99", f50, f95, f99)
	}
}

// 环形缓冲写满一圈后要正确滚动，不能把旧值当新值。
func TestLatencyRingWraps(t *testing.T) {
	s := newBackendStats()
	// 写满一圈：1..256（槽位 0..255）
	for i := 1; i <= 256; i++ {
		s.NoteLatency(int64(i))
	}
	p50, _, p99 := s.LatencyPcts()
	if p50 != 128 || p99 != 254 {
		t.Fatalf("整圈后 p50/p99 = %d/%d, 期望 128/254", p50, p99)
	}
	// 再写 100 个：300..399 覆盖槽位 0..99，剩下的是 101..256
	for i := 300; i < 400; i++ {
		s.NoteLatency(int64(i))
	}
	p50, _, _ = s.LatencyPcts()
	// 滚动后的样本集是 101..256 ∪ 300..399（256 个），第 128 小 = 228
	if p50 != 228 {
		t.Fatalf("滚动后 p50 = %d, 期望 228", p50)
	}
	// 新值必须优先于被覆盖前的旧值：窗口只看最近 256 条，不多不少
	if got := s.Count(errclass.ServerError5xx); got != 0 {
		t.Fatalf("不该有 5xx 计数，得到 %d", got)
	}
}

func TestStatsCounters(t *testing.T) {
	b := newTestBackend(t, 2)
	s := b.Stats()
	for i := 0; i < 3; i++ {
		s.NoteAttempt()
	}
	s.noteSuccess()
	if got := s.Attempts(); got != 3 {
		t.Fatalf("attempts = %d, 期望 3", got)
	}
	if got := s.Success(); got != 1 {
		t.Fatalf("success = %d, 期望 1", got)
	}
	if got := s.Recent60s(); got != 3 {
		t.Fatalf("recent60s = %d, 期望 3", got)
	}
	s.ResetRecent60s()
	if got := s.Recent60s(); got != 0 {
		t.Fatalf("清零后 = %d, 期望 0", got)
	}

	// 每类都要有预分配的槽位，否则统计会静默丢数
	for _, c := range classList {
		if got := s.Count(c); got != 0 {
			t.Errorf("%q 初始计数 = %d, 期望 0", c, got)
		}
	}
	s.Bump(errclass.ServerError5xx)
	s.Bump(errclass.ServerError5xx)
	if got := s.Count(errclass.ServerError5xx); got != 2 {
		t.Fatalf("5xx 计数 = %d, 期望 2", got)
	}
}

func TestSetLastError(t *testing.T) {
	cur := setClock(t, baseTime)
	s := newBackendStats()
	s.SetLastError(errclass.Forbidden403, 403, "no access")
	c, st, msg, at := s.LastError()
	if c != errclass.Forbidden403 || st != 403 || msg != "no access" {
		t.Fatalf("lastError = %q/%d/%q", c, st, msg)
	}
	if !at.Equal(*cur) {
		t.Fatalf("lastAt = %v, 期望 %v", at, *cur)
	}
}

// ── HTTP 连接池 ─────────────────────────────────────────────

func TestGetClientReuses(t *testing.T) {
	b := newTestBackend(t, 1)
	c1 := b.Client()
	if c1 == nil {
		t.Fatal("client 不该为 nil")
	}
	if c2 := b.Client(); c1 != c2 {
		t.Fatal("client 应复用，不能每次新建（连接池就白费了）")
	}
	if c1.Timeout != 300*time.Second {
		t.Fatalf("默认超时 = %v, 期望 300s", c1.Timeout)
	}
	b2 := &Backend{Name: "t2", Timeout: 30, Enabled: true}
	b2.Init(nil)
	if got := b2.Client().Timeout; got != 30*time.Second {
		t.Fatalf("配置超时 = %v, 期望 30s", got)
	}
}

// ── 类型别名 ────────────────────────────────────────────────
// upstream.Class 必须是 errclass.Class 的别名（不是新定义），否则调用点要大面积改。

func TestClassIsAliasOfErrclass(t *testing.T) {
	var a Class = errclass.ModelUnavailable
	var b errclass.Class = a
	if b != errclass.ModelUnavailable {
		t.Fatal("别名不成立")
	}
	if clsSkipCap != errclass.SkipCapacity || clsModelGone != errclass.ModelUnavailable {
		t.Fatal("本地短名与 errclass 常量对不上")
	}
}
