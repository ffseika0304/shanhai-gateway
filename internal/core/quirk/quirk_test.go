package quirk

import (
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"shanhaigateway/internal/core/errclass"
	"shanhaigateway/internal/core/upstream"
)

// ── 测试内注册的假怪癖 ─────────────────────────────────────────
// 注册表是包级全局的：这些假怪癖只在它们的 driver 名被某个后端引用时才生效，
// 所以不会影响本包（或任何别的包）里其它测试的行为。
// 名字带 test- 前缀，避免与将来真上生产的怪癖撞名（撞名 = Register panic）。

type fakeA struct{ OpenAI }

func (fakeA) Name() string { return "test-fake-a" }

type neverClaim struct{ OpenAI }

func (neverClaim) Name() string { return "test-never-claim" }

// Classify 永远 false —— 这就是「(Class,false) = 不归我管」的契约实现。
func (neverClaim) Classify(status int, body string) (errclass.Class, bool) {
	return "", false
}

type dupQuirk struct{ OpenAI }

func (dupQuirk) Name() string { return "test-dup-quirk" }

type raceQuirk struct {
	OpenAI
	n string
}

func (r raceQuirk) Name() string { return r.n }

func init() {
	Register(fakeA{})
	Register(neverClaim{})
}

// ── 1. 默认路径逐位等价（最强回归）──────────────────────────────

func TestGetUnspecifiedOrUnknownReturnsOpenAIDefault(t *testing.T) {
	for _, name := range []string{"", "unknown", "volc-typo", "openai", "test-fake-b"} {
		q := Get(name)
		if q.Name() != "openai-compat" {
			t.Fatalf("Get(%q).Name() = %q, 期望 openai-compat", name, q.Name())
		}
		if _, ok := q.(OpenAI); !ok {
			t.Fatalf("Get(%q) 的动态类型 = %T, 期望 OpenAI（未注册 = 通用行为）", name, q)
		}
	}
	// 单例：miss 之间、以及与包级 defaultQuirk 之间是同一个值，不每次分配
	if Get("") != Get("unknown") {
		t.Fatal("未指定/未注册必须回到同一个 defaultQuirk 单例")
	}
	if Get("") != defaultQuirk {
		t.Fatal(`Get("") 必须就是包级 defaultQuirk`)
	}
}

// ── 2. (Class,false) 契约：不认领 → 调用方走通用默认判定 ─────────

func TestClassifyFalseMeansCallerFallsBackToGeneric(t *testing.T) {
	q := Get("test-never-claim")

	// 怪癖不认，但通用词表认：内核缝② 的 else 分支必须接管（改判 model_unavailable）
	gone := `{"error":"model not found"}`
	if _, ok := q.Classify(400, gone); ok {
		t.Fatal("neverClaim 的 Classify 必须返回 false")
	}
	if !errclass.ModelUnavailableBody(gone) {
		t.Fatal("通用词表应认领这句 —— 否则这条用例测不到回退")
	}

	// 怪癖不认，通用也不认：分类保持 bad_request_400（调用方两个分支都不改判）
	other := `{"error":"tool_choice must be one of auto,none"}`
	if _, ok := q.Classify(400, other); ok {
		t.Fatal("neverClaim 的 Classify 必须对任何 body 都返回 false")
	}
	if errclass.ModelUnavailableBody(other) {
		t.Fatal("通用词表不该认领这句")
	}
}

// ── 3. 注册命中 / 未注册回落 ────────────────────────────────────

func TestRegisterHitAndUnknownMiss(t *testing.T) {
	if _, ok := Get("test-fake-a").(fakeA); !ok {
		t.Fatalf(`Get("test-fake-a") = %T, 期望命中新注册的 fakeA`, Get("test-fake-a"))
	}
	for _, name := range []string{"", "test-fake-b", "not-registered"} {
		if _, ok := Get(name).(OpenAI); !ok {
			t.Fatalf(`Get(%q) = %T, 未注册名必须回落 OpenAI 默认`, name, Get(name))
		}
	}
}

// ── 4. 重复注册同名 → panic ─────────────────────────────────────

func TestRegisterDuplicatePanics(t *testing.T) {
	Register(dupQuirk{})
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("重复注册同一 Name 必须 panic（防两家抢一个名字）")
		}
		if msg := fmt.Sprint(r); !strings.Contains(msg, "test-dup-quirk") {
			t.Fatalf("panic 信息应带上撞名的名字，got %q", msg)
		}
	}()
	Register(dupQuirk{})
}

// ── 5. 默认实现 Decorate 是 no-op ───────────────────────────────

func TestOpenAIDecorateIsNoOp(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "http://upstream.invalid/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer k")
	snapHeader := req.Header.Clone()
	snapURL := req.URL.String()

	OpenAI{}.Decorate(req, &upstream.Backend{Name: "be", Driver: ""})

	if !reflect.DeepEqual(req.Header, snapHeader) {
		t.Fatalf("no-op 被破了：header 前=%v 后=%v", snapHeader, req.Header)
	}
	if req.URL.String() != snapURL {
		t.Fatalf("no-op 被破了：url %q → %q", snapURL, req.URL.String())
	}
	if req.Method != http.MethodPost {
		t.Fatalf("no-op 被破了：method %q", req.Method)
	}
}

// ── 6. 默认实现 Classify 只认 400/404 + 模型下架词 ───────────────

func TestOpenAIClassifyClaimsOnlyModelGoneOn400404(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantClass errclass.Class
		wantOK    bool
	}{
		{"400 + 通用下架词表", 400, `{"error":"model not found"}`, errclass.ModelUnavailable, true},
		{"404 + 通用下架词表", 404, "Error: no such model `abc`", errclass.ModelUnavailable, true},
		{"400 大小写混合也算", 400, "Model_Not_Found", errclass.ModelUnavailable, true},
		// ⚠️ 429/5xx 一律不认领：不能因为问了怪癖就把它们改判
		{"429 撞文案也不认领", 429, "model not found", "", false},
		{"500 撞文案也不认领", 500, "model not found", "", false},
		{"401 撞文案也不认领", 401, "unknown model", "", false},
		{"400 无关文案不认领", 400, `{"error":"tool_choice illegal value"}`, "", false},
		// 403 欠费词表归探测层（errclass.NoCreditBody），不归缝②
		{"403 欠费文案不认领", 403, "insufficient quota", "", false},
	}
	for _, c := range cases {
		got, ok := OpenAI{}.Classify(c.status, c.body)
		if ok != c.wantOK || got != c.wantClass {
			t.Errorf("%s: Classify(%d,%q) = (%q,%v), 期望 (%q,%v)",
				c.name, c.status, c.body, got, ok, c.wantClass, c.wantOK)
		}
	}
}

// ── 7. 默认实现 ParseUsage：OpenAI usage + cached_tokens ────────

func TestOpenAIParseUsage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Usage
	}{
		{
			name: "标准 OpenAI usage（含 cached_tokens）",
			body: `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3}}}`,
			want: Usage{Prompt: 10, Completion: 5, Total: 15, CachedPrompt: 3, OK: true},
		},
		{
			name: "缺 total → p+c（与 proxy.parseUsage 同口径）",
			body: `{"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
			want: Usage{Prompt: 7, Completion: 2, Total: 9, OK: true},
		},
		{
			name: "缺 cached → 0",
			body: `{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			want: Usage{Prompt: 1, Completion: 1, Total: 2, OK: true},
		},
		{
			name: "没有 usage 键 → 不认领",
			body: `{"choices":[{"message":{"content":"hi"}}]}`,
			want: Usage{},
		},
		{
			name: "usage 为 null → 不认领",
			body: `{"usage":null}`,
			want: Usage{},
		},
		{name: "垃圾 body → 不认领", body: "not-json-at-all", want: Usage{}},
		{name: "空 body → 不认领", body: "", want: Usage{}},
	}
	for _, c := range cases {
		base := OpenAI{}
		if got := base.ParseUsage([]byte(c.body)); got != c.want {
			t.Errorf("%s: ParseUsage(%s) = %+v, 期望 %+v", c.name, c.body, got, c.want)
		}
	}
}

// ── 13. 并发：多读一写，-race 干净 ──────────────────────────────

func TestGetConcurrentReadersWhileRegistering(t *testing.T) {
	const readers = 16
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if q := Get(""); q.Name() != "openai-compat" {
					t.Errorf(`Get("").Name() = %q`, q.Name())
					return
				}
				if _, ok := Get("test-race-miss-" + strconv.Itoa(j%3)).(OpenAI); !ok {
					t.Errorf("未注册名在并发注册下也不许被抢走")
					return
				}
				if q := Get("test-fake-a"); q.Name() != "test-fake-a" {
					t.Errorf(`Get("test-fake-a").Name() = %q`, q.Name())
					return
				}
			}
		}(i)
	}
	// 一个写者：并发注册 16 个新名字（读侧同时在 Get）
	var wgw sync.WaitGroup
	for w := 0; w < 4; w++ {
		wgw.Add(1)
		go func(w int) {
			defer wgw.Done()
			for k := 0; k < 4; k++ {
				Register(raceQuirk{n: fmt.Sprintf("test-race-%d-%d", w, k)})
			}
		}(w)
	}
	wg.Wait()
	wgw.Wait()

	if _, ok := Get("test-race-0-0").(raceQuirk); !ok {
		t.Fatal("并发注册完成后应能命中")
	}
	if q := Get(""); q.Name() != "openai-compat" {
		t.Fatalf("默认实现不许被注册冲掉，Get(%q)", "")
	}
}

// ── 12. 依赖方向：core/quirk 只许 import errclass/upstream/标准库 ─

func TestDependencyDirectionOnlyCoreAndStdlib(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", "{{join .Imports \"\\n\"}}", ".").Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			t.Skip("PATH 里没有 go，跳过依赖方向自检")
		}
		t.Fatalf("go list: %v", err)
	}
	allowed := map[string]bool{
		"shanhaigateway/internal/core/errclass": true,
		"shanhaigateway/internal/core/upstream": true,
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		imp := strings.TrimSpace(line)
		if imp == "" {
			continue
		}
		if strings.HasPrefix(imp, "shanhaigateway/") && !allowed[imp] {
			t.Errorf("core/quirk 不许 import %q：只许 errclass / upstream / 标准库", imp)
		}
	}
	// 这两个是接口签名用到的依赖，必须在（防有人把接口搬走后忘了依赖）
	for _, must := range []string{
		"shanhaigateway/internal/core/errclass",
		"shanhaigateway/internal/core/upstream",
	} {
		if !strings.Contains(string(out), must) {
			t.Errorf("go list 输出里缺少 %q", must)
		}
	}
}
