// public.go —— 公网口的门。
//
// 包装与判定顺序（publicGate，由外到内的**决策树**）：
//
//	请求进来
//	 ├─ path 以 /v1/ 开头，且 Bearer key 是有效下游 key（keys.json /
//	 │    sk-user-*，经 apikey.KeyStore.Auth）→ **直接进业务 mux**，
//	 │    绕开 dashGate；响应写完扣一次额度（见下行注释）。
//	 │    这就是「sk-user key 双通道」里的另一半：OpenAI SDK 用户
//	 │    不登录也能用 key 调 LLM。
//	 └─ 其余一切 → dashGate（dashgate.go 原文）：/login /logout 自治，
//	      其余全路径（含 /v1 /admin）要求 session cookie 或 DASH_TOKEN。
//
// 结果矩阵（对照验收口径）：
//
//	未带凭据 /v1            → dashGate 401 JSON
//	Bearer <DASH_TOKEN>     → dashGate 放行 → 代理层执行（DASH_TOKEN 是管理侧
//	                           key：不撞下游库、也不该被记一次用量）
//	Bearer <sk-user-*>、<keys.json key> → key 通道直达代理并扣额度
//	浏览器无 cookie 开 /    → 302 /login
//	伪造的 sk-user-*        → key 通道查无此 key → 落回 dashGate → 401
//
// per 请求 ReloadIfChanged（keys.json mtime 热加载），与旧 proxyHandler
// 的调用位置一致。
//
// ⚠️ 额度扣减决策（**与旧实现的分歧，如实记档**）：旧实现（公版
// proxyHandler）是「上游成功才扣」；本层包在代理外面，看不到上游成败，
// 所以是「响应写完就扣」近似。不把 Consume 挪进 proxy.Handler 的理由：
// proxy 的依赖纪律明确不许 import service/ 其它包，不为了一个计数点破
// 那条纪律。将来要精确，等成功信号在 api 层可见（如代理回包标记）再挪。
package api

import (
	"net/http"
	"strings"
)

// publicGate 组装公网口的包装链。
func publicGate(h *Handlers, mux http.Handler) http.Handler {
	gated := dashGate(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 下游 key 通道（只对 /v1/* 生效）
		if strings.HasPrefix(r.URL.Path, "/v1/") && h.Keys != nil {
			// keys.json mtime 热加载（每 /v1 请求一次 stat，与旧调用点一致）
			h.Keys.ReloadIfChanged()
			if key := dashBearer(r); key != "" {
				if _, ok := h.Keys.Auth(key); ok {
					// 有效下游 key：直接进业务 mux（绕开登录门）
					mux.ServeHTTP(w, r)
					// 响应写完扣一次（分歧决策见文件头注释）
					h.Keys.Consume(key)
					return
				}
				// 不是有效下游 key：落回 dashGate —— 万一是 DASH_TOKEN
				// 或有效 session 的其它头，不该在这里被拦死。
			}
		}
		gated.ServeHTTP(w, r)
	})
}
