// Package config 是后端配置的加载层：把「配置从哪来」和「配置对不对」这两件事
// 从 main 里搬出来，各归一个文件：
//
//   - load.go      读配置 → 不可变快照；按 mtime 热加载
//   - validate.go  启动自检：真打上游探测，永久失效的写黑名单
//
// ── 依赖方向 ────────────────────────────────────────────────
// config → runtime + core/upstream + core/errclass + 标准库。
// **不 import internal/service/ 任何包**：配置层不认识路由、熔断策略、请求执行。
// 理由不是洁癖，而是方向：service 层要换实现（换一套路由算法、换上游 SDK），
// 配置格式不该跟着变；反过来，改一份 backends.json 的字段名不该逼着路由层跟着改。
//
// 路径一律来自 runtime.Paths，本包一个 filepath.Join 都不自己拼。理由见
// runtime/paths.go 的包注释：手拼的路径既不能被环境变量覆盖，也无法在测试里
// 重定向到临时目录，于是所有依赖它的测试都得往真实路径写东西。
//
// ── 三条搬入时保留的行为 ────────────────────────────────────
//  1. 不引入包级全局可变状态。Load 返回值，调用方持有；热加载由 Store 负责，
//     Store 也是显式构造的。两个进程/两个测试各持一份，互不污染。
//  2. 没有任何 log.Fatal。配置读不出来、全部后端都死了，那是调用方（main）
//     决定退出还是降级的事；本包只负责把话说清楚。这与 runtime.Resolve 的
//     「只解析不落盘」是同一条纪律：能返回错误就别替调用方做决定。
//  3. 路径与权限语义照搬 loadBackends / validate.go，见各函数的注释。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"shanhai-gateway/internal/core/upstream"
	"shanhai-gateway/internal/runtime"
)

// envBackends 是「直接注入后端配置 JSON」的通道。
//
// 为什么保留这个口子：容器/systemd 场景下把 backends.json 打进镜像是不方便的
// （改一个 key 要重建镜像），而注入一份 JSON 进环境变量是很多编排系统的原生做法。
// 它排在文件**之前**，因为显式注入的意图比「磁盘上那份可能已经旧了的文件」
// 更强 —— 这一点与旧实现（文件覆盖 env）相反，是本层的有意变更，
// 理由见 Load 的注释。
const envBackends = "ROUTER_BACKENDS"

// configFile 是 backends.json 的新格式：账号 + 后端。
// 账号段是「key 池」：同账号的多个后端共享一组 key，加一个账号只需加一组 key。
type configFile struct {
	Accounts []*upstream.Account `json:"accounts"`
	Backends []*upstream.Backend `json:"backends"`
}

// Snapshot 是一次配置加载的不可变结果。
//
// 为什么是值传递而不是共享指针：调用方拿到快照之后，无论 Store 之后重载了多少次，
// 它手里的这个 Snapshot 都不会变。要求调用方「记得每次都重新取一次」是防不住的 ——
// 总有那么一个 goroutine 会拿着旧切片跑半个请求。
//
// ⚠️ Backends 里的 *upstream.Backend 是**有状态的**（熔断计数、冷却时间、统计）。
// 不可变指的是这份**列表**，不是后端本身：热加载会造出一批新的 Backend 实例，
// 老实例上积累的健康状态不带过去。这是刻意的 —— 见 Store.ReloadIfChanged 的注释。
type Snapshot struct {
	// Backends 是配置解析出来、已跑完 upstream.Backend.Init 的后端列表。
	// 调用方**不应**修改这个切片（也不应改里面的元素）。
	Backends []*upstream.Backend

	// LoadedAt 是这份快照的加载时刻，用于看板显示「配置是什么时候的」。
	LoadedAt time.Time
}

// Load 读取后端配置。数据源优先级：
//
//  1. env ROUTER_BACKENDS（JSON 字符串，直接注入）
//  2. runtime.Paths.BackendsFile
//
// 两者都空 → 返回明确错误。**不 log.Fatal** —— 那是 main 的事：
// 降级运行、只起管理端、让运维改完配置再拉起来，都是调用方的选择。
//
// ⚠️ 优先级与旧实现相反（旧：ROUTER_BACKENDS_FILE 覆盖 ROUTER_BACKENDS）。
// 这是一个有意为之的变更，理由：显式注入表达的是「这一份就是现在要的」，
// 而磁盘上的文件可能是上一次部署留下的（改了环境变量却没换挂载卷，是很常见的
// 运维失误 —— 那种情况下旧实现会让改过的环境变量静默失效，而这恰恰是最难查的
// 一类问题）。旧优先级要想生效，还得额外记住「文件优先」这条反直觉的规则。
//
// 支持两种 JSON 形态（与旧实现一致）：
//   - 新格式：{"accounts": [...], "backends": [...]}
//   - 旧格式：顶层就是一个数组 [...]
//
// ⚠️ 本函数**只读不写**：不创建目录、不创建文件、不生成默认配置。
// 测试里「Load 不创建任何文件」这条就是靠这个性质成立的。
func Load(p *runtime.Paths) (*Snapshot, error) {
	if p == nil {
		return nil, fmt.Errorf("config: Paths 为 nil，无法读取后端配置")
	}

	jsonStr := runtime.EnvString(envBackends, "")
	source := "env " + envBackends
	if jsonStr == "" {
		// 文件不存在不是错误：可能就是要用 env 注入，也可能是配置还没写。
		// 两者都空才在下面报「没有配置」。
		data, err := os.ReadFile(p.BackendsFile)
		switch {
		case err == nil:
			jsonStr = string(data)
			source = p.BackendsFile
		case os.IsNotExist(err):
			// 留空，落到下面的「都没有」分支
		default:
			// 读得到路径但读不了（权限、目录、挂载点掉了）：这是真故障，
			// 不能当成「没配置」然后拿一个空快照继续跑。
			return nil, fmt.Errorf("config: 读取后端配置 %s 失败：%w", p.BackendsFile, err)
		}
	}

	if jsonStr == "" {
		return nil, fmt.Errorf(
			"config: 没有后端配置 —— env %s 为空，%s 也不存在或为空。"+
				"请写 %s（格式见 Account/Backend 的 json tag），或用 env %s 直接注入 JSON",
			envBackends, p.BackendsFile, p.BackendsFile, envBackends)
	}

	backends, accounts, err := parseBackends(jsonStr)
	if err != nil {
		return nil, fmt.Errorf("config: 解析后端配置（来源：%s）失败：%w", source, err)
	}

	for _, b := range backends {
		// 补默认并发位/权重、绑定账号、内联 key 并入账号池、建统计与并发位缓冲。
		// 与搬入前 loadBackends 的后处理逐条对应，全在 Init 里。
		b.Init(accounts)
		logBackend(b)
	}

	return &Snapshot{Backends: backends, LoadedAt: time.Now()}, nil
}

// parseBackends 解析 JSON → (后端列表, 账号索引)。
//
// 先按新格式 {accounts, backends} 试；解析失败**或**两个字段都为 nil 时，
// 退回旧格式（顶层数组）。这个「都为空也算退回」的细节与旧实现逐字一致：
// 一个只写了 {"backends": null} 的配置在旧行为下会被当成旧格式然后解析失败，
// 照搬而不是"改进" —— 改了它，两种格式的边界就再也没法用旧配置验证了。
//
// ⚠️ 旧实现在这里只 log 一条然后让 backends 保持为空，最终由
// 「no backends configured」的 log.Fatal 兜住。现在直接返回 error，
// 消息里带上原始解析错误，省掉一次「到底是我的 JSON 写错了还是文件没读到」的追查。
func parseBackends(jsonStr string) ([]*upstream.Backend, map[string]*upstream.Account, error) {
	accounts := map[string]*upstream.Account{}

	var cf configFile
	if err := json.Unmarshal([]byte(jsonStr), &cf); err == nil && (cf.Backends != nil || cf.Accounts != nil) {
		for _, a := range cf.Accounts {
			if a != nil && a.Name != "" {
				accounts[a.Name] = a
			}
		}
		return cf.Backends, accounts, nil
	}

	var backends []*upstream.Backend
	if err := json.Unmarshal([]byte(jsonStr), &backends); err != nil {
		return nil, nil, fmt.Errorf("%w；两种格式（新格式 {accounts,backends} / 旧格式顶层数组）都不认", err)
	}
	return backends, accounts, nil
}

// logBackend 打印一行后端摘要 —— 与搬入前 loadBackends 的两行日志同格式。
//
// 为什么在 Load 里打而不是让调用方打：这一行是「我到底载了些什么」的唯一记录，
// 而配置层是唯一同时看得见「原始配置」和「Init 之后的结果」的地方
// （补完默认并发位、账号继承的 base_url、账号池里的 key 数都在这一步才确定）。
// 挪到调用方打，打的就全是空字段。
func logBackend(b *upstream.Backend) {
	acct := b.BoundAccount()
	if !b.Enabled {
		log.Printf("  - %s (DISABLED): account=%s %s model=%s", b.Name, acct.Name, b.BaseURL, b.Model)
		return
	}
	log.Printf("  - %s: account=%s %s model=%s keys=%d conc=%d timeout=%.0fs",
		b.Name, acct.Name, b.BaseURL, b.Model, len(acct.APIKeys), b.Concurrency, b.Timeout)
}

// Store 持有一份当前配置，并按文件 mtime 提供热加载。
//
// 能力照搬公版 KeyStore.reloadIfChanged（见根目录 main.go），只是把对象从
// 「用户 API Key」换成「后端配置」——改后端不用重启。
//
// ⚠️ 三条它必须遵守的性质：
//  1. **读路径不阻塞**：Snapshot 拿 RWMutex 的读锁就返回，指针换掉是 O(1)。
//     每次请求都读配置，所以这里绝不能拿写锁做重活。
//  2. **重载失败保留旧配置**：手滑编辑 JSON 打到一半、语法错误、临时不可读 ——
//     这些都必须只返回错误，旧快照原封不动。一次手滑不能让全站路由崩掉。
//  3. **不引入包级全局状态**：Store 由调用方构造并持有。两个测试各建一个，
//     互不影响；两个进程各跑各的。
type Store struct {
	mu    sync.RWMutex
	paths *runtime.Paths
	cur   *Snapshot
	// mtime 是当前快照对应的源文件修改时间。env 注入时为 nil
	// （没有文件可盯，env 的变化只能靠重启进程 —— os.Setenv 改的运行环境
	// 对已经在跑的进程是不生效的）。
	mtime *time.Time
}

// NewStore 读一次配置并建 Store。读失败直接返回 error。
func NewStore(p *runtime.Paths) (*Store, error) {
	snap, err := Load(p)
	if err != nil {
		return nil, err
	}
	s := &Store{paths: p, cur: snap}
	if mt, err := sourceMtime(p); err == nil {
		s.mtime = &mt
	}
	return s, nil
}

// NewStoreFrom 用已有的快照建 Store（调用方已经自己 Load 过一次的场景）。
func NewStoreFrom(p *runtime.Paths, snap *Snapshot) *Store {
	s := &Store{paths: p, cur: snap}
	if mt, err := sourceMtime(p); err == nil {
		s.mtime = &mt
	}
	return s
}

// Paths 返回 Store 绑定的路径（校验等操作需要同一个 BlockedFile）。
func (s *Store) Paths() *runtime.Paths { return s.paths }

// Snapshot 返回当前配置的快照。读路径只拿读锁，可并发调用。
//
// 返回的是**快照本身**，不是副本：调用方若长期持有它，就该知道自己拿的是
// 「那一刻的配置」。真要每次都看最新的，就每次调 Snapshot()。
func (s *Store) Snapshot() *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// ReloadIfChanged 比对 mtime，变了才重新加载。返回是否发生了重载。
//
// 未改动时只做一次 stat + 一次读锁，比对完就返回 —— 不重新解析、不重新 Init。
//
// 重载失败时**保留旧配置**并返回错误：解析不了的 JSON、读不了的文件都不会
// 影响正在跑的路由。这条是硬要求：手滑编辑配置是必然会发生的，
// 而为一次手滑把全站路由搞崩是不可接受的。
//
// ⚠️ 重载会造出一批**新的** Backend 实例，已有的熔断/冷却/统计状态不带过去。
// 这是刻意的：配置改了之后，「新后端」和「旧后端」已经不是一回事了，把旧的
// 冷却计数套到新配置上是错的。代价是重载瞬间正在进行中的请求仍在用老实例
// （它们自己持有指针），新请求走新的 —— 两批并存是对的，各自的计数互不污染。
func (s *Store) ReloadIfChanged() (bool, error) {
	mt, err := sourceMtime(s.paths)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && envInjected() {
			// 配置来自 env 注入，没有文件可盯。这不是故障，是这一种部署形态
			// 的正常状态：改配置靠重启进程（对已经在跑的进程改环境变量不会生效）。
			// 在这里报 error 会让调用方每分钟记一条假告警。
			return false, nil
		}
		// 文件读不到（被删/权限）：保留旧配置。返回 error 是因为这多半是
		// 故障，而不是「本来就没有文件」——
		// 那意味着配置没了却还在拿旧配置跑，必须让人知道。
		return false, fmt.Errorf("config: 检查 %s 失败，沿用旧配置：%w", s.paths.BackendsFile, err)
	}

	s.mu.RLock()
	cur := s.cur
	last := s.mtime
	s.mu.RUnlock()

	if last != nil && !mt.After(*last) {
		return false, nil
	}

	snap, err := Load(s.paths)
	if err != nil {
		log.Printf("[config] 热加载失败，继续沿用旧配置（%d 个后端）：%v", len(cur.Backends), err)
		return false, err
	}

	s.mu.Lock()
	s.cur = snap
	s.mtime = &mt
	s.mu.Unlock()

	log.Printf("[config] 已热加载 %s：%d 个后端", s.paths.BackendsFile, len(snap.Backends))
	return true, nil
}

// sourceMtime 取配置源文件的修改时间。
//
// env 注入时返回 os.ErrNotExist —— 「没有文件可盯」是一个正常状态，
// 不是故障，所以让调用方能用 errors.Is 区分开。
func sourceMtime(p *runtime.Paths) (time.Time, error) {
	if p == nil {
		return time.Time{}, fmt.Errorf("config: Paths 为 nil")
	}
	if envInjected() {
		// env 注入优先：文件根本不参与，盯它的 mtime 毫无意义。
		return time.Time{}, os.ErrNotExist
	}
	fi, err := os.Stat(p.BackendsFile)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// envInjected 当前是否由 env ROUTER_BACKENDS 提供配置。
func envInjected() bool { return strings.TrimSpace(runtime.EnvString(envBackends, "")) != "" }

// readFile 是 os.ReadFile 的薄封装，只为让「读不到」和「读到了但是空的」
// 这两种情况在调用点上读起来是一件事。
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// writeFile 是 os.WriteFile 的薄封装（0644）。
//
// ⚠️ 不建父目录：黑名单目录不存在时写失败并报出来，比替用户建一个他没要求的
// 目录要诚实 —— 目录该由部署方（或 runtime.Paths.EnsureDirs）负责。
func writeFile(path string, data []byte) error { return os.WriteFile(path, data, 0o644) }
