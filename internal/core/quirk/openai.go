package quirk

import (
	"encoding/json"
	"net/http"

	"shanhaigateway/internal/core/errclass"
	"shanhaigateway/internal/core/upstream"
)

// OpenAI 是 OpenAI 兼容族的基准实现，也是「未指定 driver」时的默认。
// 它的三个方法 = 今天的通用行为，不引入任何新逻辑。
//
// 为什么要有一个「什么都不做」的默认实现：内核可以无条件地在三个缝上查表，
// 不必先判「这家有没有配 driver」。没有怪癖的上游拿到的就是这个值，
// 每一格行为都与不接怪癖层时逐位一致（现有测试一条不改、原样全绿）。
//
// 加一家新怪癖的正确姿势（而不是在内核里加名字分支）：
//
//	// internal/core/quirk/xxx.go
//	type Xxx struct{ OpenAI }          // 嵌基准实现，只覆盖有差异的方法
//	func (Xxx) Name() string { return "xxx" }
//	func init() { Register(Xxx{}) }
//
// 然后在 backends.json 的那条后端上加 "driver":"xxx"。内核零改动。
type OpenAI struct{}

// Name 是基准实现的 driver 名（也即 backends.json 里 driver 字段的缺省语义）。
func (OpenAI) Name() string { return "openai-compat" }

// Decorate 缝①：基准实现 no-op —— 通用 header 清理已经处理了兼容族的全部出口知识
// （删 Content-Length/Accept-Encoding/客户端凭据、固定 Content-Type/UA）。
func (OpenAI) Decorate(req *http.Request, b *upstream.Backend) {}

// Classify 缝②：只认领「模型不存在/已下架」这一句，且只在 400/404 上认领。
//
// 词表仍住在 errclass（全局默认词汇），本方法只是去调它；
// quirk 只加「按家覆盖」的缝，不复制词表。
// 返回 false = 走通用默认判定（调用方那一侧的既有逻辑，逐位不变）。
func (OpenAI) Classify(status int, body string) (errclass.Class, bool) {
	if status == 400 || status == 404 {
		if errclass.ModelUnavailableBody(body) {
			return errclass.ModelUnavailable, true
		}
	}
	return "", false
}

// ParseUsage 缝③：OpenAI 兼容 usage（含 prompt_tokens_details.cached_tokens）。
//
// 逻辑与 service/proxy 的 parseUsage 逐位一致（那边的实现与测试原样保留，
// 本方法是它在 core 层的一份副本 —— 依赖方向不允许 quirk 去 import proxy，
// 也不允许反过来把 proxy 的私有函数抬成导出）。
// OK=false 表示没解析出 usage：调用方回退到原有的 parseUsage，行为不变。
func (OpenAI) ParseUsage(body []byte) Usage {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return Usage{}
	}
	u, ok := obj["usage"].(map[string]any)
	if !ok {
		return Usage{}
	}
	asInt := func(v any) int {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case json.Number:
			i, _ := n.Int64()
			return int(i)
		}
		return 0
	}
	p := asInt(u["prompt_tokens"])
	c := asInt(u["completion_tokens"])
	t := asInt(u["total_tokens"])
	if t == 0 {
		t = p + c
	}
	cached := 0
	if d, ok := u["prompt_tokens_details"].(map[string]any); ok {
		cached = asInt(d["cached_tokens"])
	}
	return Usage{Prompt: p, Completion: c, Total: t, CachedPrompt: cached, OK: true}
}
