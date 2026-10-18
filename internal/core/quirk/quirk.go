// Package quirk 是「只在某个上游出现的问题」的落点。
//
// 内核只在请求生命周期的三个固定缝上调它；默认实现 = OpenAI 兼容族的通用行为，
// 所以没怪癖的上游一个字都不用写，行为与不接怪癖层逐位一致。
//
// ── 为什么要有这一层 ────────────────────────────────────────────
// 内核此刻 0 处按 provider 名的分支 —— 历史上踩过的坑都被「泛化」成了通用规则
// （无条件删 Content-Length、只改 model 一个键、session_header 字段、通用词表），
// 这四条泛化得对。但下一个坑如果不具备可泛化性呢？那时只有两条路：硬揉成通用
// 规则（可能伤别家）、或加 if name==xxx 分支（把名字判定撒进内核）。
// 本包是第三条路：**按家差异写进 quirk/xxx.go，内核只认接口**。
//
// ── 三个缝 ────────────────────────────────────────────────────
//
//	缝① Decorate   出口前（通用 header 清理之后、发送之前）：加该家专属头/query、按条件裁 body
//	缝② Classify   分类时：status+body → errclass.Class；返回 false = 不归我管，走通用判定
//	缝③ ParseUsage 用量时：body → Usage；OK=false = 不归我管，走 OpenAI 默认
//
// ── 依赖方向铁律 ──────────────────────────────────────────────
// 本包只许 import internal/core/errclass + internal/core/upstream + 标准库。
// 不许 import service/* / config / api —— 它必须站在 service 之下，
// 否则「内核依赖怪癖」会变成「怪癖依赖内核」的环。
//
// ── 词表归属 ──────────────────────────────────────────────────
// 全局默认词汇（模型下架词表、403 欠费词表）继续住在 errclass —— 那是纯策略层。
// 本包只提供「某家可以覆盖」的缝，不复制、不搬家任何既有词表。
package quirk

import (
	"net/http"
	"sync"

	"shanhaigateway/internal/core/errclass"
	"shanhaigateway/internal/core/upstream"
)

// Usage 是从响应体解析出的用量（缝③的返回）。
type Usage struct {
	Prompt       int
	Completion   int
	Total        int
	CachedPrompt int  // 缓存命中的输入 token
	OK           bool // false = 未解析出，调用方走 OpenAI 默认
}

// Quirk 封装一家上游/一族协议的专属知识。内核只依赖本接口，不 import 任何具体实现。
type Quirk interface {
	// Name 是怪癖的名字 = 注册表的 key，也约定为该后端 backends.json 里的 "driver" 值。
	Name() string

	// Decorate 缝①出口前：通用 header 清理之后调用。可加该家专属的头/query、按条件裁 body。
	// 默认 no-op。
	Decorate(req *http.Request, b *upstream.Backend)

	// Classify 缝②分类时：status+body → errclass.Class。
	// 返回 (Class, false) 表示「这条不归我管」，调用方走通用默认判定。
	// 关键：false 让一家只认领自己那几句文案，不抢通用判定。
	Classify(status int, body string) (errclass.Class, bool)

	// ParseUsage 缝③用量时：body → Usage。OK=false = 不归我管，走 OpenAI 默认。
	ParseUsage(body []byte) Usage
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Quirk{}

	// defaultQuirk 是「未指定 driver」与「driver 没注册」的落点。
	// 包级单例（不是每次 new）：Get 直接返回它，零分配。
	// 值 = OpenAI 兼容族的通用行为，即不接怪癖层时的今天行为。
	defaultQuirk Quirk = OpenAI{}
)

// Register 注册一家怪癖。重复注册同一 Name 时 panic（防两家抢一个名字）。
//
// 放 init() 里调（见 openai.go 的说明）：注册是一次性的装配动作，
// 发生在任何请求之前，所以这里不需要容忍「运行期改注册表」以外的时序。
func Register(q Quirk) {
	if q == nil {
		panic("quirk: Register(nil)")
	}
	name := q.Name()
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("quirk: duplicate Register for name " + name)
	}
	registry[name] = q
}

// Get 返回 name 对应的怪癖；name=="" 或未注册 → 返回包级 defaultQuirk（OpenAI 兼容通用行为）。
// 不每次分配，复用包级单例。
//
// 未注册名不 panic：driver 是配置字段，写错一个字母不该让网关起不来 ——
// 退化成通用行为（与不配 driver 逐位一致）才是安全的失败方式。
func Get(name string) Quirk {
	if name == "" {
		return defaultQuirk
	}
	registryMu.RLock()
	q, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return defaultQuirk
	}
	return q
}
