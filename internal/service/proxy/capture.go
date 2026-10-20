package proxy

import (
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"shanhai-gateway/internal/core/store"
	"shanhai-gateway/internal/core/upstream"
)

// partialRatioHasContent 是 partial（中途断连、内容已部分下发）的 partial_ratio
// 哨兵值，表示"有部分输出但未到完整"。它**不是实测比例** —— 我们拿不到"该请求
// 应该多长"（需要模型级历史 P50，A6 将来可补），所以用 0.5 当作有内容的偏序量。
// 消费方应以 usable=false 为主筛，partial_ratio 只用于分辨"有没有内容"：
//   - 完整响应      → usable=true,  partial_ratio=1.0
//   - partial 有内容 → usable=false, partial_ratio=0.5
//   - partial 无内容  → usable=false, partial_ratio=0（拒绝/空气泡/未下发即断）
const partialRatioHasContent = 0.5

// cleanJobID 白名单截断 job_id（对齐 SLR 的口径）：只留 unicode 字母/数字 +
// `.-_:/`，截 64 rune。**必须用下表的 IsNumber 而非 IsDigit** —— IsDigit 只认
// ASCII/拉丁，认不出 ④ Unicode No 类（全角、上标、分数），会把它们洗成空串；
// 这个坑在 SLR 上踩过，不再踩第二次。
func cleanJobID(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) ||
			r == '.' || r == '-' || r == '_' || r == ':' {
			b.WriteRune(r)
			n++
			if n >= 64 {
				break
			}
		}
	}
	return b.String()
}

// scrubCapture A5 最小脱敏：落库前剥掉正文里的凭据/内网/个人可标识信息。原则
// 宁过度脱敏不漏。Authorization/X-Api-Key/X-Task-* 等请求头已在 attempt() 里删掉；
// 这里只对付混进 req_body/resp_body 的正文。将来按 task_type 分策略时，保持本函数
// 为**唯一入口**，不要在挂钩点撒 regex。
//
// 规则按**顺序**执行（下表中越靠前优先级越高），一把过完再回写。顺序语义：
//   - 具体静态密钥先于通用前缀（slr_ 硬编码值落 [token] 而不是被通用前缀落 [key]）
//   - Bearer 先于普通 header 泛化，避免 Bearer 值被配置形式吞错
//   - query 凭据、路径、IP、手机号覆盖真流量高频泄漏点（2026-10-14 A5 扩批）
//
// scrubRules 按顺序执行；一次性编译（scrub 走每请求热路径，循环内 MustCompile 浪费）。
type scrubRule struct {
	re   *regexp.Regexp
	repl string
}

func compile(pat, repl string) scrubRule { return scrubRule{regexp.MustCompile(pat), repl} }

var scrubRules = []scrubRule{
	// 0) 具体静态密钥：已知事实 value 硬编码。它不是秘密（文档公开），但**不该进留存
	//    库**——某次调试贴了 curl 就会出现在 req_body。
	//    部署者经面板换掉默认 key 后（POST /api/dashkey），**新** key 由下方
	//    dynamicScrub 动态槽接管（api 层显式推送，capture 自身仍不读 env/文件，
	//    不引入启动路径依赖）；本条继续兜住默认值。
	compile(`slr_2e61800473a99f997cef6d6027d328ce`, `[token]`),
	// 1) 内部绝对路径。/opt 吞整段（整个部署目录都非公开）。/home 只掩用户名、刻意
	//    留项目名（scf-dev 等）——项目名在全部文档里公开，且保留它可聚合"哪类请求
	//    发生在哪个项目下"。这是**有意为之**，不是 regex 缺陷，别当 bug 改。
	//    ⚠️ 不对称保护：/opt 整段 vs /home 半段是**有意**的不对称。若未来有人把它们
	//    "统一"成整段掩，会丢"项目聚合维度"，改前先读本注释。
	compile(`/opt/[A-Za-z0-9_.\-/]+`, `[opt]`),
	compile(`/home/[A-Za-z0-9_.-]+`, `[home]`),
	// 2) loopback / link-local / 私有 IPv4 段：真流量里各种代理/内网/容器地址
	//    一定会被 user prompt 提到。公网 IP 不掩（示例用 RFC5737 文档段）。
	compile(`\b127\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`, `[ip]`),
	compile(`\b169\.254\.\d{1,3}\.\d{1,3}\b`, `[ip]`),
	compile(`\b10\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`, `[ip]`),
	compile(`\b172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}\b`, `[ip]`),
	compile(`\b192\.168\.\d{1,3}\.\d{1,3}\b`, `[ip]`),
	// 3) 中国手机号：1[3-9] + 9 位，真流量 user message 里可能留。
	//    RE2 无 lookbehind/lookahead，用 \b 词边界匹配孤立 11 位段（连成长串的数字不掩，
	//    "13800138000"这种独立手机号会掩）。
	compile(`\b1[3-9]\d{9}\b`, `[phone]`),
	// 4) URL query 里的凭据：SLR 上专门"不认 query token"就是这个原因——query 会进
	//    日志/正文，同理进 req_body。?token=/api_key=/access_token=/key=/secret=/
	//    signature=/auth=/password=/pwd=（后三条 OAuth 回调常见）都盖。
	compile(`([?&](?:token|api_key|access_token|key|secret|signature|auth|password|pwd)=)[^&\s"']+`, `${1}[redacted]`),
	// 5) 含鉴权的请求行（预判性：将来若把整请求/header 也落 capture，这里已盖头）。
	//   裸 `Bearer <值>`（自由文本，无 Authorization: 前缀）也要盖——用户消息/代码示例
	//   里直接写 "Bearer <token>" 很常见，不能只靠 Authorization 头规则兜（2026-10-14
	//   真流量自测暴露：只盖 Authorization 头时，自由文本的 "Bearer tok123" 会漏）。
	compile(`(?i)\bBearer\s+[A-Za-z0-9_\-\.]{4,}`, `Bearer [redacted]`),
	//   Authorization 与 X-Api-Key 一并盖：值内可能又有 Bearer 段，但通用规则会把
	//   `Authorization: <整段非逗号非换行>` 全吞。白名单值本身足够长，不会被吞错。
	compile(`(?i)(Authorization|X-Api-Key):\s*[^,\r\n]+`, `$1: [redacted]`),
	//   Cookie 一行内多个 pair（a=b; c=d），要吞到行尾而非第一个分号。
	//   ⚠️ 风险留记：若 body 出现非 HTTP header 上下文的 `Cookie:` 字符串（如代码示例），
	//     会吞到该行行尾——概率低，可接受，别当 bug 重写。
	compile(`(?i)(Cookie):\s*[^\r\n]+`, `$1: [redacted]`),
	// 6) 通用密钥前缀：sk- / ark- / sk-ws- / slr_ / gp- 等。slr_ 用下划线分隔故单独列。
	compile(`\b(?:sk-|ark-|sk-ws-|slr_|gp-)[A-Za-z0-9_\-]{6,}\b`, `[key]`),
}

// dynamicScrub 动态密钥槽（2026-10-19 面板改 key 功能配套）：api 层在启动与
// 面板改 key 时把**当前生效**的看板静态 key 塞进来（SetDynamicSecret），scrub
// 按字面量替换为 [token]。上面静态表只兜仓库可见的内置默认；部署者换出来的
// 新 key 由本槽接管——新密钥既不进仓库文件，也不进启动路径（api 层显式推送，
// capture 依旧自足）。零值（re==nil）= 无动态密钥。
var dynamicScrub atomic.Value // 存 scrubRule

// SetDynamicSecret 设置/清除动态脱敏密钥（api 层调用；空串 = 清除）。
// QuoteMeta 保证字面量匹配——key 里的 . * + 等不当正则元字符。
func SetDynamicSecret(secret string) {
	if secret == "" {
		dynamicScrub.Store(scrubRule{})
		return
	}
	dynamicScrub.Store(compile(regexp.QuoteMeta(secret), "[token]"))
}

func scrubCapture(s string) string {
	for _, r := range scrubRules {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	if dr, ok := dynamicScrub.Load().(scrubRule); ok && dr.re != nil {
		s = dr.re.ReplaceAllString(s, dr.repl)
	}
	return s
}

// ============================================================
// A6 消费规则（写在这里，别只留对话里——消费者读代码才能拦住）：
//   - 跨 SLR→shadow 的关联钥匙不是 rid（两系统 rid 各生成各的），而是
//     (job_id, 时间窗)：SLR 存 attempts.job_id = cleanJobID(X-Job-Id)，shadow 的
//     captures.job_id 同口径，对得上=同一条 SCF→SLR→shadow 链；SLR 不用动。
//     见 SLR main.go:990：客户端头全部复制给上游，X-Job-Id 不在删除名单，原样透传。
//   - **同一个 rid 会出现多条 captures**（重试/partial 场景，非 bug）。
//     消费规则：同 rid 多条按 ts 排序，usable=true 的**最后一条是最终交付**，
//     其余按 partial 或 retry-failed 处理。
//
// ============================================================
// emitCapture 在 attempt() 返回后的收敛点（成功与失败两处）构造 captures 留存行
// 并旁路投入 store。字段口径与 attempts 对齐（model=effModel、status=真 HTTP 码、
// ts=请求开始时刻）；正文在**入 channel 前**脱敏（A5）——留存库里永远不落原文。
//
// 有意不补的捕获场景：SATURATED 429（attempt() 前的排队拒绝，未发上游、无响应体，
// 留存它只是噪音）。这里只覆盖"确实尝试过上游"的请求。
func emitCapture(st *store.Store, rid string, b *upstream.Backend, streaming bool,
	effModel string, res *attemptResult, sendBody []byte, req *http.Request, start time.Time) {

	c := store.Capture{
		Rid:              rid,
		Backend:          b.Name,
		Account:          b.BoundAccount().Name,
		Model:            effModel,
		Stream:           streaming,
		Status:           res.status, // 真 HTTP 码：partial 时是 200（已下发才有 partial），不造 499
		LatMs:            res.latMs,
		FtMs:             res.ftMs,
		PromptTokens:     res.promptTokens,
		CompletionTokens: res.completionTokens,
		TotalTokens:      res.totalTokens,
		CachedTokens:     res.cachedTokens,
		TaskType:         req.Header.Get("X-Task-Type"),
		JobID:            cleanJobID(req.Header.Get("X-Job-Id")),
		ReqBody:          scrubCapture(string(sendBody)),
		RespBody:         scrubCapture(string(res.respBody)),
		Truncated:        res.truncated,
		Ts:               float64(start.UnixNano()) / 1e9, // ts=请求开始时刻（决策②）
	}
	// usage_source：只有真从上游解析到正总量才算 upstream，其余（上游没给/给0/
	// 解析失败）一律 unavailable —— 消费方聚合时能 WHERE usage_source='upstream'。
	if res.usageParsed {
		c.UsageSource = store.UsageSourceUpstream
	}
	// usable/partial_ratio（决策③）：完整=可用；中途断连=不可用+有内容哨兵0.5。
	if res.ok {
		c.Usable = true
		c.PartialRatio = 1.0
	} else if res.wrote && len(res.respBody) > 0 {
		c.Usable = false
		c.PartialRatio = partialRatioHasContent
	}
	st.EmitCapture(c)
}
