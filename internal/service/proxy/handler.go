package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"shanhaigateway/internal/core/errclass"
	"shanhaigateway/internal/core/store"
	"shanhaigateway/internal/runtime"
	"shanhaigateway/internal/service/routing"
)

// 首发轮转开关（ROTATE_START=0 可退回"严格按优先级"）。
// 原来在 main.go 的包级 var，随 proxyHandler 一起搬进来；
// 不进 Options：它不是每次部署都调的参数，与 queueWait/UA 同级，照旧读 env。
var rotateStart = runtime.EnvString("ROTATE_START", "1") == "1"

// uuidHex —— rid / session 生成器，随 proxyHandler 一起搬进来（原样）。
func uuidHex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}

// logAttempt 写 stderr 那行人类可读的尝试日志。
//
// 边界（2026-10-11 上一批定好）：store 只负责 attempts 的落库与环形缓冲
// （store.Emit），不决定日志观感 —— 这行 stderr 格式留在本层，注释钉住。
func logAttempt(ok bool, rid, backend string, keyIdx, seq, status int, latMs, ftMs int64, errStr string) {
	if errStr == "" {
		errStr = "-"
	}
	log.Printf("[attempt] rid=%s backend=%s key=%d seq=%d status=%d lat_ms=%d first_token_ms=%d %s",
		rid, backend, keyIdx, seq, status, latMs, ftMs, errStr)
}

type failInfo struct {
	Backend string `json:"backend"`
	KeyIdx  int    `json:"key_idx"`
	Class   string `json:"class"`
	Status  int    `json:"status"`
	Msg     string `json:"msg"`
}

func summarizeFails(f []failInfo) string {
	parts := []string{}
	for _, x := range f {
		parts = append(parts, fmt.Sprintf("%s/%s/%d", x.Backend, x.Class, x.Status))
	}
	return strings.Join(parts, " ")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// allCooldown 判断：是否所有候选都因为"上游限流冷却"被跳过
func allCooldown(f []failInfo) bool {
	if len(f) == 0 {
		return false
	}
	for _, x := range f {
		if errClass(x.Class) != clsSkipCool {
			return false
		}
	}
	return true
}

// allSkipped 判断：是否所有候选都只是被跳过（禁用/冷却/熔断/满并发）而非真失败
func allSkipped(f []failInfo) bool {
	if len(f) == 0 {
		return false
	}
	for _, x := range f {
		if !errClass(x.Class).IsSkip() {
			return false
		}
	}
	return true
}

// Handler 是 /v1/chat/completions 的核心（原 proxyHandler）。
//
// 搬入前是 Router 的方法闭包；搬入后依赖显式注入：rt（路由决策）、
// st（attempts 记账，store.Emit 即原 emitAttempt）、opt（配置值）。
// 先谈两个前提：本 handler 不做鉴权、不读 config —— 接口层（api/auth/apikey）
// 校验 key，本层只管「把请求发到哪个后端、怎么失败收场」。
func Handler(rt *routing.Router, st *store.Store, opt Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		now := start
		rid := uuidHex()[:16]

		// 识别重试请求：如果下游传了X-Router-Rid，说明是重试
		retryRid := r.Header.Get("X-Router-Rid")
		isRetry := retryRid != ""

		body, _ := io.ReadAll(r.Body)
		// `why` 这里曾有 `strings.ReplaceAll(body, "\"auto\"", DEFAULT_MODEL)` 的"auto → 默认模型"
		//      改写，已删除：它盲替换 body 里**每一个** `"auto"`，除了 model 还会打坏
		//      tool_choice / reasoning_effort 等值为 "auto" 的字段，甚至连 prompt 正文里的
		//      字面量 "auto" 一起改。实测把 `"tool_choice":"auto"` 改成 `"tool_choice":"<模型名>"`，
		//      被上游 400 拒（volc: expected a string or an object, but got "xxx"）。
		//      正确的做法是按 JSON 结构只改 model 一个键 —— 见下方按后端覆写处。
		streaming := false
		var pmap map[string]any
		if json.Unmarshal(body, &pmap) == nil {
			if s, ok := pmap["stream"].(bool); ok {
				streaming = s
			}
		}
		modelUsed, _ := pmap["model"].(string)
		// 会话标识：下游传 X-Session-Key（如 splicer/test-ai/delivery）即用其值，
		// 供上游做 prompt 缓存（同一会话/前缀命中缓存，cached read 便宜 50x）。
		// 没传则随机生成（退化为无缓存，行为与以前一致）。
		session := strings.TrimSpace(r.Header.Get("X-Session-Key"))
		// 亲和键：优先 X-Job-Id（任务级，粒度最细 —— 同一 RQ 任务的所有轮次
		// 共用一个 id，正是 provider 前缀缓存的复用单元），退回 X-Session-Key。
		// 两者都没传才留空：随机会话每请求都不同，写进亲和表只会白白挤占
		// LRU 容量，还把观测搅成噪音（照旧不收）。
		// `why`（2026-10-18 缓存调查）：shadow 侧命中 34.7% 里链式命中占大头，
		// 同一前缀簇在 ds41 系后端 51~56% ≈ 其池内抽取概率——散装轮转就是
		// 命中天花板；键从「会话（没人发）」换成「job（SCF 即将发）」才接得上。
		sessKey := cleanJobID(r.Header.Get("X-Job-Id"))
		if sessKey == "" {
			sessKey = session
		}
		if session == "" {
			session = uuidHex()
		}

		fails := []failInfo{}
		seq := 0

		// ① 先剔除真正不可用的（禁用/冷却/熔断）——这些不做首发分配。
		//    注意：并发位满**不算**不可用，满了是排队，不是降级。
		usable := []string{}
		for _, name := range rt.Candidates() {
			b := rt.ByName(name)
			if b == nil {
				continue
			}
			if reason := b.SkipReason(now); reason != "" {
				b.Stats().Bump(reason)
				fails = append(fails, failInfo{Backend: b.Name, Class: string(reason)})
				continue
			}
			usable = append(usable, name)
		}

		// ② 一个任务按顺序领一个首发上游：全局轮转起点，负载天然分摊，
		//    而不是所有任务都挤到队头。ROTATE_START=0 可退回"严格按优先级"。
		//    重试请求：跳过之前失败的后端，轮训换上游

		// ②' 会话亲和（2026-10-11）：已建立粘性的会话**优先钉回**同一 backend，
		//    救回 provider 侧的前缀缓存（同对话第二轮落到别家，缓存就不在
		//    同一个地——10-10 生产实测命中率 0% 的根因）。定位是「套在轮转
		//    外的优先选择」，不是替代轮转：
		//     - 新会话（表里没有钉）→ 完全照旧走 SWRR 首发轮转；
		//     - 命中且目标可用（目标在 usable 里、TryAcquire 成功占到位）→
		//       把它排到 order[0]，并跳过下方 SWRRPick（pinned != "" 短路）；
		//     - 命中但满载 → **不改 order**（照走现有排队/兜底链，原本就该
		//       排队），表保持在原 backend，只在亲和表里滞回计数 +1，
		//       连续 AFFINITY_CONTENTION_LIMIT 次才迁移（affinity 包负责）；
		//     - 命中但目标被健康层过滤（冷却/熔断/禁用）→ 本次不钉，也不拆
		//       ——等它自己冷却回来还能接着命中局部性；目标 backend 名已
		//       消失（热重载换名，AtomicRouter 换装）→ 拆钉，跟着新拓扑走。
		//    mid-stream / 客户端取消不动亲和（响应已开始 / 不是 backend 的错）；
		//    429 才逐出——判定走 errclass.ReleaseAffinity，见失败路径的调用点。
		pinned := ""
		affinityHeld := false // 选序点已为钉住的后端占好并发位（循环里不再重复抢）
		if opt.Affinity != nil && sessKey != "" && !isRetry {
			if p, _, ok := opt.Affinity.Get(sessKey); ok {
				switch pb := rt.ByName(p); {
				case pb == nil:
					// 钉的是个死名字：局部性已失效 → 拆钉。
					opt.Affinity.Forget(sessKey)
				case slices.Contains(usable, p):
					if pb.TryAcquire() {
						// 命中：钉住。占到位才算数——真正的获取/排队/兜底
						// 仍由下方既有路径完成（这里提前占的那一位与循环
						// 末尾既有的 Release 严格一对一，不多占不漏占）。
						pinned = p
						affinityHeld = true
					} else {
						// 满载：不动 order、不逐出，只做滞回计数。
						opt.Affinity.NoteContention(sessKey)
					}
				}
			}
		}

		order := usable
		if pinned != "" {
			// 钉住的后端排到候选序首位，其余候选顺序原样保留
			for i, n := range usable {
				if n == pinned {
					order = append(append([]string{pinned}, usable[:i]...), usable[i+1:]...)
					break
				}
			}
		}
		if isRetry && len(usable) > 1 {
			// 重试请求：跳过之前失败的后端，从下一个后端开始轮训
			//（亲和不对重试请求生效——重试语义就是换后端；成功后的写表
			// 会按实际接应它的后端把粘性迁过去）
			if prevBackend := r.Header.Get("X-Router-Failed-Backend"); prevBackend != "" {
				for i, name := range order {
					if name == prevBackend {
						// 从下一个后端开始轮训
						order = append(order[i+1:], order[:i+1]...)
						break
					}
				}
			}
		} else if pinned == "" && len(usable) > 1 && rotateStart {
			// 正常请求：加权首发轮转（weight 全 1.0 且候选集稳定时，
			// 与旧 rrSeq%N 严格轮转逐字等价——零回归；低权重者按比例少领回合）
			// 亲和命中时跳过（pinned != "" 短路，不进本分支）：钉住的会话
			// 不再每请求消耗 SWRR 回合，轮转的权重分摊不被亲和流量扰动。
			pick := rt.SWRRPick(usable)
			for i, n := range usable {
				if n == pick {
					order = append(append([]string{}, usable[i:]...), usable[:i]...)
					break
				}
			}
		}

		for _, name := range order {
			b := rt.ByName(name)
			if b == nil {
				continue
			}
			// 检查客户端是否已取消请求
			select {
			case <-r.Context().Done():
				// 下游取消日志：标记为下游问题
				//（亲和选序点占了位还没进 picking 分支时，取消会从这里
				// 直接返回——把占下的位放回去，别让它漏掉）
				if affinityHeld {
					b.Release()
					affinityHeld = false
				}
				log.Printf("[downstream-error] rid=%s client cancelled, stopping failover", rid)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Router-Rid", rid)
				w.WriteHeader(499) // 自定义状态码：客户端取消
				json.NewEncoder(w).Encode(map[string]any{
					"error":  "client cancelled request",
					"rid":    rid,
					"source": "downstream",
				})
				return
			default:
			}
			// ③ 优先分散到兜底，其次再排队
			//    如果首发轮转后端满了，先尝试分散到兜底后端
			//    如果兜底后端也满了，再排队等待
			if affinityHeld {
				// 亲和选序点已为该后端占好唯一的并发位：这里不再重复抢。
				// 错过后要进 else 分支就什么都变了——那个占位必须与下方
				// 既有路径的 Release（成功分支 / keyLoop 后的归还）严格
				// 一对一。affinityHeld 只对应 order[0]（亲和钉住的那位），
				// 第一轮必命中。
				affinityHeld = false
			} else if b.TryAcquire() {
				// 成功获取并发位，继续处理
			} else {
				// 首发轮转后端满了，按轮转起点逐个试**全部**可用兜底（批次3：原来只给 1 个候选）
				分散成功 := false
				for _, fallbackName := range rt.GetFallbackCandidates() {
					fb := rt.ByName(fallbackName)
					if fb == nil || fb.SkipReason(now) != "" {
						continue
					}
					if fb.TryAcquire() {
						// 成功分散到兜底后端
						b = fb
						分散成功 = true
						log.Printf("[proxy] rid=%s分散到兜底 %s (原 %s 满载)", rid, fb.Name, name)
						break
					}
				}
				if !分散成功 {
					// 兜底后端也满了，排队等待
					if !b.AcquireWait(r.Context(), opt.QueueWait) {
						b.Stats().Bump(clsSkipCap)
						st.Emit(store.Attempt{Rid: rid, Backend: b.Name, Model: modelUsed, JobID: sessKey,
							Stream: streaming, Seq: seq, Status: http.StatusTooManyRequests,
							Class: string(clsSkipCap), Err: "router saturated: no upstream slot", OK: false})
						// Router满载日志：标记为router问题
						logAttempt(false, rid, b.Name, -1, seq, http.StatusTooManyRequests,
							time.Since(start).Milliseconds(), 0, "router saturated")
						retryAfter := int(opt.QueueWait.Seconds())
						if retryAfter < 1 {
							retryAfter = 1
						}
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
						w.Header().Set("X-Router-Rid", rid)
						w.WriteHeader(http.StatusTooManyRequests)
						json.NewEncoder(w).Encode(map[string]any{
							"error":        "router saturated: no upstream slot available",
							"rid":          rid,
							"backend":      b.Name,
							"concurrency":  b.Concurrency,
							"in_flight":    b.InFlight(),
							"waiters":      b.Waiters(),
							"queue_wait_s": opt.QueueWait.Seconds(),
							"hint":         "abort this task now, or retry after Retry-After",
						})
						log.Printf("[proxy] rid=%s SATURATED backend=%s conc=%d in_flight=%d waiters=%d -> 429",
							rid, b.Name, b.Concurrency, b.InFlight(), b.Waiters())
						return
					}
				}
			}
			attempted := false
			// key 来自所属账号的共享池：同账号下所有 backend 共用一个轮转游标
			keys := b.BoundAccount().APIKeys
			nKeys := len(keys)
			if nKeys == 0 {
				nKeys = 1
			}
			rot := b.BoundAccount().KeySeqNext()
			dominant := errClass("")

		keyLoop:
			for off := 0; off < nKeys; off++ {
				keyIdx := (rot + off) % nKeys
				key := ""
				if len(keys) > 0 {
					key = keys[keyIdx%len(keys)]
				}
				seq++
				attempted = true
				b.Stats().NoteAttempt()

				// 每个后端用它自己配的 model（对齐 python sidecar：{**payload, "model": backend.model}）。
				// `why`: 这是**唯一**的模型解析点，且只改 model 这一个 JSON 键
				//      —— 不能像过去那样对整段 body 做字符串替换（会连带改坏 tool_choice 等）。
				//      客户端传 "auto"/空 → 落到本后端的 model；后端没配则用 DEFAULT_MODEL。
				sendBody := body
				wantModel := b.EffectiveModel(start)
				if wantModel == "" {
					wantModel = opt.DefaultModel
				}
				effModel := modelUsed
				if pmap != nil && wantModel != "" {
					cp := make(map[string]any, len(pmap)+2)
					for k, v := range pmap {
						cp[k] = v
					}
					changed := false
					if cur, _ := cp["model"].(string); cur != wantModel {
						cp["model"] = wantModel
						changed = true
					}
					// 思考模型越来越多：reasoning_effort 是网关能统一压低推理预算的杠杆。
					// 仅在客户端没显式指定时注入默认值（尊重显式选择）；REASONING_EFFORT="" 关闭注入。
					// 与 seika_llm_router(main.go) 同源；SLR 已注入时这里守卫命中"已有"会跳过，不双注入。
					if opt.ReasoningEffort != "" {
						if _, has := cp["reasoning_effort"]; !has {
							cp["reasoning_effort"] = opt.ReasoningEffort
							changed = true
						}
					}
					if changed {
						if nb, err := json.Marshal(cp); err == nil {
							sendBody = nb
						}
					}
					effModel = wantModel
				}

				res := attempt(w, r, b, sendBody, streaming, key, session, start)
				a := store.Attempt{
					Rid: rid, Backend: b.Name, KeyIdx: keyIdx, Model: effModel, Stream: streaming,
					JobID: sessKey,
					Seq:   seq, Status: res.status, LatMs: res.latMs, FtMs: res.ftMs,
					Class: string(res.class), Err: truncate(res.msg, 500), OK: res.ok,
					PromptTokens: res.promptTokens, CompletionTokens: res.completionTokens,
					TotalTokens: res.totalTokens, CachedTokens: res.cachedTokens,
				}
				if res.ok {
					b.NoteSuccessMetrics(res.latMs, res.ftMs)
					b.Release()
					st.Emit(a)
					emitCapture(st, rid, b, streaming, effModel, &res, sendBody, r, start)
					logAttempt(true, rid, b.Name, keyIdx, seq, res.status, res.latMs, res.ftMs, "")
					// 会话亲和写表：**整个 rid 的最后一次 200**（内容已交付）
					// 才写——在 attempt 循环里写会把粘性钉到「第一发就 429」
					// 的那家。流式成功（[DONE] 交付完毕）同样从这里过。
					if opt.Affinity != nil && sessKey != "" {
						opt.Affinity.Put(sessKey, b.Name)
					}
					// 记录兜底模型成功
					if b.IsFallback {
						rt.RecordFallbackSuccess(b.Name)
					}
					// 重试成功：记录为router加分
					if isRetry {
						log.Printf("[proxy] rid=%s RETRY_SUCCESS on %s (was %s)", rid, b.Name, retryRid)
					}
					return
				}
				st.Emit(a)
				emitCapture(st, rid, b, streaming, effModel, &res, sendBody, r, start)
				logAttempt(false, rid, b.Name, keyIdx, seq, res.status, res.latMs, res.ftMs,
					fmt.Sprintf("%s: %s", res.class, truncate(res.msg, 300)))
				// 下游 context 已 done = 客户端主动断开或连接断了。此时上游的
				// context canceled 不是后端故障，noteFailure 据此跳过 60s 冷却，
				// 避免一次断连把全体在飞后端同时打入冷却 → 全网 503。
				clientGone := r.Context().Err() != nil
				b.NoteFailure(res.class, now, truncate(res.msg, 500), res.status, clientGone)
				// 记录兜底模型失败
				if b.IsFallback {
					rt.RecordFallbackFailure(b.Name, res.class)
				}
				fails = append(fails, failInfo{Backend: b.Name, KeyIdx: keyIdx,
					Class: string(res.class), Status: res.status, Msg: truncate(res.msg, 200)})
				// 会话逐出：判定走 errclass.ReleaseAffinity（与冷却/熔断/兜底
				// 同源，不发明第二个健康层）。429/5xx/模型下架 → 放弃；400、
				// 瞬时网络、空气泡 → 不动。mid-stream（res.wrote）在判定之先
				// 直接短路：响应已开始就没切换 backend，谈不上拆钉。
				if !res.wrote && opt.Affinity != nil && sessKey != "" {
					opt.Affinity.Relinquish(sessKey, res.class)
				}
				// 已经把内容写给客户端了（流式半途断）：不能再切后端，只能收尾
				if res.wrote {
					b.Release()
					log.Printf("[proxy] rid=%s mid-stream failure on %s after %dms (first_token=%dms, msg=%s), response already started; stop failover",
						rid, b.Name, res.latMs, res.ftMs, truncate(res.msg, 120))
					return
				}
				// 429错误：立即触发冷却，不管其他错误
				if res.class == cls429 {
					dominant = cls429
					break keyLoop
				}
				if dominant == "" {
					dominant = res.class
				}
				// 400/404 / 模型不存在 = 请求本身不被该后端接受，换 key 没用 → 直接换后端
				if res.class == cls400 || res.class == cls404 || res.class == clsModelGone {
					break keyLoop
				}
				if !errclass.KeyRotatable(res.class) {
					break keyLoop
				}
				// 其余继续轮换下一个 key
			}

			b.Release()
			if !attempted {
				continue
			}
			// 该后端所有 key 都失败 → 后端级冷却，后续请求主动跳过（省一次 429 往返）。
			// 配额类(429/403/401)走指数冷却（锁屏逻辑）；其余类别不冷却、维持原样。
			if dominant == cls429 || dominant == cls403 || dominant == cls401 {
				b.QuotaCooldown(dominant)
			}
		}

		// 全部失败：优先把上游 4xx 的真实原因回传给调用方
		status := http.StatusBadGateway
		payload := map[string]any{
			"error": "all backends failed", "rid": rid,
			"failures": fails,
		}
		if len(fails) > 0 {
			last := fails[len(fails)-1]
			if last.Status >= 400 && last.Status < 500 && last.Class != string(cls429) {
				status = last.Status
				payload["error"] = fmt.Sprintf("upstream rejected request (%d)", last.Status)
			}
			// 所有上游都在冷却（被上游限流）→ 语义上就是"现在别来"，回 429 而不是 503
			if allCooldown(fails) {
				status = http.StatusTooManyRequests
				payload["error"] = "all upstreams cooling down (rate limited)"
				w.Header().Set("Retry-After", "60")
			} else if allSkipped(fails) {
				status = http.StatusServiceUnavailable
				payload["error"] = "all backends skipped (capacity/cooldown)"
			}
		}
		// 添加重试信息：让下游知道可以重试，并指定下一个后端
		if len(order) > 1 {
			payload["retry_hint"] = "try next backend"
			payload["failed_backend"] = order[0]
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Router-Rid", rid)
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(payload)
		log.Printf("[proxy] rid=%s ALL_BACKENDS_FAILED status=%d fails=%d (%s)",
			rid, status, len(fails), summarizeFails(fails))
	}
}
