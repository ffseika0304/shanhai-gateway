package errclass

// ReleaseAffinity 的表驱动钉死（2026-10-11 会话亲和批次新增）：
// 逐出/保持的判定与 CountsAsFault 等策略函数共用同一份分类来源，
// 这个测试钉的就是「哪些类拆钉、哪些类不动」——affinity 包不再另造一套。

import "testing"

func TestReleaseAffinity(t *testing.T) {
	keep := map[Class]string{
		// 客户端的锅：别迁怒 backend
		BadRequest400: "client fault",
		// 瞬时问题：60s 冷却自己会回来，逐掉 locality 是损失
		Network: "transient", Timeout: "transient",
		// mid-stream：响应已开始，根本没切换 backend，谈不上拆
		MidStreamFail: "response already started",
		// 空气泡：请求侧原因，与 SoftFailure 同源（后端本来是健康的）
		NoContent: "soft", EmptyOutput: "soft",
		// skip_*：路由决策不是失败（满载走滞回 contention，不走这里）
		SkipCooldown: "routing decision", SkipCapacity: "routing decision",
		SkipCircuit: "routing decision", SkipDisabled: "routing decision",
	}
	// ClientGone / Cancelled 在类目上落 Network（attempt 对 clientGone
	// 只斩冷却不斩亲和），与 Network 一起留在「不动」侧。
	for c, why := range keep {
		if ReleaseAffinity(c) {
			t.Errorf("%s (%s) must NOT release the sticky", c, why)
		}
	}
	drop := map[Class]string{
		// ImmediateCooldown 类：配额打满，钉在限流后端=钉在坏地方
		RateLimit429: "quota exhausted",
		// 其余后端侧的错：5xx / 408/504 / 401 / 403 / 404 / 模型下架 / 200+error_body / 首字超时
		ServerError5xx: "backend fault", UpstreamTimeout: "backend fault",
		Auth401: "backend fault", Forbidden403: "backend fault",
		NotFound404: "backend fault", ModelUnavailable: "backend fault",
		ErrorBody: "backend fault", FirstTokenTimeout: "backend fault",
	}
	for c, why := range drop {
		if !ReleaseAffinity(c) {
			t.Errorf("%s (%s) must release the sticky", c, why)
		}
	}
	// 兜底口径：不在名单里的未知类按 SSE 最后一行处理 ——
	// ClassifyStatus 的 default 是"说不清就按最坏情况"，这里同向：说不清
	// 的失败不该把会话钉死在（可能已坏）的原 backend 上。
	if ReleaseAffinity(Class("brand_new_unknown_failure")) != true {
		t.Error("unknown classes fall into the worst-case side (release)")
	}
}
