package config

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"shanhaigateway/internal/core/errclass"
	"shanhaigateway/internal/core/upstream"
	"shanhaigateway/internal/runtime"
)

// 本文件是 seika_llm_router/go/validate.go 的搬入版，判定逻辑逐条照搬：
// 真发一个最小请求探测每个启用后端，把「永久失效」写进黑名单（此后不再载入），
// 「瞬时故障」只记 warn、照常载入。
//
// ── 最重要的一条规则 ────────────────────────────────────────
// **永久失效**（欠费 / 额度耗尽 / key 失效 / 模型不存在）→ 写黑名单 + 不载入。
// **瞬时故障**（网络抖动 / DNS 解析失败 / 5xx / 429）→ 只记 warn，**不拉黑**。
//
// 这条分界线不是随手划的：2026-10-10 阿里云 DNS 抖动期间，程序把所有后端
// 一次性判成「连不上」，若把它归进永久类，全站后端会被同时拉黑 ——
// 一个几秒钟的外部抖动，换来一个要人工逐条清理的黑名单文件。
// 「探测失败」和「上游明确说这个后端废了」是两回事，别把它们混为一谈。
//
// ⚠️ 黑名单**只写不删**是既有行为，照搬不改：某个后端被拉黑之后，
// 即使它当天就恢复了，条目也**不会自动移除**，必须人工清理黑名单文件。
// 理由是自动恢复的代价不对称 —— 反复拉黑/放回的抖动会让人完全无法判断
// 「现在到底是哪个后端在服役」，而人工清理的代价只是重启一次。
// ⚠️ 这一点在代码里没有任何自动化保障，所以本包专门用测试把它钉死
// （TestBlocklistNeverRemovesRecoveredEntry）。

// envValidate 关闭启动自检（ROUTER_VALIDATE=0）。
const envValidate = "ROUTER_VALIDATE"

// validateTimeout 单个后端的探测超时。
const defaultValidateTimeout = 20 * time.Second

// validateConcurrency 并发探测的后端数上限。
// 照搬旧实现的 8：探测是真发请求，一次性放几十个出去会被上游当成流量，
// 拿到一堆 429，然后被误判成「瞬时故障」—— 自检自己把上游惹了。
const validateConcurrency = 8

// BlockedEntry 是启动校验判定为「永久失效」的后端。
// 持久化后，后续每次启动直接跳过（等效删除），不再重复探测、也不加载。
type BlockedEntry struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// blocklist 黑名单文件：`[]BlockedEntry` 的 JSON。
//
// 存成数组而不是对象，是为了让人能直接 cat 出来看、直接手改。
// map 的 key 是后端名，读起来还要在脑子里做一次映射。
type blocklist []BlockedEntry

// LoadBlocklist 读黑名单文件；文件不存在/损坏都返回空表。
//
// 损坏（解析失败）只记一条 warn 并当作空表：黑名单坏了不该让网关起不来，
// 最多是下次启动重新探测一遍 —— 代价可控，而「因为黑名单文件里多了一个逗号
// 就整个网关拒绝启动」不可接受。
func LoadBlocklist(path string) map[string]BlockedEntry {
	out := map[string]BlockedEntry{}
	data, err := readFile(path)
	if err != nil {
		return out
	}
	var list blocklist
	if json.Unmarshal(data, &list) != nil {
		log.Printf("[config/validate] 黑名单 %s 已损坏，忽略", path)
		return out
	}
	for _, e := range list {
		if e.Name != "" {
			out[e.Name] = e
		}
	}
	return out
}

// SaveBlocklist 写黑名单。
//
// ⚠️ **只写不删**：本函数把传入的整张表原样写回，调用方往里加条目它就多一条，
// 但绝不会自己删掉表里的任何东西 —— 哪怕那个后端今天已经恢复正常。
// 已恢复的条目不会自动移除，需人工清理。
func SaveBlocklist(path string, m map[string]BlockedEntry) error {
	list := make(blocklist, 0, len(m))
	for _, e := range m {
		list = append(list, e)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("config: 序列化黑名单失败：%w", err)
	}
	// ⚠️ 与旧实现一致用 0644：黑名单里只有后端名和一段报错摘要，不含 key。
	// 但它确实是「能让某个后端不再被使用」的控制文件，权限收紧由部署方负责。
	if err := writeFile(path, data); err != nil {
		return fmt.Errorf("config: 写黑名单 %s 失败：%w", path, err)
	}
	return nil
}

// ProbeResult 是一次探测的结论。
type ProbeResult struct {
	// OK = 该（后端, key）组合真实可用。
	OK bool

	// Permanent = 永久失效（额度/鉴权/模型不存在），应打标记删除。
	// ⚠️ 只有**明确**的回包（401/403/404 及其文案）才置位；
	// 连不上、超时、5xx 一律留 false —— 见文件头的分界线。
	Permanent bool

	// Reason 人可读的结论，写进日志与黑名单。
	Reason string

	// KeyInvalid = 401：这把 key 本身失效（key 级，可从账户池剔除）。
	KeyInvalid bool

	// Dropped 需要从账户 key 池剔除的失效 key（原始值，日志里脱敏）。
	Dropped []string

	// KeyStat 多 key 时的结果摘要，拼进 [config/validate] 日志。
	KeyStat string
}

// Prober 探测一个后端，聚合它下面**所有 key** 的单发结果。
//
// 抽成接口（而不是写死成 HTTP 实现）的理由只有一个：测试不能真打上游。
// 判定逻辑（哪些算永久、哪些算瞬时、401 的 key 怎么摘）才是这一层真正的东西，
// 它必须能在没有网络的情况下被表驱动测试逐条钉死。
type Prober interface {
	Probe(ctx context.Context, b *upstream.Backend) ProbeResult
}

// ProberFunc 让普通函数满足 Prober。
type ProberFunc func(ctx context.Context, b *upstream.Backend) ProbeResult

// Probe 调用 f。
func (f ProberFunc) Probe(ctx context.Context, b *upstream.Backend) ProbeResult {
	return f(ctx, b)
}

// httpProber 是真实实现：真发一个最小请求。
type httpProber struct{}

// Probe 实现 Prober。
func (httpProber) Probe(ctx context.Context, b *upstream.Backend) ProbeResult {
	return probeBackend(ctx, b)
}

// NewHTTPProber 返回真实探测器（会发网络请求）。
//
// 刻意导出来而不是藏在 Validate 里：main 要在启动日志里说清「这次自检是真打
// 上游还是被跳过了」，得能显式选择；而测试要传假的进去。
func NewHTTPProber() Prober { return httpProber{} }

// Validate 启动校验：剔除黑名单/手动标签 → 并发探测 → 永久失效者不加载并写入黑名单。
// 返回本次实际应加载的后端。
//
// **不做 log.Fatal**：旧实现在这里 fatal 两次（应用黑名单后为空、探测后为空）。
// 现在一律返回 error —— 「全都死了」是调用方要展示给用户并决定怎么退出的状况，
// 不是一个库该替它做的决定。
//
// prober 为 nil 时跳过探测（等价于旧实现的 ROUTER_VALIDATE=0）。
// paths 为 nil 或 p 为空时返回 error，不 panic。
func Validate(p *runtime.Paths, snap *Snapshot, prober Prober) ([]*upstream.Backend, error) {
	if p == nil {
		return nil, fmt.Errorf("config: Validate 的 Paths 为 nil")
	}
	if snap == nil {
		return nil, fmt.Errorf("config: Validate 的 Snapshot 为 nil")
	}

	blocked := LoadBlocklist(p.BlockedFile)

	// 只保留 enabled、未被黑名单命中、且未被打「暂停标签」的后端；命中 = 直接删除
	usable := make([]*upstream.Backend, 0, len(snap.Backends))
	for _, b := range snap.Backends {
		if !b.Enabled {
			continue
		}
		// 手动标签（首发轮转用）：没恢复就不载入。与下面的自动黑名单同一个"不载入"逻辑。
		if until, paused := manualPause(b); paused {
			log.Printf("[config/validate] removed %s (manual tag, not loaded until %s: %s)",
				b.Name, until.Format(time.RFC3339), b.DisabledReason)
			continue
		}
		if e, bad := blocked[b.Name]; bad {
			log.Printf("[config/validate] removed %s (blocked: %s)", b.Name, e.Reason)
			continue
		}
		usable = append(usable, b)
	}
	if len(usable) == 0 {
		return nil, fmt.Errorf(
			"config: 应用黑名单与手动标签后没有可用后端（共 %d 个，全部 enabled=false / 被打标签 / 在黑名单里）",
			len(snap.Backends))
	}

	// prober 为 nil = 关闭自检，保留剔除后的结果直接返回
	if prober == nil {
		return usable, nil
	}

	timeout := runtime.EnvDuration("VALIDATE_TIMEOUT", defaultValidateTimeout)
	type item struct {
		b   *upstream.Backend
		res ProbeResult
	}
	results := make([]item, len(usable))
	var wg sync.WaitGroup
	sem := make(chan struct{}, validateConcurrency)
	for i, b := range usable {
		wg.Add(1)
		go func(i int, b *upstream.Backend) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			results[i] = item{b, prober.Probe(ctx, b)}
		}(i, b)
	}
	wg.Wait()

	kept := make([]*upstream.Backend, 0, len(usable))
	changed := false
	for _, it := range results {
		r := it.res
		m := it.b.EffectiveModel(time.Now())

		// 401 失效 key 从账户池剔除（运行期生效，不回写配置文件）。
		// 账户池是同账号所有后端共享的指针，按值过滤——多后端报同一把死 key 时天然幂等。
		if len(r.Dropped) > 0 {
			drop := make(map[string]bool, len(r.Dropped))
			for _, k := range r.Dropped {
				drop[k] = true
			}
			acct := it.b.BoundAccount()
			ks := acct.APIKeys
			out := make([]string, 0, len(ks))
			for _, k := range ks {
				if !drop[k] {
					out = append(out, k)
				}
			}
			if len(out) != len(ks) {
				acct.APIKeys = out
				for _, k := range r.Dropped {
					log.Printf("[config/validate] %s dropped dead key %s (401 auth)", it.b.Name, maskKey(k))
				}
			}
		}

		ks := ""
		if r.KeyStat != "" {
			ks = " [" + r.KeyStat + "]"
		}
		switch {
		case r.OK:
			log.Printf("[config/validate] ok   %s (%s)%s", it.b.Name, m, ks)
			kept = append(kept, it.b)
		case r.Permanent:
			log.Printf("[config/validate] DEAD %s (%s) — not loading: %s%s",
				it.b.Name, m, truncate(r.Reason, 200), ks)
			blocked[it.b.Name] = BlockedEntry{
				Name:   it.b.Name,
				Reason: truncate(r.Reason, 300),
				At:     time.Now().Format(time.RFC3339),
			}
			changed = true
		default:
			// ⚠️ 瞬时故障：只 warn，**不拉黑**。见文件头的分界线。
			log.Printf("[config/validate] warn %s (%s) transient, keep: %s%s",
				it.b.Name, m, truncate(r.Reason, 200), ks)
			kept = append(kept, it.b)
		}
	}

	if changed {
		if err := SaveBlocklist(p.BlockedFile, blocked); err != nil {
			// 写不进去不能当致命错误：探测结论已经生效（本次不载入），
			// 下次启动再探一遍只是多花几个请求。让网关为写不进黑名单而拒绝
			// 启动，是把「记录失败」升级成「服务不可用」。
			log.Printf("[config/validate] %v", err)
		} else {
			log.Printf("[config/validate] updated blocklist %s (%d entries)", p.BlockedFile, len(blocked))
		}
	}

	if len(kept) == 0 {
		return nil, fmt.Errorf(
			"config: 探测后没有可用后端（共探测 %d 个，全部判定为永久失效；"+
				"黑名单 %s 已更新 —— ⚠️ 黑名单只写不删，恢复后需人工清理）",
			len(usable), p.BlockedFile)
	}
	return kept, nil
}

// ShouldValidate 读 ROUTER_VALIDATE，默认开。
//
// 照搬旧实现的口径：只有显式等于 "1" 才关，任何别的值（包括空）都开着。
// 这个"宽松关闭"是对的 —— 拼错一个值不至于让自检被静默跳过。
func ShouldValidate() bool { return runtime.EnvString(envValidate, "1") != "0" }

// probeBackend 探测后端可用性。2026-09-24（批次3）改为**逐把 key 都探**：
// 原来只测 APIKeys[0] —— 第 1 把坏了整个后端被拉黑（哪怕第 2 把是好的）；
// 第 1 把好、后面藏的坏 key 要等运行期轮换轮到才暴露（白烧一次真请求）。
// 结论规则：
//   - 任一 key 2xx → 后端可加载；401 的 key 从账户池剔除（运行期生效，不回写配置文件）
//   - 所有 key 都是永久类(401/403/404) → 后端死亡（拉黑；单 key 后端与旧版行为完全一致）
//   - 其余（存在临时失败）→ 保留并告警（同旧版）
//   - 没配 key → 单发一次无鉴权探测（同旧版）
func probeBackend(ctx context.Context, b *upstream.Backend) ProbeResult {
	keys := b.BoundAccount().APIKeys
	if len(keys) == 0 {
		return probeOnce(ctx, b, "")
	}
	res := ProbeResult{}
	var nOK, nTransient int
	var firstBad, firstWarn string
	for _, k := range keys {
		r := probeOnce(ctx, b, k)
		switch {
		case r.OK:
			nOK++
		case r.Permanent:
			if r.KeyInvalid {
				res.Dropped = append(res.Dropped, k)
			}
			if firstBad == "" {
				firstBad = r.Reason
			}
		default:
			nTransient++
			if firstWarn == "" {
				firstWarn = r.Reason
			}
		}
	}
	switch {
	case nOK == 0 && nTransient == 0:
		// 全部 key 永久失败 → 后端级死亡（同旧语义）
		res.Permanent = true
		res.Reason = firstBad
		if len(keys) > 1 {
			res.KeyStat = fmt.Sprintf("%d keys: all dead", len(keys))
		}
	default:
		res.OK = nOK > 0
		res.Reason = firstWarn
		if len(keys) > 1 {
			res.KeyStat = fmt.Sprintf("%d keys: %d ok, %d transient, %d dead",
				len(keys), nOK, nTransient, len(res.Dropped))
		}
	}
	return res
}

// probeOnce 用最小请求探测「后端 + 指定 key」是否真实可用（key="" = 无鉴权后端）。
func probeOnce(ctx context.Context, b *upstream.Backend, key string) ProbeResult {
	model := b.EffectiveModel(time.Now())
	payload, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
		"max_tokens": 16,
		"stream":     false,
	})
	target := strings.TrimRight(b.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return ProbeResult{Reason: "build request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", upstreamUA())
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	// 会话头：按后端配置注入（opencode 系要 x-opencode-session），别再按名字硬编码
	if b.SessionHeader != "" {
		req.Header.Set(b.SessionHeader, uuidHex())
	}
	resp, err := b.Client().Do(req)
	if err != nil {
		// 网络/超时 = 临时问题，**不标记删除**。
		// 10-10 的阿里云 DNS 抖动就是这么被误判的：这里必须返回 Permanent=false。
		return ProbeResult{Reason: "transport: " + err.Error()}
	}
	defer resp.Body.Close()
	body := readSnippet(resp.Body, 512)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return ProbeResult{OK: true}
	case resp.StatusCode == http.StatusUnauthorized:
		return ProbeResult{Permanent: true, KeyInvalid: true, Reason: "401 auth: " + body}
	case resp.StatusCode == http.StatusForbidden:
		// 欠费/额度词表住在 errclass（全局默认词汇）；按家覆盖走 quirk.Classify。
		if errclass.NoCreditBody(body) {
			return ProbeResult{Permanent: true, Reason: "403 no-credit: " + body}
		}
		return ProbeResult{Permanent: true, Reason: "403 forbidden: " + body}
	case resp.StatusCode == http.StatusNotFound:
		return ProbeResult{Permanent: true, Reason: "404 model/endpoint: " + body}
	default:
		// 400/429/5xx 等多为请求侧或临时问题，保留
		return ProbeResult{Reason: "HTTP " + strconv.Itoa(resp.StatusCode) + ": " + body}
	}
}

// manualPause 手动打标签：disabled_until 未到期 = 这个后端还没恢复，不载入。
// 返回 (到期时间, 是否处于暂停中)。解析失败视为未打标签。
//
// ⚠️ 解析失败**视为未打标签**（照搬旧实现）：一个写错格式的时间字段不该
// 悄悄把一个健康后端摘掉 —— 那会让用户以为上游挂了，实际上是他自己少写了一个冒号。
func manualPause(b *upstream.Backend) (time.Time, bool) {
	if b.DisabledUntil == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, b.DisabledUntil)
	if err != nil {
		log.Printf("[config/validate] %s disabled_until parse error: %v (ignored)", b.Name, err)
		return time.Time{}, false
	}
	if time.Now().Before(t) {
		return t, true
	}
	return time.Time{}, false
}

// upstreamUA 探测请求用的 User-Agent。
//
// 默认用 runtime.AppID()（= "gateway/<版本>"）而不是旧实现硬编码的
// 旧项目 UA：项目身份的唯一来源是 runtime.AppName/AppVersion，
// 硬编码一份 UA 就等于开了第二个身份来源 —— 改名/开源时会漏掉它，
// 而 UA 是上游和日志里最容易漏掉的那一处。
func upstreamUA() string { return runtime.EnvString("UPSTREAM_UA", runtime.AppID()) }

// maskKey key 脱敏：只露头尾，中间打码（日志/展示用）。
func maskKey(k string) string {
	if len(k) <= 12 {
		return "****"
	}
	return k[:6] + "…" + k[len(k)-4:]
}

// truncate 截断到 n 字节，避免一条超长报错把日志/黑名单撑爆。
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// readSnippet 读响应体的前 limit 字节并压成单行（探测日志/黑名单里用）。
//
// 换行全部折成空格：不折的话，一条报错里的多行 JSON 会把 [config/validate]
// 的日志行拆成好几行，而黑名单里存的是一个 JSON 字符串 —— 未转义的换行
// 会被 json.Marshal 处理掉，但要人读的时候就是一坨没法 grep 的东西。
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

// uuidHex 生成一个十六进制 uuid（用于注入会话头）。
func uuidHex() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}
