package errclass

import (
	"strings"
	"testing"
)

// 整表钉死：任何新增/改名的类别都会在这里暴露。
// 也顺带保证 List 和各常量不脱节。
func TestListCoversEveryConstant(t *testing.T) {
	want := []Class{
		Network, Timeout, FirstTokenTimeout, RateLimit429, Auth401, Forbidden403,
		BadRequest400, NotFound404, UpstreamTimeout, ServerError5xx, ErrorBody,
		NoContent, EmptyOutput, MidStreamFail, ModelUnavailable,
		SkipCooldown, SkipCapacity, SkipCircuit, SkipDisabled,
	}
	if len(List) != len(want) {
		t.Fatalf("List 长度 = %d, 期望 %d", len(List), len(want))
	}
	for i := range want {
		if List[i] != want[i] {
			t.Errorf("List[%d] = %q, 期望 %q", i, List[i], want[i])
		}
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status int
		want   Class
	}{
		{401, Auth401},
		{403, Forbidden403},
		{429, RateLimit429},
		{404, NotFound404},
		{408, UpstreamTimeout},
		{504, UpstreamTimeout},
		{400, BadRequest400},
		{500, ServerError5xx},
		{502, ServerError5xx},
		{503, ServerError5xx},
		{599, ServerError5xx},
		// 兜底：说不清的按最坏处理
		{200, ServerError5xx},
		{204, ServerError5xx},
		{301, ServerError5xx},
		{402, ServerError5xx},
		{418, ServerError5xx},
		{0, ServerError5xx},
	}
	for _, c := range cases {
		if got := ClassifyStatus(c.status); got != c.want {
			t.Errorf("ClassifyStatus(%d) = %q, 期望 %q", c.status, got, c.want)
		}
	}
}

// 字符串值是持久化格式，写进了指标库和看板：一旦改动就是历史数据断层。
func TestClassStringValuesAreFrozen(t *testing.T) {
	want := map[Class]string{
		Network: "network", Timeout: "timeout",
		FirstTokenTimeout: "first_token_timeout",
		RateLimit429:      "rate_limit_429",
		Auth401:           "auth_401", Forbidden403: "forbidden_403",
		BadRequest400: "bad_request_400", NotFound404: "not_found_404",
		UpstreamTimeout: "upstream_timeout", ServerError5xx: "server_error_5xx",
		ErrorBody: "error_body", NoContent: "no_content", EmptyOutput: "empty_output",
		MidStreamFail: "mid_stream_fail", ModelUnavailable: "model_unavailable",
		SkipCooldown: "skip_cooldown", SkipCapacity: "skip_capacity",
		SkipCircuit: "skip_circuit", SkipDisabled: "skip_disabled",
	}
	for c, s := range want {
		if string(c) != s {
			t.Errorf("Class %v 的字符串值 = %q, 期望 %q", c, string(c), s)
		}
	}
}

func TestModelUnavailableBody(t *testing.T) {
	keywords := []string{
		"model is unavailable",
		"model not found",
		"model_not_found",
		"model does not exist",
		"unknown model",
		"no such model",
	}
	for _, k := range keywords {
		body := `{"error":{"message":"Error code: 400 - ` + k + ` - Requested model"}}`
		if !ModelUnavailableBody(body) {
			t.Errorf("关键词 %q 未命中", k)
		}
		// 大小写不敏感（源实现里 strings.ToLower 是刻意的）
		if !ModelUnavailableBody(strings.ToUpper(body)) {
			t.Errorf("关键词 %q 全大写未命中", k)
		}
		if !ModelUnavailableBody(`{"error":"` + strings.Title(k) + `"}`) {
			t.Errorf("关键词 %q 首字母大写未命中", k)
		}
	}
}

func TestModelUnavailableBodyNegative(t *testing.T) {
	neg := []string{
		"",
		`{"error":{"message":"invalid api key"}}`,
		`{"error":{"message":"context length exceeded"}}`,
		`{"error":{"message":"rate limit reached for gpt-4"}}`,
		// 看着像但不是关键词，不能误伤
		`{"error":{"message":"model parameter is required"}}`,
		`{"error":{"message":"the model you requested is overloaded, please retry"}}`,
	}
	for _, body := range neg {
		if ModelUnavailableBody(body) {
			t.Errorf("报文 %q 不应命中", body)
		}
	}
}

func TestIsSkip(t *testing.T) {
	skip := []Class{SkipCooldown, SkipCapacity, SkipCircuit, SkipDisabled}
	for _, c := range skip {
		if !c.IsSkip() {
			t.Errorf("%q 应是跳过类", c)
		}
	}
	for _, c := range List {
		if !contains(skip, c) && c.IsSkip() {
			t.Errorf("%q 不该是跳过类", c)
		}
	}
}

func contains(s []Class, c Class) bool {
	for _, x := range s {
		if x == c {
			return true
		}
	}
	return false
}

// CountsAsFault：4 个 soft 类不记账，其余非 skip 类都记账，skip_* 全不记。
func TestCountsAsFault(t *testing.T) {
	soft := []Class{BadRequest400, EmptyOutput, NoContent, Network}
	for _, c := range soft {
		if CountsAsFault(c) {
			t.Errorf("%q 是 soft 类，不该计入故障", c)
		}
	}
	for _, c := range List {
		if contains(soft, c) {
			continue
		}
		want := !c.IsSkip()
		if got := CountsAsFault(c); got != want {
			t.Errorf("CountsAsFault(%q) = %v, 期望 %v", c, got, want)
		}
	}
}

// ⚠️ 2026-10-06 的教训：模型下架回 400 被当客户端的锅 → 不涨 failStreak、不跳闸
// → 静默黑洞空转 4 天半。这条回归要一直留着。
func TestModelUnavailableCountsAsFault(t *testing.T) {
	if !CountsAsFault(ModelUnavailable) {
		t.Fatal("模型下架是后端的锅，必须计入故障（否则退化成静默黑洞）")
	}
	if ClientFault(ModelUnavailable) {
		t.Fatal("模型下架不能被当成客户端的错")
	}
	if !ClientFault(BadRequest400) {
		t.Fatal("400 是客户端的错")
	}
}

func TestCooldownKindOf(t *testing.T) {
	cases := map[Class]CooldownKind{
		Network:           CooldownTransient,
		Timeout:           CooldownTransient,
		RateLimit429:      CooldownQuotaExponential,
		Auth401:           CooldownQuotaExponential,
		Forbidden403:      CooldownQuotaExponential,
		BadRequest400:     CooldownNone,
		NotFound404:       CooldownNone,
		UpstreamTimeout:   CooldownNone,
		ServerError5xx:    CooldownNone,
		ErrorBody:         CooldownNone,
		NoContent:         CooldownNone,
		EmptyOutput:       CooldownNone,
		MidStreamFail:     CooldownNone,
		ModelUnavailable:  CooldownNone,
		FirstTokenTimeout: CooldownNone,
	}
	for c, want := range cases {
		if got := CooldownKindOf(c); got != want {
			t.Errorf("CooldownKindOf(%q) = %v, 期望 %v", c, got, want)
		}
	}
	for _, c := range []Class{SkipCooldown, SkipCapacity, SkipCircuit, SkipDisabled} {
		if got := CooldownKindOf(c); got != CooldownNone {
			t.Errorf("跳过类 %q 不该配冷却，得到 %v", c, got)
		}
	}
}

func TestKeyRotatable(t *testing.T) {
	rot := []Class{Auth401, Forbidden403, RateLimit429, ServerError5xx, UpstreamTimeout}
	for _, c := range rot {
		if !KeyRotatable(c) {
			t.Errorf("%q 应可换 key", c)
		}
	}
	for _, c := range List {
		if contains(rot, c) {
			continue
		}
		if KeyRotatable(c) {
			t.Errorf("%q 不该换 key（换也是白费力气）", c)
		}
	}
}

// BreaksKeyLoop 必须与搬入前两段并列 if 的合成结果一致。
func TestBreaksKeyLoop(t *testing.T) {
	for _, c := range List {
		// 搬入前的原表达式，逐类复算一遍
		want := c == BadRequest400 || c == NotFound404 || c == ModelUnavailable ||
			!KeyRotatable(c)
		if got := BreaksKeyLoop(c); got != want {
			t.Errorf("BreaksKeyLoop(%q) = %v, 期望 %v", c, got, want)
		}
	}
	for _, c := range []Class{Auth401, Forbidden403, ServerError5xx, UpstreamTimeout} {
		if BreaksKeyLoop(c) {
			t.Errorf("%q 应该继续换 key，而不是直接换后端", c)
		}
	}
	for _, c := range []Class{BadRequest400, NotFound404, ModelUnavailable, Network, Timeout} {
		if !BreaksKeyLoop(c) {
			t.Errorf("%q 应该直接换后端", c)
		}
	}
}

func TestImmediateCooldown(t *testing.T) {
	if !ImmediateCooldown(RateLimit429) {
		t.Error("429 应一票否决，立刻跳出 key 循环")
	}
	for _, c := range List {
		if c == RateLimit429 {
			continue
		}
		if ImmediateCooldown(c) {
			t.Errorf("只有 429 一票否决，%q 不该是", c)
		}
	}
}

func TestSoftFailureAndTransienceAndSlowProbe(t *testing.T) {
	if !SoftFailure(EmptyOutput) || !SoftFailure(NoContent) {
		t.Error("空气泡类是软失败")
	}
	for _, c := range []Class{EmptyOutput, NoContent} {
		if Transience(c) || SlowProbeTrigger(c) {
			t.Errorf("%q 不该被当成网络抖动/慢探测", c)
		}
	}
	if !Transience(Network) || !Transience(Timeout) {
		t.Error("network/timeout 是临时抖动")
	}
	if Transience(FirstTokenTimeout) {
		t.Error("首字超时有自己的慢探测窗口，不走固定冷却")
	}
	if !SlowProbeTrigger(FirstTokenTimeout) {
		t.Error("首字超时喂慢探测窗")
	}
	for _, c := range List {
		if c == FirstTokenTimeout {
			continue
		}
		if SlowProbeTrigger(c) {
			t.Errorf("只有首字超时喂慢探测窗，%q 不该", c)
		}
	}
}

// ⚠️ 2026-10-03：客户端断连曾被误罚后端，一次断连让全体在飞后端同时冷却，
// 候选清空 → 全网 1 分钟 503，共 1720 次。这条规则不许消失。
//
// 2026-10-11：原先这条保障挂在 errclass.ClientGoneSkipCooldown() 上，而那是个
// 恒返回 true 的无参开关（clientGone && f() ≡ clientGone，纯噪音），已删除。
// **规则本身一个字没改**，判定现在直接写在 upstream.noteFailure 的
// `if clientGone { ... return }` 里；这条测试钉的是 errclass 这一侧的配套事实：
// 断连时会被看到的那几类，确实全是 Transience（否则上游那边根本走不到那条分支）。
// 端到端的行为回归见 upstream_test.go 的 TestClientGoneSkipsCooldown。
func TestClientGoneSeesOnlyTransientClasses(t *testing.T) {
	// 客户端断连时会被看到的那几类：必须落在 Transience 分支里，
	// upstream.noteFailure 才会走到 `if clientGone { return }` 那条豁免。
	for _, c := range []Class{Network, Timeout} {
		if !Transience(c) {
			t.Errorf("客户端断连表现为 %q，它必须落在 Transience 分支里才会被 clientGone 豁免", c)
		}
	}
	// context canceled 在兜底隔离里也不记账（断连是我方的锅，不是上游背锅）。
	// 注意 timeout 不在此列：它是真上游超时，该记的账要记。
	if CountsAsFault(Network) {
		t.Error("network 断连不该计入兜底隔离")
	}
	if !CountsAsFault(Timeout) {
		t.Error("timeout 是真上游超时，仍该计入兜底隔离")
	}
}

func TestCooldownKindString(t *testing.T) {
	if CooldownNone.String() != "none" ||
		CooldownTransient.String() != "transient" ||
		CooldownQuotaExponential.String() != "quota_exponential" {
		t.Fatal("CooldownKind.String 意外")
	}
}
