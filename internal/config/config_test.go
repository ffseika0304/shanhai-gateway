package config

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shanhai-gateway/internal/core/upstream"
	"shanhai-gateway/internal/runtime"
)

// ── 测试脚手架 ──────────────────────────────────────────────
// 本包的测试**绝不真打上游**：探测结论全部用假的 Prober 注入。
// 一来 CI 里不该有出网依赖，二来「瞬时故障不拉黑」这类规则要能稳定复现 ——
// 真去打 DNS，谁也说不准今天抖不抖。

func TestMain(m *testing.M) {
	// 校验日志每个后端好几行，不收走的话 -v 输出全是它。
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// testPaths 造一套指向临时目录的 Paths。
func testPaths(t *testing.T) *runtime.Paths {
	t.Helper()
	root := t.TempDir()
	p := &runtime.Paths{
		DataDir:      filepath.Join(root, "data"),
		LogDir:       filepath.Join(root, "data", "logs"),
		ConfigDir:    filepath.Join(root, "conf"),
		BackendsFile: filepath.Join(root, "conf", "backends.json"),
		BlockedFile:  filepath.Join(root, "conf", "backends.blocked.json"),
	}
	return p
}

func writeConf(t *testing.T, p *runtime.Paths, content string) {
	t.Helper()
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(p.BackendsFile, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// oneBackendJSON 造一份只含一个后端的配置。
func oneBackendJSON(name string) string {
	return `{"backends":[{"name":"` + name + `","base_url":"http://upstream.invalid","model":"m","enabled":true}]}`
}

func names(bs []*upstream.Backend) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return out
}

func hasName(bs []*upstream.Backend, want string) bool {
	for _, b := range bs {
		if b.Name == want {
			return true
		}
	}
	return false
}

// ── Load ────────────────────────────────────────────────────

func TestLoadFromFile(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p, oneBackendJSON("alpha"))

	snap, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snap.Backends) != 1 || snap.Backends[0].Name != "alpha" {
		t.Fatalf("后端 = %v, 期望 [alpha]", names(snap.Backends))
	}
	if snap.LoadedAt.IsZero() {
		t.Error("LoadedAt 不该为零值")
	}
	// Init 必须跑过：并发位/权重/stats/freeCh 都在
	b := snap.Backends[0]
	if b.Concurrency != 16 || b.Weight != 1.0 || b.Stats() == nil {
		t.Fatalf("Init 未执行：conc=%d weight=%v", b.Concurrency, b.Weight)
	}
}

// 新格式：accounts + backends，key 池共享。
func TestLoadNewFormatAccounts(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p, `{
      "accounts":[{"name":"main","base_url":"http://acct.invalid","api_keys":["k1","k2"]}],
      "backends":[{"name":"b1","account":"main","model":"m","enabled":true},
                  {"name":"b2","account":"main","model":"m","enabled":true}]
    }`)

	snap, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snap.Backends) != 2 {
		t.Fatalf("后端数 = %d, 期望 2", len(snap.Backends))
	}
	if snap.Backends[0].BoundAccount() != snap.Backends[1].BoundAccount() {
		t.Error("同账号后端应共享同一个 key 池指针")
	}
	if snap.Backends[0].BaseURL != "http://acct.invalid" {
		t.Errorf("base_url 应从账号继承，得到 %q", snap.Backends[0].BaseURL)
	}
}

// 旧格式：顶层数组。
func TestLoadLegacyArrayFormat(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p, `[{"name":"old","base_url":"http://old.invalid","enabled":true}]`)

	snap, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snap.Backends) != 1 || snap.Backends[0].Name != "old" {
		t.Fatalf("后端 = %v, 期望 [old]", names(snap.Backends))
	}
}

// env 注入优先于文件：两者都在时，取 env 那份。
//
// ⚠️ 这与旧实现（文件覆盖 env）相反，是本层有意变更，理由见 Load 的注释。
// 改动这个行为时，请连带看 Load 上面那段注释 —— 它记的是为什么反过来。
func TestLoadEnvWinsOverFile(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p, oneBackendJSON("from-file"))
	t.Setenv(envBackends, oneBackendJSON("from-env"))

	snap, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snap.Backends) != 1 || snap.Backends[0].Name != "from-env" {
		t.Fatalf("后端 = %v, 期望 [from-env]（env 注入应优先于文件）", names(snap.Backends))
	}
}

// env 注入是完整来源：文件里有的后端不该混进来。
func TestLoadEnvDoesNotMergeWithFile(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p, `{"backends":[
      {"name":"file-only","base_url":"http://a.invalid","enabled":true}]}`)
	t.Setenv(envBackends, `{"backends":[{"name":"env-only","base_url":"http://b.invalid","enabled":true}]}`)

	snap, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snap.Backends) != 1 || snap.Backends[0].Name != "env-only" {
		t.Fatalf("后端 = %v, 期望只有 [env-only]，两份配置不合并", names(snap.Backends))
	}
}

// 两者皆空 → 明确错误，且消息要能指出去怎么修。
func TestLoadBothSourcesEmptyReturnsClearError(t *testing.T) {
	p := testPaths(t)
	t.Setenv(envBackends, "")

	_, err := Load(p)
	if err == nil {
		t.Fatal("两者皆空时应当返回错误，而不是给一份空快照")
	}
	msg := err.Error()
	for _, want := range []string{envBackends, p.BackendsFile} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误消息没提到 %q：%s", want, msg)
		}
	}
}

// 空文件等价于没配。
func TestLoadEmptyFileReturnsError(t *testing.T) {
	p := testPaths(t)
	t.Setenv(envBackends, "")
	writeConf(t, p, "")

	if _, err := Load(p); err == nil {
		t.Fatal("空文件应当返回错误")
	}
}

// 两种格式都不认 → 错误里要带上原始解析错误。
func TestLoadMalformedJSONReturnsError(t *testing.T) {
	p := testPaths(t)
	t.Setenv(envBackends, "")
	writeConf(t, p, `{"backends": [ this is not json `)

	_, err := Load(p)
	if err == nil {
		t.Fatal("语法错误的 JSON 应当返回错误（不能静默给一份空配置）")
	}
	if !strings.Contains(err.Error(), "后端配置") {
		t.Errorf("错误消息应说明是后端配置解析失败：%v", err)
	}
}

// nil Paths 不 panic。
func TestLoadNilPathsReturnsError(t *testing.T) {
	if _, err := Load(nil); err == nil {
		t.Fatal("nil Paths 应当返回错误而不是 panic")
	}
}

// ── Load 不创建任何文件 ──────────────────────────────────────

// Load 是纯读：不建目录、不建文件、不写默认配置。
// 这条是整个包可测性的前提 —— 只要 Load 碰了磁盘，所有依赖它的测试
// 都得往临时目录里收拾残局，而真实路径上的测试就永远不敢跑。
func TestLoadCreatesNothing(t *testing.T) {
	p := testPaths(t)
	t.Setenv(envBackends, "")
	// 故意**不**创建 ConfigDir

	if _, err := Load(p); err == nil {
		t.Fatal("没有配置时应当返回错误")
	}

	for _, path := range []string{p.ConfigDir, p.DataDir, p.LogDir, p.BlockedFile} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("Load 不该创建 %s（stat err=%v）", path, err)
		}
	}
}

// 有配置文件时，Load 也只读不写（黑名单不该凭空出现）。
func TestLoadDoesNotCreateBlocklist(t *testing.T) {
	p := testPaths(t)
	t.Setenv(envBackends, "")
	writeConf(t, p, oneBackendJSON("alpha"))

	if _, err := Load(p); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := os.Lstat(p.BlockedFile); !os.IsNotExist(err) {
		t.Errorf("Load 不该创建黑名单文件（stat err=%v）", err)
	}
}

// 配置目录整个不该被创建出来。
func TestLoadDoesNotCreateConfigDir(t *testing.T) {
	p := testPaths(t)
	t.Setenv(envBackends, oneBackendJSON("alpha"))
	// ConfigDir 根本不存在，env 注入也不该去建它

	if _, err := Load(p); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := os.Lstat(p.ConfigDir); !os.IsNotExist(err) {
		t.Errorf("env 注入时不该创建 %s（stat err=%v）", p.ConfigDir, err)
	}
}

// ── Store / ReloadIfChanged ────────────────────────────────

func TestStoreSnapshotIsInitialLoad(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p, oneBackendJSON("alpha"))

	st, err := NewStore(p)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if got := st.Snapshot(); got == nil || len(got.Backends) != 1 || got.Backends[0].Name != "alpha" {
		t.Fatalf("初始快照 = %v", names(got.Backends))
	}
	if st.Paths() != p {
		t.Error("Paths() 应返回绑定的 Paths")
	}
}

// 文件没改 → 不重载。
func TestReloadIfChangedNoopWhenUnmodified(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p, oneBackendJSON("alpha"))

	st, err := NewStore(p)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	before := st.Snapshot()

	for i := 0; i < 3; i++ {
		changed, err := st.ReloadIfChanged()
		if err != nil {
			t.Fatalf("第 %d 次 ReloadIfChanged 报错：%v", i+1, err)
		}
		if changed {
			t.Fatalf("第 %d 次：文件没改却重载了", i+1)
		}
	}
	if st.Snapshot() != before {
		t.Error("未改动时快照指针不该被换掉（每次请求都读它，无谓的替换会让所有人重新取）")
	}
}

// 文件改了 → 重载，新配置生效。
func TestReloadIfChangedPicksUpEdit(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p, oneBackendJSON("alpha"))

	st, err := NewStore(p)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	writeConf(t, p, oneBackendJSON("bravo"))
	// mtime 精度兜底：某些文件系统上同一秒内的两次写拿到同一个 mtime，
	// 显式推到未来一秒，保证「改了」这件事在测试里是可观测的。
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p.BackendsFile, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	changed, err := st.ReloadIfChanged()
	if err != nil {
		t.Fatalf("ReloadIfChanged: %v", err)
	}
	if !changed {
		t.Fatal("文件改了却没重载")
	}
	if got := names(st.Snapshot().Backends); len(got) != 1 || got[0] != "bravo" {
		t.Fatalf("重载后 = %v, 期望 [bravo]", got)
	}
}

// ⚠️ 重载失败必须保留旧配置 —— 不能因为一次手滑的编辑让全站路由崩掉。
// 这条是本包最重要的一条不变量，单独一个用例盯死它。
func TestReloadFailureKeepsOldConfig(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p, oneBackendJSON("alpha"))

	st, err := NewStore(p)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	before := st.Snapshot()

	// 模拟「手滑打到一半」：语法错误
	writeConf(t, p, `{"backends": [ {"name": "brok`)
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p.BackendsFile, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	changed, err := st.ReloadIfChanged()
	if err == nil {
		t.Fatal("坏 JSON 应当返回错误")
	}
	if changed {
		t.Fatal("坏 JSON 不该被算作「已重载」")
	}
	// 关键断言：旧配置一个字节都没变
	after := st.Snapshot()
	if after != before {
		t.Error("重载失败时快照指针不该被换掉")
	}
	if got := names(after.Backends); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("重载失败后配置被改了：%v, 期望仍是 [alpha]", got)
	}

	// 改回合法配置后应当能正常重载（没有被卡死在失败态）
	writeConf(t, p, oneBackendJSON("charlie"))
	later := time.Now().Add(4 * time.Second)
	if err := os.Chtimes(p.BackendsFile, later, later); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	changed, err = st.ReloadIfChanged()
	if err != nil || !changed {
		t.Fatalf("修好后应能重载：changed=%v err=%v", changed, err)
	}
	if got := names(st.Snapshot().Backends); len(got) != 1 || got[0] != "charlie" {
		t.Fatalf("恢复后 = %v, 期望 [charlie]", got)
	}
}

// 文件被删掉 → 保留旧配置并报错。
//
// ⚠️ 运维手滑 `rm backends.json` 是完全现实的操作。如果这里当成「没配置」
// 给一份空快照，路由会瞬间没有任何后端可用 —— 一次误删换全站 503。
func TestReloadFileDeletedKeepsOldConfig(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p, oneBackendJSON("alpha"))

	st, err := NewStore(p)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := os.Remove(p.BackendsFile); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	changed, err := st.ReloadIfChanged()
	if err == nil {
		t.Fatal("文件消失应当返回错误")
	}
	if changed {
		t.Fatal("文件消失不该算作已重载")
	}
	if got := names(st.Snapshot().Backends); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("文件消失后配置被改了：%v, 期望仍是 [alpha]", got)
	}
}

// env 注入时没有文件可盯：重载是安全的空操作。
func TestReloadIsNoopWhenEnvInjected(t *testing.T) {
	p := testPaths(t)
	t.Setenv(envBackends, oneBackendJSON("alpha"))

	st, err := NewStore(p)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	for i := 0; i < 3; i++ {
		changed, err := st.ReloadIfChanged()
		if err != nil {
			t.Fatalf("env 注入时 ReloadIfChanged 不该报错：%v", err)
		}
		if changed {
			t.Fatal("env 注入时不该有重载（没有文件可盯）")
		}
	}
}

// 旧快照不受重载影响：拿在手里的 Snapshot 不会凭空变形。
func TestOldSnapshotUnaffectedByReload(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p, oneBackendJSON("alpha"))

	st, err := NewStore(p)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	held := st.Snapshot()

	writeConf(t, p, oneBackendJSON("bravo"))
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(p.BackendsFile, future, future); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if _, err := st.ReloadIfChanged(); err != nil {
		t.Fatalf("ReloadIfChanged: %v", err)
	}

	if got := names(held.Backends); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("已持有的快照被重载改动了：%v, 期望仍是 [alpha]", got)
	}
	if got := names(st.Snapshot().Backends); len(got) != 1 || got[0] != "bravo" {
		t.Fatalf("新快照 = %v, 期望 [bravo]", got)
	}
}

// ── 并发：一边重载一边读 ────────────────────────────────────
//
// 每次请求都会读配置，同时后台可能正在重载。用 -race 跑这个用例，
// 锁要是拿错了，这里立刻炸数据竞争。

func TestConcurrentReloadAndRead(t *testing.T) {
	p := testPaths(t)
	writeConf(t, p, oneBackendJSON("alpha"))

	st, err := NewStore(p)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	const readers, rounds = 8, 300
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 读者：模拟请求路径每次都取快照
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap := st.Snapshot()
				if snap == nil || len(snap.Backends) == 0 {
					t.Errorf("快照不该为空")
					return
				}
				// 后端对象本身也要能安全读（统计是 atomic 的）
				for _, b := range snap.Backends {
					_ = b.Name
					_ = b.SkipReason(time.Now())
				}
			}
		}()
	}

	// 写者：反复改配置触发重载
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < rounds/10; i++ {
			writeConf(t, p, oneBackendJSON("alpha"))
			writeConf(t, p, oneBackendJSON("bravo"))
			tm := time.Now().Add(time.Duration(i+1) * time.Second)
			if err := os.Chtimes(p.BackendsFile, tm, tm); err != nil {
				t.Errorf("Chtimes: %v", err)
				return
			}
			if _, err := st.ReloadIfChanged(); err != nil {
				t.Errorf("ReloadIfChanged: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}

// ── Validate：探测结论 ──────────────────────────────────────

// 永久失效（欠费/额度耗尽/key 失效/模型不存在）→ 写黑名单 + 不载入。
func TestValidatePermanentFailureGoesToBlocklist(t *testing.T) {
	p := testPaths(t)
	snap := mustLoad(t, p, `{"backends":[
      {"name":"dead","base_url":"http://dead.invalid","model":"m","enabled":true},
      {"name":"alive","base_url":"http://alive.invalid","model":"m","enabled":true}]}`)

	kept, err := Validate(p, snap, ProberFunc(func(_ context.Context, b *upstream.Backend) ProbeResult {
		if b.Name == "dead" {
			return ProbeResult{Permanent: true, Reason: "403 no-credit: 欠费"}
		}
		return ProbeResult{OK: true}
	}))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(kept) != 1 || kept[0].Name != "alive" {
		t.Fatalf("保留 = %v, 期望只剩 [alive]", names(kept))
	}

	blocked := LoadBlocklist(p.BlockedFile)
	e, ok := blocked["dead"]
	if !ok {
		t.Fatalf("dead 应当进黑名单，实际黑名单 = %v", blocked)
	}
	if !strings.Contains(e.Reason, "欠费") {
		t.Errorf("黑名单应保留原因摘要，得到 %q", e.Reason)
	}
	if at, err := time.Parse(time.RFC3339, e.At); err != nil || at.IsZero() {
		t.Errorf("黑名单条目应带 RFC3339 时间，得到 %q", e.At)
	}
	if _, bad := blocked["alive"]; bad {
		t.Error("健康后端不该进黑名单")
	}
}

// ⚠️ 瞬时故障（网络抖动 / DNS）→ 只 warn，**不拉黑**。
//
// 2026-10-10 阿里云 DNS 抖动正是这类。若把它归进永久类，全站后端会被一次性
// 拉黑，一个几秒钟的外部抖动换来一个要人工逐条清理的黑名单文件。
// 外部的 Permanent:false / Reason 是 transport/DNS 的结论。
func TestValidateTransientFailureOnlyWarns(t *testing.T) {
	p := testPaths(t)
	snap := mustLoad(t, p, `{"backends":[
      {"name":"dns-jitter","base_url":"http://a.invalid","model":"m","enabled":true},
      {"name":"refused","base_url":"http://b.invalid","model":"m","enabled":true},
      {"name":"throttled","base_url":"http://c.invalid","model":"m","enabled":true}]}`)

	kept, err := Validate(p, snap, ProberFunc(func(_ context.Context, b *upstream.Backend) ProbeResult {
		switch b.Name {
		case "dns-jitter":
			return ProbeResult{Reason: `transport: lookup api.example.com: no such host`}
		case "refused":
			return ProbeResult{Reason: `transport: connection refused`}
		default:
			return ProbeResult{Reason: "HTTP 429: rate limited"}
		}
	}))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(kept) != 3 {
		t.Fatalf("瞬时故障不该摘后端：保留 = %v", names(kept))
	}

	if _, err := os.Lstat(p.BlockedFile); !os.IsNotExist(err) {
		t.Errorf("全部瞬时故障时不该创建黑名单文件（stat err=%v）", err)
	}
}

// 永久类必须是「上游明确说废了」，401/403/404 各覆盖一条。
//
// ⚠️ 只配了这一个后端，所以它被判死后 kept 必然为空、Validate 必然返回错误
// —— 这与旧实现在同一位置 log.Fatal 的判断口径一致。
func TestValidatePermanentClasses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result ProbeResult
	}{
		{"401 key 失效", ProbeResult{Permanent: true, KeyInvalid: true, Reason: "401 auth"}},
		{"403 欠费", ProbeResult{Permanent: true, Reason: "403 no-credit"}},
		{"404 模型不存在", ProbeResult{Permanent: true, Reason: "404 model/endpoint"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			snap := mustLoad(t, p, oneBackendJSON("only"))

			kept, err := Validate(p, snap, ProberFunc(
				func(context.Context, *upstream.Backend) ProbeResult { return tc.result }))
			if err == nil {
				t.Fatal("只有一个后端且被判死时应当返回错误")
			}
			if len(kept) != 0 {
				t.Fatalf("永久失效不该被载入：%v", names(kept))
			}
			if _, ok := LoadBlocklist(p.BlockedFile)["only"]; !ok {
				t.Fatal("永久失效应当进黑名单（即使整体启动因此失败，也必须先落盘，" +
					"否则下次启动会重新探测一遍同一个坏后端）")
			}
		})
	}
}

// 401 的 key 从账户池剔除（运行期生效，不回写配置文件），后端本身仍可加载。
func TestValidateDropsInvalidKeys(t *testing.T) {
	p := testPaths(t)
	snap := mustLoad(t, p, `{
      "accounts":[{"name":"main","base_url":"http://a.invalid","api_keys":["good-key-12345678","bad-key-12345678"]}],
      "backends":[{"name":"b","account":"main","model":"m","enabled":true}]}`)

	kept, err := Validate(p, snap, ProberFunc(func(_ context.Context, b *upstream.Backend) ProbeResult {
		// 第 2 把 key 401，第 1 把好的 → 后端可加载
		return ProbeResult{
			OK: true, Dropped: []string{"bad-key-12345678"},
			KeyStat: "2 keys: 1 ok, 0 transient, 1 dead",
		}
	}))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(kept) != 1 {
		t.Fatalf("有一个好 key 时后端应可加载：%v", names(kept))
	}
	got := kept[0].BoundAccount().APIKeys
	if len(got) != 1 || got[0] != "good-key-12345678" {
		t.Fatalf("key 池 = %v, 期望只剩 [good-key-12345678]", got)
	}
	if _, bad := LoadBlocklist(p.BlockedFile)["b"]; bad {
		t.Error("只是 key 失效、仍有可用 key 时不该拉黑后端")
	}
}

// 已在黑名单里的后端：跳过、不探测、不载入。
func TestValidateSkipsAlreadyBlocked(t *testing.T) {
	p := testPaths(t)
	snap := mustLoad(t, p, `{"backends":[
      {"name":"blocked","base_url":"http://a.invalid","model":"m","enabled":true},
      {"name":"fresh","base_url":"http://b.invalid","model":"m","enabled":true}]}`)

	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := SaveBlocklist(p.BlockedFile, map[string]BlockedEntry{
		"blocked": {Name: "blocked", Reason: "403 no-credit", At: "2026-10-01T00:00:00Z"},
	}); err != nil {
		t.Fatalf("SaveBlocklist: %v", err)
	}

	probed := map[string]bool{}
	kept, err := Validate(p, snap, ProberFunc(func(_ context.Context, b *upstream.Backend) ProbeResult {
		probed[b.Name] = true
		return ProbeResult{OK: true}
	}))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(kept) != 1 || kept[0].Name != "fresh" {
		t.Fatalf("保留 = %v, 期望只剩 [fresh]", names(kept))
	}
	if probed["blocked"] {
		t.Error("黑名单里的后端不该被重复探测")
	}
}

// enabled=false 与手动标签（disabled_until 未到期）同样不载入。
func TestValidateSkipsDisabledAndManuallyPaused(t *testing.T) {
	p := testPaths(t)
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	// 格式写错的时间字段 → 视为未打标签（不该悄悄摘掉健康后端）
	snap := mustLoad(t, p, `{"backends":[
      {"name":"off","base_url":"http://a.invalid","model":"m","enabled":false},
      {"name":"paused","base_url":"http://b.invalid","model":"m","enabled":true,
       "disabled_until":"`+future+`","disabled_reason":"等对方恢复"},
      {"name":"expired-tag","base_url":"http://c.invalid","model":"m","enabled":true,
       "disabled_until":"`+past+`","disabled_reason":"早就恢复了"},
      {"name":"bad-tag","base_url":"http://d.invalid","model":"m","enabled":true,
       "disabled_until":"不是 RFC3339","disabled_reason":"写错了"}]}`)

	kept, err := Validate(p, snap, ProberFunc(func(context.Context, *upstream.Backend) ProbeResult {
		return ProbeResult{OK: true}
	}))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(kept) != 2 || !hasName(kept, "expired-tag") || !hasName(kept, "bad-tag") {
		t.Fatalf("保留 = %v, 期望 [expired-tag bad-tag]", names(kept))
	}
	if _, err := os.Lstat(p.BlockedFile); !os.IsNotExist(err) {
		t.Errorf("这几种情况都不该写黑名单（stat err=%v）", err)
	}
}

// prober 为 nil（ROUTER_VALIDATE=0 的等价物）→ 只做剔除，不探测、不写黑名单。
func TestValidateNilProberSkipsProbing(t *testing.T) {
	p := testPaths(t)
	snap := mustLoad(t, p, `{"backends":[
      {"name":"a","base_url":"http://a.invalid","model":"m","enabled":true},
      {"name":"b","base_url":"http://b.invalid","model":"m","enabled":false}]}`)

	kept, err := Validate(p, snap, nil)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(kept) != 1 || kept[0].Name != "a" {
		t.Fatalf("保留 = %v, 期望 [a]", names(kept))
	}
	if _, err := os.Lstat(p.BlockedFile); !os.IsNotExist(err) {
		t.Errorf("跳过探测时不该写黑名单（stat err=%v）", err)
	}
}

// 全被剔除 / 全被判死 → 返回明确错误，不 log.Fatal。
func TestValidateAllGoneReturnsError(t *testing.T) {
	t.Run("全部被剔除", func(t *testing.T) {
		p := testPaths(t)
		snap := mustLoad(t, p, oneBackendJSON("off"))
		snap.Backends[0].Enabled = false

		_, err := Validate(p, snap, ProberFunc(
			func(context.Context, *upstream.Backend) ProbeResult { return ProbeResult{OK: true} }))
		if err == nil {
			t.Fatal("全部不可用时应当返回错误")
		}
		if !strings.Contains(err.Error(), "没有可用后端") {
			t.Errorf("错误消息应说明没有可用后端：%v", err)
		}
	})

	t.Run("全部判死", func(t *testing.T) {
		p := testPaths(t)
		snap := mustLoad(t, p, oneBackendJSON("only"))

		_, err := Validate(p, snap, ProberFunc(
			func(context.Context, *upstream.Backend) ProbeResult {
				return ProbeResult{Permanent: true, Reason: "401 auth"}
			}))
		if err == nil {
			t.Fatal("全部判死时应当返回错误")
		}
		if !strings.Contains(err.Error(), "人工清理") {
			t.Errorf("错误消息要提醒黑名单只写不删、需人工清理：%v", err)
		}
	})
}

func TestValidateNilArguments(t *testing.T) {
	p := testPaths(t)
	snap := mustLoad(t, p, oneBackendJSON("a"))
	probe := ProberFunc(func(context.Context, *upstream.Backend) ProbeResult { return ProbeResult{OK: true} })

	if _, err := Validate(nil, snap, probe); err == nil {
		t.Error("nil Paths 应当返回错误而不是 panic")
	}
	if _, err := Validate(p, nil, probe); err == nil {
		t.Error("nil Snapshot 应当返回错误而不是 panic")
	}
}

// ── 黑名单：只写不删 ────────────────────────────────────────

// ⚠️ 黑名单只写不删是**既有行为**，这里把它钉死。
//
// 一个后端被拉黑之后即使当场就恢复了，条目也不会自动消失，
// 必须人工清理黑名单文件。这条看起来像个缺陷，但代价不对称：
// 自动摘除会带来「反复拉黑/放回」的抖动，让人无法判断当前到底谁在服役；
// 而人工清理的代价只是重启一次。
func TestBlocklistNeverRemovesRecoveredEntry(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// 第一次启动：后端探测正常，不该动黑名单
	snap := mustLoad(t, p, oneBackendJSON("flaky"))
	if _, err := Validate(p, snap, ProberFunc(
		func(context.Context, *upstream.Backend) ProbeResult { return ProbeResult{OK: true} })); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := SaveBlocklist(p.BlockedFile, map[string]BlockedEntry{
		"flaky": {Name: "flaky", Reason: "403 no-credit", At: "2026-10-01T00:00:00Z"},
	}); err != nil {
		t.Fatalf("SaveBlocklist: %v", err)
	}

	// 第二次启动：后端**已经完全恢复**（探测 OK），但它在黑名单里，
	// 于是被跳过 —— 而且黑名单里那条**必须还在**。
	probed := false
	kept, err := Validate(p, snap, ProberFunc(func(_ context.Context, b *upstream.Backend) ProbeResult {
		probed = true
		return ProbeResult{OK: true}
	}))
	if err == nil {
		t.Fatal("只有这一个后端且它在黑名单里时应当返回错误")
	}
	if probed {
		t.Error("黑名单里的后端不该被探测（所以它的「已恢复」根本无从得知）")
	}
	if len(kept) != 0 {
		t.Fatalf("保留 = %v, 期望空", names(kept))
	}
	if _, ok := LoadBlocklist(p.BlockedFile)["flaky"]; !ok {
		t.Fatal("⚠️ 已恢复的条目被自动移除了 —— 黑名单只写不删是既有行为，不能改")
	}
}

// 写入时保留表里已有的条目（不因为这次没新增就把老的丢了）。
func TestSaveBlocklistPreservesExisting(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	cur := map[string]BlockedEntry{
		"old": {Name: "old", Reason: "401 auth", At: "2026-10-01T00:00:00Z"},
	}
	if err := SaveBlocklist(p.BlockedFile, cur); err != nil {
		t.Fatalf("SaveBlocklist: %v", err)
	}
	// 再存一次，内容相同
	if err := SaveBlocklist(p.BlockedFile, cur); err != nil {
		t.Fatalf("二次 SaveBlocklist: %v", err)
	}
	got := LoadBlocklist(p.BlockedFile)
	if len(got) != 1 || got["old"].Reason != "401 auth" {
		t.Fatalf("往返后 = %v, 期望保留 [old]", got)
	}
}

// 损坏的黑名单不该让网关起不来（当作空表即可）。
func TestLoadBlocklistCorruptIsIgnored(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(p.BlockedFile, []byte("{not a list"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := LoadBlocklist(p.BlockedFile); len(got) != 0 {
		t.Fatalf("损坏的黑名单应当被忽略，得到 %v", got)
	}
}

// 文件不存在 → 空表，不是错误。
func TestLoadBlocklistMissingIsEmpty(t *testing.T) {
	p := testPaths(t)
	if got := LoadBlocklist(p.BlockedFile); len(got) != 0 {
		t.Fatalf("黑名单不存在时应返回空表，得到 %v", got)
	}
}

// 黑名单文件格式是人可读的数组（能直接 cat 出来看、直接手改）。
func TestBlocklistFileIsReadableJSON(t *testing.T) {
	p := testPaths(t)
	if err := os.MkdirAll(p.ConfigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := SaveBlocklist(p.BlockedFile, map[string]BlockedEntry{
		"a": {Name: "a", Reason: "403", At: "2026-10-01T00:00:00Z"},
	}); err != nil {
		t.Fatalf("SaveBlocklist: %v", err)
	}
	data, err := os.ReadFile(p.BlockedFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var list []BlockedEntry
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatalf("黑名单不是合法 JSON：%v", err)
	}
	if len(list) != 1 || list[0].Name != "a" {
		t.Fatalf("黑名单内容 = %+v", list)
	}
}

// ── 小工具 ──────────────────────────────────────────────────

func TestShouldValidate(t *testing.T) {
	t.Run("默认开", func(t *testing.T) {
		t.Setenv(envValidate, "")
		if !ShouldValidate() {
			t.Error("默认应开启自检")
		}
		t.Setenv(envValidate, "1")
		if !ShouldValidate() {
			t.Error("显式 1 应开启")
		}
	})
	t.Run("只有 0 才关", func(t *testing.T) {
		t.Setenv(envValidate, "0")
		if ShouldValidate() {
			t.Error("显式 0 应关闭")
		}
		// 拼错的值不该让自检被静默跳过 —— 宽松关闭是对的
		t.Setenv(envValidate, "off")
		if !ShouldValidate() {
			t.Error("非 0 的值应当保持开启（宽松关闭）")
		}
	})
}

func TestMaskKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "****"},
		{"short", "****"},
		{"1234567890ab", "****"},         // 正好 12：整把打码（再长一位就露头尾了）
		{"1234567890abc", "123456…0abc"}, // 13：露头 6 尾 4
	} {
		if got := maskKey(tc.in); got != tc.want {
			t.Errorf("maskKey(%q) = %q, 期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("未超长不该截断，得到 %q", got)
	}
	if got := truncate("hello", 3); got != "hel" {
		t.Errorf("超长应截断，得到 %q", got)
	}
}

func TestReadSnippet(t *testing.T) {
	got := readSnippet(strings.NewReader("  line1\nline2  "), 100)
	if got != "line1 line2" {
		t.Errorf("换行应折成空格并去空白，得到 %q", got)
	}
	if got := readSnippet(strings.NewReader("abcdef"), 3); got != "abc" {
		t.Errorf("应按 limit 截断，得到 %q", got)
	}
}

func TestUUIDHex(t *testing.T) {
	a, b := uuidHex(), uuidHex()
	if len(a) != 32 {
		t.Errorf("长度 = %d, 期望 32（无横杠的 hex）", len(a))
	}
	if a == b {
		t.Error("两次生成不应相同")
	}
}

// mustLoad 从字符串加载一份快照。
func mustLoad(t *testing.T, p *runtime.Paths, content string) *Snapshot {
	t.Helper()
	t.Setenv(envBackends, "")
	writeConf(t, p, content)
	snap, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return snap
}
