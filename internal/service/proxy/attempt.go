package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"shanhaigateway/internal/core/errclass"
	"shanhaigateway/internal/core/quirk"
	"shanhaigateway/internal/core/upstream"
	"shanhaigateway/internal/runtime"
)

// 上游 User-Agent。原来在 main.go 的包级 var（UPSTREAM_UA env，默认一个
// 硬编码的旧项目名/1.0），随 attempt 一起搬进 proxy 层；照旧用 runtime 读
// env，不依赖 config。
// ⚠️ 2026-10-11 接线批：默认值从硬编码的旧项目名改为 runtime.AppID()
// （= "gateway/<版本>"）—— 项目身份的唯一来源是
// internal/runtime/identity.go，硬编码一份 UA 就等于开了第二个身份来源；
// 上游不看 UA 内容、只要求它稳定非通用（Go 默认 UA 会被 Cloudflare 挡），
// 所以改默认值不影响连通性。与 validate.go 的 upstreamUA 口径对齐。
// env UPSTREAM_UA 覆写照旧有效。
var upstreamUA = runtime.EnvString("UPSTREAM_UA", runtime.AppID())

type errClass = errclass.Class

// 分类短名（搬入前的调用点原样可用；类型别名 + 常量别名，不是新定义）。
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

// isHopByHop 逐位照搬 seika_llm_router main.go（attempt 的请求头透传过滤用）。
// 没有归入 errclass：它不是错误语义，是 HTTP 头语义。
func isHopByHop(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

type attemptResult struct {
	ok     bool
	wrote  bool // 是否已开始写响应（一旦为真就绝不能再切后端）
	class  errClass
	status int
	ftMs   int64
	latMs  int64
	msg    string
	body   string // 上游响应体片段（排障/回传给调用方）

	promptTokens     int
	completionTokens int
	totalTokens      int
	cachedTokens     int

	// 留存（captures）专用字段：不参与现有 attempts/路由逻辑，只供挂钩点在
	// attempt() 返回后据此构造 Capture。
	respBody    []byte // 脱敏前的响应原文（非流式=整包；流式=captureTee 累积）
	truncated   bool   // respBody 因超 captureRespBodyMax 被截断
	usageParsed bool   // 是否从上游解析到真实 token 用量（决定 usage_source）
}

// parseUsage 从上游响应对象里取 token 用量（OpenAI 兼容）
// 返回 (prompt, completion, total, cached)
//
// 搬入时的核对结论（要求写明）：源实现**只**解析 OpenAI 兼容
// 的 usage.prompt_tokens / completion_tokens / total_tokens +
// prompt_tokens_details.cached_tokens。Anthropic 风格的
// prompt_cache_hit_tokens / prompt_cache_miss_tokens / credit
// **在源实现里就没有处理**，这里照原样不处理 —— 补上等于行为变更，不属于搬运。
func parseUsage(obj map[string]any) (int, int, int, int) {
	u, ok := obj["usage"].(map[string]any)
	if !ok {
		return 0, 0, 0, 0
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
	return p, c, t, cached
}

// asStr 安全取字符串。为什么不用 fmt.Sprint(v)：
// fmt.Sprint(nil) 会返回字面量 "<nil>"（非空！），会让"字段缺失"被误判成"有内容"，
// 于是空气泡型的响应被当成成功放过去（本机实测踩到）。
func asStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return ""
}

func readSnippet(rdr io.Reader, limit int) string {
	var buf bytes.Buffer
	io.Copy(&buf, io.LimitReader(rdr, int64(limit)))
	s := strings.TrimSpace(buf.String())
	if len(s) > limit {
		s = s[:limit]
	}
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, s)
}

// captureTee 流式响应正文的旁路累积器：最多攒 captureRespBodyMax 字节，超限即
// 截断并置 truncated。防止模型循环输出 1GB 时 tee 把整个进程内存吃掉 ——
// 丢一部分比丢整个进程好（READEM §五 那条"兜底后端 0 成功率空转"的后端若回个
// 巨大错误体，tee 无上限就会爆内存）。平时大多数 capture 远小于上限，开销可忽略。
type captureTee struct {
	buf       []byte
	truncated bool
	max       int
}

const captureRespBodyMax = 8 << 20 // 8MB

func (t *captureTee) Write(p []byte) {
	if t.truncated || len(p) == 0 {
		return
	}
	room := t.max - len(t.buf)
	if room <= 0 {
		t.truncated = true
		t.buf = t.buf[:t.max]
		return
	}
	if len(p) > room {
		t.buf = append(t.buf, p[:room]...)
		t.truncated = true
		return
	}
	t.buf = append(t.buf, p...)
}

// attempt 执行一次上游调用。streaming 时承担写响应体的职责（外部先别 WriteHeader）。
// 搬入前是 Backend 的方法；跨包搬入后成为自由函数，b 是第一个形参 —— 逻辑逐位未变。
// 返回值用**具名结果** res：后文在流式分支用 defer 回填 res.respBody/res.truncated。
// 必须具名——若返回无名的值，`return res` 会在 defer 执行前就拷贝返回值快照，
// 那些 defer 里才写的 respBody/truncated 到不了调用方（实测流式 resp_body 恒空，
// 2026-10-13 捕捉到后改成具名结果）。
func attempt(w http.ResponseWriter, r *http.Request, b *upstream.Backend, body []byte,
	streaming bool, key string, session string, start time.Time) (res attemptResult) {

	// 失败分支也要有真实耗时，否则统计里所有失败都是 lat_ms=0，延迟分位数会被污染
	defer func() {
		if res.latMs == 0 {
			res.latMs = time.Since(start).Milliseconds()
		}
	}()
	target := b.BaseURL + strings.TrimPrefix(r.URL.Path, "/v1")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	reqCtx, reqCancel := context.WithCancel(r.Context())
	defer reqCancel()
	req, err := http.NewRequestWithContext(reqCtx, r.Method, target, bytes.NewReader(body))
	if err != nil {
		res.class, res.msg = clsNetwork, err.Error()
		return res
	}
	for k := range r.Header {
		if !isHopByHop(k) {
			req.Header.Set(k, r.Header.Get(k))
		}
	}
	// 必须删掉客户端带来的 Content-Length：body 做过 auto→默认模型 替换 / model 覆盖后长度会变，
	// 带着旧长度会把请求体截断（火山实测回 "missing `messages` parameter"，opencode/百炼则吞成空内容）。
	req.Header.Del("Content-Length")
	req.Header.Del("Authorization")
	req.Header.Del("X-Api-Key")
	req.Header.Del("X-Task-Type")
	// X-Session-Key 是 router 内部约定，翻译成上游会话头后不再透传
	req.Header.Del("X-Session-Key")
	// 必须删掉客户端的 Accept-Encoding：一旦显式带了这个头，Go 的 transport 就
	// **不会**自动解压上游的 gzip 响应；而我们回响应时又没转发 Content-Encoding，
	// 结果把 gzip 字节当 JSON 发给下游（httpx 客户端实测 UnicodeDecodeError 0x8b）。
	// 删掉后由 Go 自己加 gzip 并透明解压，响应体恒为明文。
	req.Header.Del("Accept-Encoding")
	// 客户端可能用了 curl -d 的默认 application/x-www-form-urlencoded。
	// 我们发的永远是 JSON，照抄 Content-Type 会让上游按表单去解析 JSON 体
	// （火山实测回 "missing `messages` parameter"，opencode/百炼则吞成空内容）。
	req.Header.Set("Content-Type", "application/json")
	// 必须用稳定的业务 UA：Go 默认的 "Go-http-client/1.1" 会被 Cloudflare 挡
	// （opencode.ai 实测返回 403 error code: 1010，而直连 curl 正常）。
	// UA 的默认值归 runtime.AppID()（identity.go 唯一身份来源）。
	req.Header.Set("User-Agent", upstreamUA)
	req.ContentLength = int64(len(body))
	// 会话头：按后端配置映射（opencode 系要 x-opencode-session）。
	// 别的上游要别的头，只需在该后端配置里加 session_header，不用改代码。
	if b.SessionHeader != "" {
		req.Header.Set(b.SessionHeader, session)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	// 缝①（怪癖层）：通用 header 清理（上面的 Del/Set）+ 会话头 + 鉴权头全部完成
	// 之后、发往上游之前的最后一次出口定制机会。默认实现是 no-op ——
	// 没怪癖的上游行为与不接怪癖层逐位一致。q 在本请求的缝②/缝③ 复用同一个。
	q := quirk.Get(b.Driver)
	q.Decorate(req, b)

	resp, err := b.Client().Do(req)
	if err != nil {
		res.class = clsNetwork
		if errorsTimeout(err) {
			res.class = clsTimeout
		}
		res.msg = err.Error()
		// 上游网络错误日志：暴露问题源（写入stderr和attempt日志）
		log.Printf("[upstream-error] backend=%s class=%s err=%q", b.Name, res.class, res.msg)
		return res
	}
	defer resp.Body.Close()

	res.status = resp.StatusCode
	if resp.StatusCode >= 400 {
		res.class = errclass.ClassifyStatus(resp.StatusCode)
		res.body = readSnippet(resp.Body, 2048)
		res.msg = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, res.body)
		// ⚠️ "模型不存在/已下架"是**后端**的故障，不是客户端请求的错，但它回的是 400。
		// 旧代码一律归 cls400 → countsAsFault=false → 不涨 failStreak、不跳闸 →
		// 该后端变成静默黑洞（2026-10-06 起 bunny 两条 0 成功率空转 4 天半的根因）。
		// 改判成 clsModelGone 后会计入 fail_streak、连续 5 次自动熔断摘掉。
		// 只在"本后端配了 model"时改判：没配的话上游看到的是客户端原样的 model 名，
		// 那就真是客户端的问题，不该让后端背锅。
		// 缝②（怪癖层）：先问本后端 driver 的怪癖认不认领这句话，
		// (Class,false)=不归我管 → 落回下面的通用词表判定（逐位照旧）。
		// ⚠️ 原有的两道前置条件（400/404 + 本后端配了 model）原样保留：
		// 不能因为问了怪癖就把 429/500 也改判，也不能让「没配 model」的后端
		// 替客户端背锅 —— 前置条件之外 res.class 一个字节都不动。
		if (res.class == cls400 || res.class == cls404) && b.EffectiveModel(start) != "" {
			if c, ok := q.Classify(res.status, res.body); ok {
				res.class = c
			} else if errclass.ModelUnavailableBody(res.body) {
				log.Printf("[upstream-error] backend=%s model=%q 上游说模型不可用 -> 改判为后端故障 (%s): %s",
					b.Name, b.EffectiveModel(start), clsModelGone, truncate(res.body, 160))
				res.class = clsModelGone
			}
		}
		// why: 上游 4xx 往往是"我们发出去的 body 不合它口味"，光看上响应体无法判断
		//      是哪一方的问题（实测有 tool_choice 被上游指为非法值、却看不出谁写的案例）。
		//      这里把关键字段的精确取值落日志，一眼分清是客户端发错还是网关改坏。
		sentModel, sentTC := "", ""
		toolsSet := false
		if len(body) > 0 {
			var probe map[string]any
			if json.Unmarshal(body, &probe) == nil {
				sentModel, _ = probe["model"].(string)
				if v, ok := probe["tool_choice"]; ok && v != nil {
					if b, err := json.Marshal(v); err == nil {
						sentTC = string(b)
					}
				}
				toolsSet = probe["tools"] != nil
			}
		}
		// 上游HTTP错误日志：暴露完整上下文
		log.Printf("[upstream-error] backend=%s status=%d model=%q tool_choice=%s tools=%v sent_len=%d resp_body=%q",
			b.Name, resp.StatusCode, sentModel, sentTC, toolsSet, len(body), res.body)
		return res
	}

	if !streaming {
		bbuf, _ := io.ReadAll(resp.Body)
		var obj map[string]any
		if json.Unmarshal(bbuf, &obj) == nil {
			if obj["error"] != nil {
				res.class, res.body = clsErrBody, readSnippet(bytes.NewReader(bbuf), 2048)
				res.msg = "200 + error body"
				// 上游200但body是error：暴露问题源
				log.Printf("[upstream-error] backend=%s class=err_body resp_body=%q", b.Name, res.body)
				return res
			}
			ch, ok := obj["choices"].([]any)
			if !ok || len(ch) == 0 {
				res.class, res.body = clsNoContent, readSnippet(bytes.NewReader(bbuf), 1024)
				res.msg = "200 + missing/empty choices"
				// 上游200但无choices：暴露问题源
				log.Printf("[upstream-error] backend=%s class=no_content resp_body=%q", b.Name, res.body)
				return res
			}
			has := false
			if m0, ok := ch[0].(map[string]any); ok {
				if msg, ok := m0["message"].(map[string]any); ok {
					if strings.TrimSpace(asStr(msg["content"])) != "" || msg["tool_calls"] != nil || strings.TrimSpace(asStr(msg["reasoning_content"])) != "" {
						has = true
					}
				}
				if strings.TrimSpace(asStr(m0["text"])) != "" {
					has = true
				}
			}
			if !has {
				res.class, res.body = clsEmptyOut, readSnippet(bytes.NewReader(bbuf), 1024)
				res.msg = "200 + empty content/tool_calls"
				// 上游200但内容为空：暴露问题源
				log.Printf("[upstream-error] backend=%s class=empty_output resp_body=%q", b.Name, res.body)
				return res
			}
		}
		res.latMs = time.Since(start).Milliseconds()
		res.ftMs = res.latMs
		// 缝③（怪癖层）：怪癖不认（OK=false）就落回原有的 parseUsage（= OpenAI 默认）。
		if u := q.ParseUsage(bbuf); u.OK {
			res.promptTokens, res.completionTokens, res.totalTokens, res.cachedTokens =
				u.Prompt, u.Completion, u.Total, u.CachedPrompt
			res.usageParsed = true
		} else {
			res.promptTokens, res.completionTokens, res.totalTokens, res.cachedTokens = parseUsage(obj)
			if res.totalTokens > 0 {
				res.usageParsed = true // 解析到真实值才标 upstream；0 视为 unavailable
			}
		}
		res.respBody = bbuf // 非流式整包带回，供 captures 落留存
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Served-By", b.Name)
		w.WriteHeader(resp.StatusCode)
		w.Write(bbuf)
		res.ok = true
		return res
	}

	// ---- 流式：首帧到达前不写响应头，保证可干净切换后端 ----
	ftTimeout := b.FirstTokenTimeout(start)
	var gotFirst atomic.Bool
	timer := time.AfterFunc(ftTimeout, func() {
		if !gotFirst.Load() {
			reqCancel()
		}
	})
	defer timer.Stop()

	// 留存的流式 tee：旁路累积实际下发的内容行（data 事件），超 8MB 截断。
	// 断连/midstream 时 defer 仍把它拷回 res.respBody —— 部分响应也是数据。
	tee := &captureTee{max: captureRespBodyMax}
	defer func() {
		res.respBody = tee.buf
		res.truncated = tee.truncated
	}()

	flusher, _ := w.(http.Flusher)
	started := false
	emitted := false
	beginWrite := func() {
		if started {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Served-By", b.Name)
		w.WriteHeader(resp.StatusCode)
		started = true
		res.wrote = true
	}

	reader := bufio.NewReader(resp.Body)
	for {
		line, rerr := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if !strings.HasPrefix(trimmed, "data:") {
				var o map[string]any
				if json.Unmarshal([]byte(trimmed), &o) == nil && o["error"] != nil {
					res.class = clsErrBody
					res.body = strings.TrimSpace(trimmed)
					res.msg = "200 + non-SSE error body"
					if emitted {
						res.class = clsMidStream
						res.latMs = time.Since(start).Milliseconds()
						endStream(w, flusher, started)
						return res
					}
					return res
				}
				// SSE 靠空行派发事件：空行 / event: / id: / : 注释 都是分帧的一部分，
				// 必须原样透传。吞掉空行会让严格 SSE 客户端把整段事件攒到连接结束才派发
				// （外部表现：不流式、最后一次性返回）。
				if started {
					w.Write([]byte(line))
					if flusher != nil {
						flusher.Flush()
					}
				}
				continue
			}
			data := strings.TrimSpace(trimmed[len("data:"):])
			if data == "" {
				continue
			}
			if data == "[DONE]" {
				if !emitted {
					// 一个字没吐就收尾 = 空气泡；此时还没写响应头，可干净切后端
					res.class = clsNoContent
					res.msg = "stream ended with no content"
					return res
				}
				beginWrite()
				w.Write([]byte("data: [DONE]\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
				res.ok = true
				res.latMs = time.Since(start).Milliseconds()
				return res
			}
			var o map[string]any
			if json.Unmarshal([]byte(data), &o) == nil && o["error"] != nil {
				res.class = clsErrBody
				res.body = data
				res.msg = "200 + SSE error chunk"
				if emitted {
					res.class = clsMidStream
					res.latMs = time.Since(start).Milliseconds()
					endStream(w, flusher, started)
					return res
				}
				return res
			}
			// 流式用量：有些上游把 usage 放在末尾 chunk（需 stream_options.include_usage）
			// 缝③（怪癖层）：怪癖不认或没解析出总量 → 落回原有的 parseUsage（逐位照旧）。
			if u := q.ParseUsage([]byte(data)); u.OK && u.Total > 0 {
				res.promptTokens, res.completionTokens, res.totalTokens, res.cachedTokens =
					u.Prompt, u.Completion, u.Total, u.CachedPrompt
				res.usageParsed = true
			} else if p, c, t, cached := parseUsage(o); t > 0 {
				res.promptTokens, res.completionTokens, res.totalTokens, res.cachedTokens = p, c, t, cached
				res.usageParsed = true
			}
			// 首个有效 chunk：停定时器并写响应头
			if !gotFirst.Swap(true) {
				timer.Stop()
				res.ftMs = time.Since(start).Milliseconds()
			}
			beginWrite()
			tee.Write([]byte(line)) // 旁路累积内容行，供 captures 留存
			w.Write([]byte(line))
			if flusher != nil {
				flusher.Flush()
			}
			emitted = true
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			// 首帧超时(reqCancel)/网络错误/流中断
			if !gotFirst.Load() {
				res.class = clsFirstTok
				res.msg = rerr.Error()
				return res
			}
			res.class = clsMidStream
			res.msg = rerr.Error()
			res.latMs = time.Since(start).Milliseconds()
			endStream(w, flusher, started)
			return res
		}
	}
	res.latMs = time.Since(start).Milliseconds()
	if emitted {
		// 内容已交付，但不能再切后端（响应已开始）。
		// 没收到 [DONE] 说明上游提前掐断 → 记 mid_stream_fail，让统计反映上游抖动。
		endStream(w, flusher, started)
		res.class, res.msg = clsMidStream, "upstream closed before [DONE]"
		return res
	}
	res.class = clsNoContent
	res.msg = "stream EOF with no content"
	return res
}

// endStream 已吐字后断流的收尾：补 [DONE]，不可再切后端
func endStream(w http.ResponseWriter, flusher http.Flusher, started bool) {
	if started {
		w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func errorsTimeout(err error) bool {
	type timeout interface{ Timeout() bool }
	t, ok := err.(timeout)
	return ok && t.Timeout()
}
