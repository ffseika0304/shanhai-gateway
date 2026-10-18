// Package upstream 管的是「后端本身」：它是谁、能接多少并发、现在健康不健康、
// 历史上快不快、HTTP 连接池怎么复用。
//
// 从 seika_llm_router 调度内核原样搬入。挑的是调度内核里最纯粹的一块：
// **零 IO、零外部依赖**（只依赖标准库 + internal/core/errclass），
// 所以能被表驱动测试逐条钉死，不需要起服务、不需要打桩网络。
//
// ── 分层约定 ────────────────────────────────────────────────
// 本包是 core 层，依赖方向是 core → services → api 单向。
// 允许 import internal/runtime，但**只用它的 env 读取器**
// （runtime.EnvDuration / EnvFloat）—— 不取路径、不碰日志、不碰数据库。
// 2026-10-11 之前 durEnv/numEnv 在本包和 service/routing 各抄了一份，
// 两处逐字相同却可以各自漂移；现在只有一个来源。runtime 不 import
// 任何项目内包，所以这条依赖不会成环。
// 核心逻辑的可测试性正来自「不碰运行时设施」—— env 是唯一例外，
// 而且它只在包级 var 初始化时读一次，不在测试里生效（测试要改就改包内变量）。
//
// ── 搬入时保留了哪些不能碰的东西 ──────────────────────────────
// 这里的每一个魔法数字都是生产事故调出来的，**不是随手写的**：
//   - 并发位上限 / 60 秒排队：满了不等于降级（见 slot.go）
//   - 配额类指数退避 5m→10m→…→4h 封顶：10-01 的 403 自锁风暴
//   - failsToTrip=5 / slowFailsToOpen=2：熔断与慢探测的触发门槛
//   - clientGone 时跳过冷却：10-03 一次断连让全网 503 一分钟（1720 次）
//   - 模型下架识别：10-06 起 bunny 两条 0 成功率空转 4 天半
//
// 改动前请先读这些注释，它们记的是事故，不是偏好。
package upstream

import (
	"net/http"
	"sync/atomic"
	"time"

	"shanhai-gateway/internal/core/errclass"
)

// Class 是错误分类。搬入前这一层直接用包内的 errClass；现在错误分类归 errclass 包
// 所有，这里只做**类型别名**（不是新定义）——两者是同一个类型，可以直接互换，
// 调用点不必大面积改。
type Class = errclass.Class

// 与上游状态机相关的几个分类在本地留一份短名，搬入前的调用点原样可用。
const (
	clsNetwork   = errclass.Network
	clsTimeout   = errclass.Timeout
	clsFirstTok  = errclass.FirstTokenTimeout
	cls429       = errclass.RateLimit429
	cls401       = errclass.Auth401
	cls403       = errclass.Forbidden403
	cls400       = errclass.BadRequest400
	cls404       = errclass.NotFound404
	cls408       = errclass.UpstreamTimeout
	cls5xx       = errclass.ServerError5xx
	clsErrBody   = errclass.ErrorBody
	clsNoContent = errclass.NoContent
	clsEmptyOut  = errclass.EmptyOutput
	clsMidStream = errclass.MidStreamFail
	clsModelGone = errclass.ModelUnavailable

	clsSkipCool    = errclass.SkipCooldown
	clsSkipCap     = errclass.SkipCapacity
	clsSkipCircuit = errclass.SkipCircuit
	clsSkipDisable = errclass.SkipDisabled
)

// classList 供统计层预分配每类的零值计数器。
var classList = errclass.List

// Account 账号：key 池的拥有者。同一账号的多个 key 汇聚成一个共享池，
// 该账号下**所有** backend 的请求都从这个池轮换取 key（账号级共享池）。
// 这样「加一个账号」只需在 accounts 段加一组 key，引用它的 backend 自动生效。
type Account struct {
	keySeq int64 // 必须放首位：保证 64 位对齐，供 atomic 使用

	Name    string   `json:"name"`
	BaseURL string   `json:"base_url"`
	APIKeys []string `json:"api_keys"`
}

// Pricing 单价（每 1M tokens，美元）。缺省全 0 = 不参与金额估算。
// 用同一套算法对所有后端算钱：cost = 非缓存in*Input + 缓存in*CachedRead + out*Output
type Pricing struct {
	Input       float64 `json:"input"`
	Output      float64 `json:"output"`
	CachedRead  float64 `json:"cached_read"`
	CachedWrite float64 `json:"cached_write"`
}

// Backend 一个上游后端：身份、容量（并发位）、健康状态机、统计都在这里。
type Backend struct {
	Name          string   `json:"name"`
	Account       string   `json:"account"` // 所属账号名；空则该 backend 自成一个匿名账号
	Model         string   `json:"model"`
	ModelExpire   string   `json:"model_expire"`   // 模型过期时间（RFC3339），过期后使用ModelFallback
	ModelFallback string   `json:"model_fallback"` // 过期后使用的模型
	BaseURL       string   `json:"base_url"`
	APIKeys       []string `json:"api_keys"` // 兼容旧配置：backend 内联 key（并入所属/匿名账号）
	Timeout       float64  `json:"timeout"`
	MaxToken      int      `json:"max_tokens"`
	Concurrency   int      `json:"concurrency"`
	Weight        float64  `json:"weight"`
	Enabled       bool     `json:"enabled"`
	IsFallback    bool     `json:"is_fallback"`
	IsPrimary     bool     `json:"is_primary"`     // 主轮转后端（配置驱动，替代硬编码列表）
	SessionHeader string   `json:"session_header"` // 该上游要求的会话头名（如 x-opencode-session）
	Driver        string   `json:"driver"`         // 怪癖层/协议族名，默认 "openai-compat"；未填=通用
	Pricing       *Pricing `json:"pricing"`        // 单价（每1M tokens，USD），用于金额估算

	// 手动打标签：到期前不载入（与自动黑名单走同一个"不载入"判定）。
	// 兜底的剔除是自动的（探测永久失效→黑名单）；首发轮转靠这个手动标签。
	// 空值=不暂停；到点自动恢复载入，无需手工摘标签。
	DisabledUntil  string `json:"disabled_until"`  // RFC3339，如 2026-09-21T00:10:00+08:00
	DisabledReason string `json:"disabled_reason"` // 打标签的原因说明

	acct *Account // 运行期绑定的账号（key 池来源）

	cbTripped  int64 // 跳闸时间戳(unix秒)
	failStreak int64
	curConc    int64 // 在飞请求数（并发占用）
	waiters    int64 // 排队等并发位的请求数

	freeCh chan struct{} // 释放通知：有并发位空出来时推一个 token，唤醒排队者

	cooldownUntil  int64
	cooldownReason Class
	cooldownStrike int64 // 配额类冷却的连续次数（指数退避用；成功一次归零）
	slowUntil      int64
	slowStreak     int64

	stats  *BackendStats
	client *http.Client // 持久化HTTP客户端（见 client.go）
}

// BoundAccount 返回该后端绑定的账号（key 池来源）。
func (b *Backend) BoundAccount() *Account { return b.acct }

// New 构造一个 Backend 并补全成可运行态。
//
// accounts 是「账号名 → 账号」的索引。后端引用了不存在的账号时退化成内联 key
// —— 这与搬入前的 loadBackends 行为一致：坏配置不该让整个网关起不来。
func New(name, model, baseURL string, accounts map[string]*Account) *Backend {
	b := &Backend{Name: name, Model: model, BaseURL: baseURL}
	b.Init(accounts)
	return b
}

// Init 把配置态的后端补全成可运行态。与搬入前 loadBackends 的后处理逐条对应。
//
// 为什么保留这个入口（而不是让 config 包自己做这些后处理）：这六步——补默认
// 并发位、补默认权重、绑定账号、账号提供默认 base_url、内联 key 并入账号池、
// 建统计与并发位缓冲——是一组**必须一起执行、不许漏一步**的补全动作，散在
// 别的包里迟早会漏掉某一步（漏了 freeCh 就 nil，漏了 stats 就 panic 在第一请求）。
// 归一处在这里，后补全的人只需要调一次 Init。
func (b *Backend) Init(accounts map[string]*Account) {
	if b.Concurrency <= 0 {
		b.Concurrency = 16 // 搬入前的默认并发位，照旧
	}
	if b.Weight == 0 {
		b.Weight = 1.0 // 搬入前的默认权重，照旧
	}
	// 绑定账号：同账号共享 key 池；未引用或引用不存在则自建匿名账号（兼容旧配置）
	if b.Account != "" {
		if a := accounts[b.Account]; a != nil {
			b.acct = a
		}
	}
	if b.acct == nil {
		b.acct = &Account{Name: b.Name, BaseURL: b.BaseURL}
	}
	// 账号提供默认 base_url，并作为 key 池来源（backend 内联 key 并入）
	if b.BaseURL == "" {
		b.BaseURL = b.acct.BaseURL
	}
	if len(b.acct.APIKeys) == 0 {
		b.acct.APIKeys = b.APIKeys
	}
	b.stats = newBackendStats()
	b.freeCh = make(chan struct{}, freeChBuf)
}

// freeChBuf 是 freeCh 的缓冲大小（搬入前写死 256，照旧）。
// 它决定了「一次 release 最多唤醒几个排队者」的上限：非阻塞发送，塞满就丢 token。
// 丢 token 是安全的 —— 排队者被唤醒后还要 tryAcquire，抢不到就继续等下一次通知。
const freeChBuf = 256

// KeySeqNext 轮转取号：账号级共享池的游标。搬入前的用法是
//
//	rot := int(atomic.AddInt64(&b.acct.keySeq, 1)) - 1
//	keyIdx := (rot + off) % nKeys
func (a *Account) KeySeqNext() int {
	return int(atomic.AddInt64(&a.keySeq, 1)) - 1
}

// EffectiveModel 返回当前有效的模型（根据时间条件判断）
func (b *Backend) EffectiveModel(now time.Time) string {
	if b.ModelExpire == "" || b.ModelFallback == "" {
		return b.Model
	}
	expire, err := time.Parse(time.RFC3339, b.ModelExpire)
	if err != nil {
		return b.Model
	}
	if now.After(expire) {
		return b.ModelFallback
	}
	return b.Model
}
