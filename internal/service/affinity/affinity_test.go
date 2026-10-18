package affinity

// 测试纪律（同 upstream/errclass 各包）：全表驱动，不起服务、不打桩网络。
// TTL 用包内 now 的可注入时钟（生产恒 time.Now，见 affinity.go 注释），
// 没有 sleep。

import (
	"errors"
	"sync"
	"testing"
	"time"

	"shanhai-gateway/internal/core/errclass"
)

// fakeClock 把包内 now 换成注入时钟，返回恢复函数。
func fakeClock(t *testing.T, start time.Time) func() {
	t.Helper()
	now = func() time.Time { return start }
	return func() { now = time.Now }
}

func TestPutGetRoundtrip(t *testing.T) {
	tbl := New(Options{})
	if tbl.Len() != 0 {
		t.Fatalf("fresh table len=%d", tbl.Len())
	}
	tbl.Put("sess-a", "be-1")
	backend, contention, ok := tbl.Get("sess-a")
	if !ok || backend != "be-1" || contention != 0 {
		t.Fatalf("Get= (%q,%d,%v)", backend, contention, ok)
	}
	if _, _, ok := tbl.Get("nope"); ok {
		t.Fatal("unknown key must miss")
	}
	// puts 口径（2026-10-18 与 SLR 对齐）：真实 Put 计数、同键刷新也计、
	// 空键/空 backend 不计（SLR 同守卫）。
	if got := tbl.Stats().Puts; got != 1 {
		t.Fatalf("Puts=%d want 1", got)
	}
	tbl.Put("sess-a", "be-1")
	if got := tbl.Stats().Puts; got != 2 {
		t.Fatalf("Puts after refresh=%d want 2", got)
	}
	tbl.Put("", "be-1")
	tbl.Put("sess-a", "")
	if got := tbl.Stats().Puts; got != 2 {
		t.Fatalf("empty key/backend must not count: Puts=%d want 2", got)
	}
}

func TestLRUEvictsOldest(t *testing.T) {
	tbl := New(Options{Max: 2})
	tbl.Put("k1", "b1")
	tbl.Put("k2", "b2")
	tbl.Put("k3", "b3") // 容量满 → 最旧的 k1 被挤出
	if _, _, ok := tbl.Get("k1"); ok {
		t.Fatal("oldest k1 should be evicted")
	}
	if _, _, ok := tbl.Get("k2"); !ok {
		t.Fatal("k2 should survive")
	}
	if _, _, ok := tbl.Get("k3"); !ok {
		t.Fatal("k3 should survive")
	}
	if got := tbl.Len(); got != 2 {
		t.Fatalf("len=%d", got)
	}
	// 命中会刷新 LRU 顺序：先摸 k2，再插 k4 → 挤出的就是 k3
	tbl.Get("k2")
	tbl.Put("k4", "b4")
	if _, _, ok := tbl.Get("k3"); ok {
		t.Fatal("k3 (least recently used) should be evicted after k2 was touched")
	}
	if got := tbl.Stats().Evictions; got != 2 {
		t.Fatalf("evictions=%d want 2 (k1 + k3)", got)
	}
}

func TestTTLExpiryOnRead(t *testing.T) {
	start := time.Unix(1700000000, 0)
	defer fakeClock(t, start)()
	tbl := New(Options{TTL: time.Hour}) // 显式很短的 TTL：到期即删

	tbl.Put("s", "b")
	// 半小时后：还在
	fakeClock(t, start.Add(30*time.Minute))
	if _, _, ok := tbl.Get("s"); !ok {
		t.Fatal("within TTL should hit")
	}
	// 1h+ 之后：过期即删
	fakeClock(t, start.Add(2*time.Hour))
	if _, _, ok := tbl.Get("s"); ok {
		t.Fatal("past TTL should miss")
	}
	if tbl.Len() != 0 {
		t.Fatalf("expired entry must be deleted, len=%d", tbl.Len())
	}
	if got := tbl.Stats().Evictions; got != 1 {
		t.Fatalf("evictions=%d want 1 (TTL expiry)", got)
	}
	if got := tbl.Stats().Misses; got != 1 {
		t.Fatalf("misses=%d want 1", got)
	}
}

func TestForgetOnlyWhenPresent(t *testing.T) {
	tbl := New(Options{})
	ev0 := tbl.Stats().Evictions
	tbl.Forget("ghost") // 没有条目：无效果、计数不动
	if got := tbl.Stats().Evictions; got != ev0 {
		t.Fatalf("Forget(ghost) bumped evictions: %d", got)
	}
	tbl.Put("s", "b")
	tbl.Forget("s")
	if _, _, ok := tbl.Get("s"); ok {
		t.Fatal("Forget must remove the pin")
	}
	if got := tbl.Stats().Evictions; got != ev0+1 {
		t.Fatalf("evictions=%d want +1", got)
	}
}

func TestNoteContentionHysteresis(t *testing.T) {
	tbl := New(Options{}) // ContentionLimit 默认 3
	tbl.Put("s", "b")
	tbl.NoteContention("s") // 1
	if _, c, ok := tbl.Get("s"); !ok || c != 1 {
		t.Fatalf("after 1st contention: (%q,%d,%v)", "b", c, ok)
	}
	tbl.NoteContention("s") // 2 → 仍在（滞回：满载不是故障）
	if _, c, ok := tbl.Get("s"); !ok || c != 2 {
		t.Fatalf("after 2nd contention: still pinned expected, got (%d,%v)", c, ok)
	}
	tbl.NoteContention("s") // 3 → 达到阈值：迁移（拆钉）
	if _, _, ok := tbl.Get("s"); ok {
		t.Fatal("3 consecutive contentions must migrate (drop the pin)")
	}
	if got := tbl.Stats().Evictions; got != 1 {
		t.Fatalf("migration must count one eviction, got %d", got)
	}
	// 对不存在的 key：无效果
	ev := tbl.Stats().Evictions
	tbl.NoteContention("ghost")
	if got := tbl.Stats().Evictions; got != ev {
		t.Fatalf("NoteContention(ghost) must be a no-op, evictions=%d", got)
	}
}

func TestPutResetsContention(t *testing.T) {
	tbl := New(Options{})
	tbl.Put("s", "b")
	tbl.NoteContention("s")
	tbl.NoteContention("s")
	tbl.Put("s", "b") // 成功一次 = 滞回清零
	if _, c, ok := tbl.Get("s"); !ok || c != 0 {
		t.Fatalf("Put must reset contention, got (%d,%v)", c, ok)
	}
}

func TestPutReplacesBackend(t *testing.T) {
	tbl := New(Options{})
	tbl.Put("s", "old")
	tbl.Put("s", "new")
	if backend, _, ok := tbl.Get("s"); !ok || backend != "new" {
		t.Fatalf("re-Put must replace backend, got (%q,%v)", backend, ok)
	}
	if got := tbl.Len(); got != 1 {
		t.Fatalf("re-Put must not duplicate the entry, len=%d", got)
	}
}

func TestCountPerBackendAndLen(t *testing.T) {
	start := time.Unix(1700000000, 0)
	defer fakeClock(t, start)()
	tbl := New(Options{TTL: time.Hour})
	tbl.Put("a1", "be1")
	tbl.Put("a2", "be1")
	tbl.Put("a3", "be2")
	want := map[string]int{"be1": 2, "be2": 1}
	got := tbl.CountPerBackend()
	if len(got) != 2 || got["be1"] != 2 || got["be2"] != 1 {
		t.Fatalf("CountPerBackend=%v want %v", got, want)
	}
	// 过期条目从观测口清掉，不报"表里还有"。
	fakeClock(t, start.Add(2*time.Hour))
	if got := tbl.CountPerBackend(); len(got) != 0 {
		t.Fatalf("expired entries must vanish from observation: %v", got)
	}
}

func TestDefaults(t *testing.T) {
	tbl := New(Options{})
	if tbl.opt.Max != defaultMax || tbl.opt.TTL != defaultTTL || tbl.opt.ContentionLimit != defaultContentionLimit {
		t.Fatalf("defaults not filled: %+v", tbl.opt)
	}
	// Evict nil → errclass.ReleaseAffinity（同源判定，不注入也逐位一致）
	if _, _, ok := tbl.Get("none"); ok {
		t.Fatal("empty table must miss")
	}
}

func TestRelinquishDefaultPolicy(t *testing.T) {
	tbl := New(Options{})
	// 逐出类：429 / 5xx / model_unavailable / 404
	for _, c := range []errclass.Class{
		errclass.RateLimit429, errclass.ServerError5xx, errclass.NotFound404,
		errclass.ModelUnavailable, errclass.Auth401, errclass.Forbidden403,
	} {
		tbl.Put("s", "b")
		if !tbl.Relinquish("s", c) {
			t.Fatalf("%s should relinquish the sticky", c)
		}
		if _, _, ok := tbl.Get("s"); ok {
			t.Fatalf("%s must have dropped the pin", c)
		}
	}
	// 保持类：400 / 瞬时网络 / 空气泡 / mid-stream / clientGone(network) / skip 类
	for _, c := range []errclass.Class{
		errclass.BadRequest400, errclass.Network, errclass.Timeout,
		errclass.MidStreamFail, errclass.NoContent, errclass.EmptyOutput,
		errclass.SkipCapacity, errclass.SkipCooldown,
	} {
		tbl.Put("s", "b")
		if tbl.Relinquish("s", c) {
			t.Fatalf("%s must NOT relinquish the sticky", c)
		}
		if _, _, ok := tbl.Get("s"); !ok {
			t.Fatalf("%s must keep the pin", c)
		}
	}
}

func TestRelinquishCustomPolicy(t *testing.T) {
	// Evict 接缝只给测试：注入恒逐出策略 → 任何类都拆
	tbl := New(Options{Evict: func(errclass.Class) bool { return true }})
	tbl.Put("s", "b")
	if !tbl.Relinquish("s", errclass.BadRequest400) {
		t.Fatal("custom always-evict policy must relinquish")
	}
	if _, _, ok := tbl.Get("s"); ok {
		t.Fatal("custom policy evicted pin must be gone")
	}
}

func TestStatsCounters(t *testing.T) {
	tbl := New(Options{})
	tbl.Put("hit", "b")
	tbl.Put("gone", "b")
	tbl.Forget("gone")
	st := tbl.Stats()
	if st.Hits != 0 || st.Misses != 0 || st.Evictions != 1 {
		t.Fatalf("initial stats: %+v", st)
	}
	tbl.Get("hit")   // hit
	tbl.Get("gone")  // miss（已被逐出）
	tbl.Get("other") // miss
	st = tbl.Stats()
	if st.Hits != 1 || st.Misses != 2 {
		t.Fatalf("stats after gets: %+v", st)
	}
}

func TestPutRejectsEmpty(t *testing.T) {
	tbl := New(Options{})
	tbl.Put("", "b")
	tbl.Put("s", "")
	if tbl.Len() != 0 {
		t.Fatalf("empty key/backend must not enter the table, len=%d", tbl.Len())
	}
}

func TestConcurrentAccess(t *testing.T) {
	// -race 干净：多 goroutine 同 key 同写 + 同 Get + Forget/contention 混流
	tbl := New(Options{Max: 64})
	const workers = 8
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				key := "hot-key"
				switch i % 4 {
				case 0:
					tbl.Put(key, "backend-a")
				case 1:
					backend, _, ok := tbl.Get(key)
					if ok && backend == "" {
						errCh <- errors.New("hit with empty backend")
						return
					}
				case 2:
					tbl.NoteContention(key)
					tbl.Forget(key)
				case 3:
					_ = tbl.CountPerBackend()
					tbl.Put(key, "backend-b")
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if tbl.Len() > tbl.opt.Max {
		t.Fatalf("LRU overflow: len=%d max=%d", tbl.Len(), tbl.opt.Max)
	}
}
