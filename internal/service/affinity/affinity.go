// Package affinity —— 会话亲和表。
//
// 为什么存在（10-10 生产调查钉死的事实）：生产实测缓存命中率 0%。根因不是
// 客户端没传 X-Session-Key，而是 SWRR 把同一对话轮流甩给不同 provider ——
// 前缀缓存在 provider 侧，同一对话第二轮落到另一家，缓存根本不在同一个地方。
// 「网关知道是谁」（X-Session-Key）解决不了「落在别家」。
//
// 方案定位：**亲和是套在轮转外的一层「优先选择」，不是替代轮转。**
//   - 新会话首落仍由 SWRR（权重控制）决定；
//   - 已建立过粘性的会话在后续请求里**优先钉回**同一 backend，从而命中
//     provider 的前缀缓存（连续多轮 agent 循环的前缀是递增的，钉住即可命中）。
//
// ── 依赖纪律 ────────────────────────────────────────────────
// 本包只允许 import internal/core/errclass（逐出判断，见 Relinquish）+
// 标准库。**不许** import 任何 service/* 或 core/upstream——表里只存
// backend **名字**，不存 *upstream.Backend 指针：与 AtomicRouter 的原子
// 换装天然解耦（热重载换掉整只 Router，表里的旧名字最多在名字消失时被
// 逐出，绝不会隔着指针摸到旧实例）。runtime/env 也不在这里读——开关与
// 默认值属于装配层（cmd/gateway），本包必须能独立表驱动测试。
//
// ── 与健康层的关系 ─────────────────────────────────────────
// **affinity 是既有健康层的消费者，不发明第二个熔断器。**
// 逐出/保持的判断与「请求错了还是后端错了」的判断同源——都走
// errclass.ReleaseAffinity（见 errclass/policy.go）：mid-stream、客户端
// 取消、400、瞬时网络错都不放弃粘性；429、5xx、模型下架这些后端侧的错
// 才放弃。满载（并发位挤不进去）不是故障：只做滞回计数（contention），
// 连续 K 次都挤才迁移——不把 60s 排队当 backend fault。
package affinity

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"

	"shanhaigateway/internal/core/errclass"
)

// 默认参数。都是给生产直接跑的值：Max 8192 足够容下一天内活跃会话数，
// TTL 6h 覆盖 provider 前缀缓存的实际生存期，K=3 是滞回的经验值。
const (
	defaultMax             = 8192
	defaultTTL             = 6 * time.Hour
	defaultContentionLimit = 3
)

// EvictPolicy 逐出/保持策略：「这一类失败是否应该放弃该会话的粘性」。
//
// 默认实现是 errclass.ReleaseAffinity —— 判断与「请求错了还是后端错了」
// 同源，与冷却/熔断/兜底共用同一份语义。Options.Evict 只是测试接缝
// （不注入就与生产逐位一致），不是给运维开第二套策略的口子。
type EvictPolicy func(errclass.Class) bool

// Options 构造参数。零值字段一律回落默认值（表必须拿默认参数也能跑）。
type Options struct {
	// Max LRU 容量：超过后最旧的会话被挤出。默认 8192。
	Max int
	// TTL 条目过期时间（自 Put 起算）。默认 6h。
	TTL time.Duration
	// ContentionLimit 滞回迁移阈值：钉住的后端连续满载几次之后迁移。
	// 默认 3（env AFFINITY_CONTENTION_LIMIT 由装配层读好传进来）。
	// ≤0 一律回落默认值——把阈值关小到 0/负数等于把滞回关掉，"超时即逃"
	// 正是本设计要避免的行为，不是可选项。
	ContentionLimit int
	// Evict 逐出策略，nil → errclass.ReleaseAffinity（见包注释与 Relinquish）。
	Evict EvictPolicy
}

// entry 表条目。**只存名字**——不存指针，理由见包注释的依赖纪律。
type entry struct {
	key        string
	backend    string
	createdAt  time.Time // 自 Put 起算；TTL 过期判断用
	contention int       // 连续满载（钉住的后端 TryAcquire 挤不进去）次数
}

// Table 会话亲和表：LRU（双向链表 + map）+ TTL + 滞回迁移。
// 并发安全：整只 mutex（每个条目只有几十字节、每请求一把、持锁只有纯内存
// 操作），没有值得为它上读写锁的读路径——Get 也要动 LRU 顺序。
type Table struct {
	mu    sync.Mutex // 整只互斥：条目小、操作一次一锁，没有值得 RWMutex 的读路径
	opt   Options
	items map[string]*list.Element
	order *list.List // front = 最近使用（命中/Put 都移到队头），back = 最旧

	hits      atomic.Uint64 // Get 命中（表里有钉）
	misses    atomic.Uint64 // Get 未命中（没有钉 / 已过期）
	evictions atomic.Uint64 // 拆钉总次数（逐出策略 / 迁移 / LRU 挤出 / TTL 过期）
	puts      atomic.Uint64 // Put 调用次数（含同 backend 重复刷新；与 SLR 口径一致）
}

// New 构造亲和表。Options 零值字段回落默认值（见各字段注释）。
func New(opt Options) *Table {
	if opt.Max <= 0 {
		opt.Max = defaultMax
	}
	if opt.TTL <= 0 {
		opt.TTL = defaultTTL
	}
	if opt.ContentionLimit <= 0 {
		opt.ContentionLimit = defaultContentionLimit
	}
	if opt.Evict == nil {
		opt.Evict = errclass.ReleaseAffinity
	}
	return &Table{
		opt:   opt,
		items: make(map[string]*list.Element),
		order: list.New(),
	}
}

// now 表内「当前时间」的唯一来源（默认 time.Now）。
// 唯一存在的理由是可测性：TTL 语义不值得真等 6 小时。同一做法见
// upstream 包的 clock——生产路径恒等于 time.Now，逐位一致。
var now = time.Now

// Get 返回钉住的 backend 名（空=无钉）、该条目当前连续被挤次数、以及表里
// 是否有钉（ok）。命中即刷新 LRU 顺序。
//
// 观测口径：ok 时 hits++，!ok 时 misses++。注意「表里有钉但接线层没用上」
// （目标被健康层过滤、满载挤不进去）也算 miss 之前的 hit——亲和意图命中了，
// 失败的具体原因由 contention/eviction 计数与 [proxy] 日志给出。
//
// TTL 过期在读取时判定（now-createdAt > TTL 即删）：不需要独立的定期
// 清写者——一张 LRU 表里读不到的条目本来就不该活着。
func (t *Table) Get(key string) (backend string, contention int, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, has := t.items[key]
	if !has {
		t.misses.Add(1)
		return "", 0, false
	}
	e := el.Value.(*entry)
	if now().Sub(e.createdAt) > t.opt.TTL {
		t.removeLocked(el)
		t.misses.Add(1)
		return "", 0, false
	}
	t.order.MoveToFront(el)
	t.hits.Add(1)
	return e.backend, e.contention, true
}

// Put 粘性写入。**只在请求完全成功且内容已交付之后调用**——这个时机约束
// 由接线方（service/proxy 的成功出口）保证：任何一次 429/失败都不该把
// 会话钉到没接住它的 backend 上。
//
// 已存在的 key：更新 backend（通常是同一个）、刷新时间戳、清零滞回计数、
// 移到 LRU 队头。不存在时新增；容量满则挤出最旧条目（计 evictions）。
func (t *Table) Put(key, backend string) {
	if key == "" || backend == "" {
		return
	}
	t.puts.Add(1) // 观测口径与 SLR 对齐：每次真实 Put 都计（含同 backend 刷新）
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, has := t.items[key]; has {
		e := el.Value.(*entry)
		e.backend = backend
		e.createdAt = now()
		e.contention = 0
		t.order.MoveToFront(el)
		return
	}
	t.items[key] = t.order.PushFront(&entry{key: key, backend: backend, createdAt: now()})
	for len(t.items) > t.opt.Max {
		oldest := t.order.Back()
		if oldest == nil {
			break
		}
		t.removeLocked(oldest)
	}
}

// Forget 显式失效（逐出）。key 不存在时不做任何事、计数也不动——
// 只有真正拆掉一个钉才叫一次 eviction。（接线层的场景：热重载后 backend
// 名字已消失；或 errclass.ReleaseAffinity 判定要放弃。）
func (t *Table) Forget(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if el, has := t.items[key]; has {
		t.removeLocked(el)
	}
}

// NoteContention 滞回计数：钉住的后端满载、TryAcquire 挤不进去时调用。
// **不逐出**——满载是排队不是降级，60s 排队不该被当成 backend fault；
// 只有连续 ContentionLimit 次都要挤才迁移（拆钉 + 计一次 eviction），
// 下一次请求走 SWRR 重新首落、成功后由接线层 Put 到新地方。
func (t *Table) NoteContention(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	el, has := t.items[key]
	if !has {
		return
	}
	e := el.Value.(*entry)
	e.contention++
	if e.contention >= t.opt.ContentionLimit {
		t.removeLocked(el)
	}
}

// Relinquish 「判定 + 逐出」一步走：用逐出策略（默认 errclass.ReleaseAffinity，
// 与冷却/熔断/兜底同源）判定该类失败是否应放弃粘性；应放弃就立刻拆钉。
// 返回是否放弃（供接线层打日志）。
//
// 说明：mid-stream / 客户端取消在策略函数里就归「不动」，接线层还在外面
// 加了 !res.wrote 双保险——响应已经开始就根本没切换 backend，谈不上拆。
func (t *Table) Relinquish(key string, c errclass.Class) bool {
	if !t.opt.Evict(c) {
		return false
	}
	t.Forget(key)
	return true
}

// CountPerBackend 按 backend 统计当前钉住多少个会话（看板 sticky_count）。
// 读时顺带把已过期的条目清掉——观测口不该报"表里还有 3000 个没人再用的会话"。
func (t *Table) CountPerBackend() map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]int, len(t.items))
	for el := t.order.Front(); el != nil; {
		next := el.Next()
		e := el.Value.(*entry)
		if now().Sub(e.createdAt) > t.opt.TTL {
			t.removeLocked(el)
		} else {
			out[e.backend]++
		}
		el = next
	}
	return out
}

// Len 当前条目数（含未过期的全部条目；观测/测试用）。
func (t *Table) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.items)
}

// Stats 计数快照（/api/stats 全局计数的数据源）。
// sticky_hits / sticky_misses / sticky_evictions 是本网关看板的历史键名；
// hits/misses/puts 与 SLR 的 affinity 对象同源同义（迁移期两套键并存，
// 消费方逐步收拢到 affinity{} 一套）。标准库 atomic，无需第二把锁。
type Stats struct {
	Hits      uint64
	Misses    uint64
	Evictions uint64
	Puts      uint64
}

func (t *Table) Stats() Stats {
	return Stats{
		Hits:      t.hits.Load(),
		Misses:    t.misses.Load(),
		Evictions: t.evictions.Load(),
		Puts:      t.puts.Load(),
	}
}

// removeLocked 删除一个条目并计一次 eviction。调用方必须已持 t.mu。
func (t *Table) removeLocked(el *list.Element) {
	if el == nil {
		return
	}
	e := el.Value.(*entry)
	t.order.Remove(el)
	delete(t.items, e.key)
	t.evictions.Add(1)
}
