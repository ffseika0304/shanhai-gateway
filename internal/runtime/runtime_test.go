package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // 真实 SQLite 驱动，仅测试用
)

// --- 测试辅助 ---

// clearRuntimeEnv 清空所有会影响路径解析的环境变量，并给出干净的 HOME。
// 每个用例都必须先调它：进程环境是全局的，漏清一个就会让结果依赖执行顺序。
func clearRuntimeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		envDataDir, envConfigDir, envDBFile,
		envBackendsFile, envKeysFile, envQuotaFile,
		"XDG_DATA_HOME",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("HOME", t.TempDir())
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	return string(b)
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Errorf("期望 %s 不存在，实际存在", path)
	} else if !os.IsNotExist(err) {
		t.Errorf("检查 %s 时出错：%v", path, err)
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("期望 %s 存在：%v", path, err)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// --- identity ---

func TestAppID(t *testing.T) {
	if got, want := AppID(), AppName+"/"+AppVersion; got != want {
		t.Fatalf("AppID() = %q, want %q", got, want)
	}

	// 身份必须中性、可开源：不点名任何个人品牌，只约束形态 ——
	// 小写 ASCII 字母，且不含分隔符（分隔符会直接污染文件名和日志前缀）。
	if AppName != "shg" {
		t.Errorf("AppName = %q，改名/开源时只需改这一处", AppName)
	}
	for i, r := range AppName {
		if r < 'a' || r > 'z' {
			t.Errorf("AppName 第 %d 个字符 %q 不是小写 ASCII 字母", i, r)
		}
	}
	for _, bad := range []string{" ", "/", ".", "-", "_"} {
		if strings.ContainsAny(AppName, bad) {
			t.Errorf("AppName %q 含分隔符 %q，会污染路径与日志前缀", AppName, bad)
		}
	}
}

func TestDefaultDataDir(t *testing.T) {
	t.Run("HOME 生效", func(t *testing.T) {
		clearRuntimeEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)

		got, err := DefaultDataDir()
		if err != nil {
			t.Fatalf("DefaultDataDir() 报错：%v", err)
		}
		want := filepath.Join(home, ".local", "share", AppName)
		if got != want {
			t.Fatalf("DefaultDataDir() = %q, want %q", got, want)
		}
	})

	t.Run("XDG_DATA_HOME 优先", func(t *testing.T) {
		clearRuntimeEnv(t)
		xdg := t.TempDir()
		t.Setenv("XDG_DATA_HOME", xdg)

		got, err := DefaultDataDir()
		if err != nil {
			t.Fatalf("DefaultDataDir() 报错：%v", err)
		}
		want := filepath.Join(xdg, AppName)
		if got != want {
			t.Fatalf("DefaultDataDir() = %q, want %q", got, want)
		}
	})

	t.Run("XDG_DATA_HOME 非绝对路径被忽略", func(t *testing.T) {
		clearRuntimeEnv(t)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_DATA_HOME", "relative/dir")

		got, err := DefaultDataDir()
		if err != nil {
			t.Fatalf("DefaultDataDir() 报错：%v", err)
		}
		if want := filepath.Join(home, ".local", "share", AppName); got != want {
			t.Fatalf("DefaultDataDir() = %q, want %q", got, want)
		}
	})

	t.Run("无 HOME 返回错误", func(t *testing.T) {
		clearRuntimeEnv(t)
		t.Setenv("HOME", "")
		t.Setenv("XDG_DATA_HOME", "")

		if _, err := DefaultDataDir(); err == nil {
			t.Fatal("HOME 未设置时应当返回错误，而不是猜一个目录")
		}
	})
}

// --- Resolve ---

func TestResolveDefaults(t *testing.T) {
	clearRuntimeEnv(t)
	home := t.TempDir()
	configDir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(envConfigDir, configDir)

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() 报错：%v", err)
	}

	dataDir := filepath.Join(home, ".local", "share", AppName)
	checks := []struct{ name, got, want string }{
		{"DataDir", p.DataDir, dataDir},
		{"LogDir", p.LogDir, filepath.Join(dataDir, "logs")},
		{"ConfigDir", p.ConfigDir, configDir},
		{"DBFile", p.DBFile, filepath.Join(dataDir, AppName+".db")},
		{"BackendsFile", p.BackendsFile, filepath.Join(configDir, "backends.json")},
		{"BlockedFile", p.BlockedFile, filepath.Join(configDir, "backends.blocked.json")},
		{"KeysFile", p.KeysFile, filepath.Join(configDir, "keys.json")},
		{"QuotaFile", p.QuotaFile, filepath.Join(dataDir, "quota.json")},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// 没有 GATEWAY_CONFIG_DIR 时必须自己找到仓库根下的 config/。
func TestResolveDefaultConfigDir(t *testing.T) {
	clearRuntimeEnv(t)

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() 报错：%v", err)
	}
	if !filepath.IsAbs(p.ConfigDir) {
		t.Fatalf("默认 ConfigDir 应当是绝对路径，实际 %q", p.ConfigDir)
	}
	if filepath.Base(p.ConfigDir) != "config" {
		t.Fatalf("默认 ConfigDir 应当指向 config/，实际 %q", p.ConfigDir)
	}
	if _, err := os.Stat(filepath.Join(p.ConfigDir, "..", "go.mod")); err != nil {
		t.Fatalf("默认 ConfigDir 的上级应当是模块根（含 go.mod）：%v", err)
	}
}

func TestResolveEnvOverrides(t *testing.T) {
	clearRuntimeEnv(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	configDir := filepath.Join(root, "conf")
	dbFile := filepath.Join(root, "custom", "main.db")
	backends := filepath.Join(root, "custom", "b.json")
	keys := filepath.Join(root, "custom", "k.json")
	quota := filepath.Join(root, "custom", "q.json")

	t.Setenv(envDataDir, dataDir)
	t.Setenv(envConfigDir, configDir)
	t.Setenv(envDBFile, dbFile)
	t.Setenv(envBackendsFile, backends)
	t.Setenv(envKeysFile, keys)
	t.Setenv(envQuotaFile, quota)

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() 报错：%v", err)
	}

	checks := []struct{ name, got, want string }{
		{"DataDir", p.DataDir, dataDir},
		{"ConfigDir", p.ConfigDir, configDir},
		// LogDir 始终跟着 DataDir 走：日志和数据必须在同一个可写、可搬走的地方。
		{"LogDir", p.LogDir, filepath.Join(dataDir, "logs")},
		{"DBFile", p.DBFile, dbFile},
		{"BackendsFile", p.BackendsFile, backends},
		{"KeysFile", p.KeysFile, keys},
		{"QuotaFile", p.QuotaFile, quota},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("env 覆盖未生效：%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// Resolve 是纯函数：解析路径不得在磁盘上留下任何痕迹。
func TestResolveCreatesNothing(t *testing.T) {
	clearRuntimeEnv(t)
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	configDir := filepath.Join(root, "conf")

	t.Setenv(envDataDir, dataDir)
	t.Setenv(envConfigDir, configDir)

	if _, err := Resolve(); err != nil {
		t.Fatalf("Resolve() 报错：%v", err)
	}

	for _, p := range []string{dataDir, configDir, filepath.Join(dataDir, "logs")} {
		mustNotExist(t, p)
	}
	// 整个 root 下面除了临时目录本身不该多出任何东西。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("Resolve() 之后 %s 下出现了 %d 个条目", root, len(entries))
	}
}

func TestEnsureDirs(t *testing.T) {
	clearRuntimeEnv(t)
	root := t.TempDir()
	t.Setenv(envDataDir, filepath.Join(root, "data"))
	t.Setenv(envConfigDir, filepath.Join(root, "conf"))

	p, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() 报错：%v", err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() 报错：%v", err)
	}
	for _, dir := range []string{p.DataDir, p.LogDir, p.ConfigDir} {
		st, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("目录 %s 未创建：%v", dir, err)
		}
		if !st.IsDir() {
			t.Fatalf("%s 不是目录", dir)
		}
	}
	// 幂等：重复调用不得报错。
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() 第二次报错：%v", err)
	}
}

func TestEnsureDirsNil(t *testing.T) {
	var p *Paths
	if err := p.EnsureDirs(); err == nil {
		t.Fatal("nil Paths 应当返回错误而不是 panic")
	}
}

// --- RotatingWriter ---

// MaxBytes=12、Keep=2 下的确定性轮转序列（轮转在写入前判断）：
// aaa/bbb/ccc 填满一代 → ddd 触发第 1 次轮转 → eee/fff 填满 → ggg 触发第 2 次。
func TestRotatingWriterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	opt := LogOptions{MaxBytes: 12, Keep: 2}

	w, err := NewRotatingWriter(path, opt)
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	if w.Path() != path {
		t.Fatalf("Path() = %q, want %q", w.Path(), path)
	}
	for _, s := range []string{"aaa\n", "bbb\n", "ccc\n", "ddd\n", "eee\n", "fff\n", "ggg\n"} {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatalf("Write(%q): %v", s, err)
		}
	}

	if got := readFile(t, path+".2"); got != "aaa\nbbb\nccc\n" {
		t.Errorf(".2 内容 = %q, want %q", got, "aaa\nbbb\nccc\n")
	}
	if got := readFile(t, path+".1"); got != "ddd\neee\nfff\n" {
		t.Errorf(".1 内容 = %q, want %q", got, "ddd\neee\nfff\n")
	}
	if got := readFile(t, path); got != "ggg\n" {
		t.Errorf("当前文件内容 = %q, want %q", got, "ggg\n")
	}
	mustNotExist(t, path+".3")

	// hhh/iii 填满第 2 代，jjj 触发第 3 次轮转：此时 .2 已存在，必须被淘汰。
	for _, s := range []string{"hhh\n", "iii\n", "jjj\n"} {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatalf("Write(%q): %v", s, err)
		}
	}
	if got := readFile(t, path+".2"); got != "ddd\neee\nfff\n" {
		t.Errorf(".2 内容 = %q, want %q（最老的 aaa~ccc 应已被淘汰）", got, "ddd\neee\nfff\n")
	}
	if got := readFile(t, path+".1"); got != "ggg\nhhh\niii\n" {
		t.Errorf(".1 内容 = %q, want %q", got, "ggg\nhhh\niii\n")
	}
	if got := readFile(t, path); got != "jjj\n" {
		t.Errorf("当前文件 = %q, want %q", got, "jjj\n")
	}
	mustNotExist(t, path+".3")
}

func TestRotatingWriterKeepsOnlyKeepGenerations(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	w, err := NewRotatingWriter(path, LogOptions{MaxBytes: 8, Keep: 2})
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	defer w.Close()

	for i := 0; i < 10; i++ {
		if _, err := w.Write([]byte("0123456789abcdef\n")); err != nil {
			t.Fatalf("Write #%d: %v", i, err)
		}
	}
	for i := 1; i <= 2; i++ {
		mustExist(t, filepath.Join(dir, fmt.Sprintf("app.log.%d", i)))
	}
	for i := 3; i <= 6; i++ {
		mustNotExist(t, filepath.Join(dir, fmt.Sprintf("app.log.%d", i)))
	}
}

// 重启后追加：不覆盖旧内容，且大小要从已有内容算起。
func TestRotatingWriterAppendsToExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	writeFile(t, path, "0123456789") // 10 字节，MaxBytes=12

	w, err := NewRotatingWriter(path, LogOptions{MaxBytes: 12, Keep: 2})
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}
	// 10 + 3 > 12，必须先轮转。
	if _, err := w.Write([]byte("abc")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := readFile(t, path+".1"); got != "0123456789" {
		t.Errorf(".1 内容 = %q, want %q（旧日志应被完整保留）", got, "0123456789")
	}
	if got := readFile(t, path); got != "abc" {
		t.Errorf("当前文件 = %q, want %q", got, "abc")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close 可重复调用。
	if err := w.Close(); err != nil {
		t.Fatalf("Close 第二次: %v", err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("关闭后写入应当返回错误")
	}
}

func TestRotatingWriterDefaultOptions(t *testing.T) {
	opt := LogOptions{}.withDefaults()
	if opt.MaxBytes != 32<<20 {
		t.Errorf("默认 MaxBytes = %d, want %d", opt.MaxBytes, 32<<20)
	}
	if opt.Keep != 3 {
		t.Errorf("默认 Keep = %d, want 3", opt.Keep)
	}
	if got := (LogOptions{MaxBytes: -1, Keep: -1}).withDefaults(); got != opt {
		t.Errorf("负值未回落默认：%+v", got)
	}
}

// 路径创建失败必须返回 error，不能 panic。
func TestRotatingWriterBadPath(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	writeFile(t, blocker, "not a directory")

	if w, err := NewRotatingWriter(filepath.Join(blocker, "sub", "app.log"), LogOptions{}); err == nil {
		w.Close()
		t.Fatal("路径非法时应当返回 error，而不是 panic")
	}
	if _, err := NewRotatingWriter("  ", LogOptions{}); err == nil {
		t.Fatal("空路径应当返回 error")
	}
}

// 并发写 + 并发轮转：一行字节都不能丢。
func TestRotatingWriterConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	const (
		writers  = 8
		writes   = 40
		payload  = 512
		maxBytes = 4096
		keep     = 64 // 足够大，保证测试期间不淘汰任何一代
	)
	w, err := NewRotatingWriter(path, LogOptions{MaxBytes: maxBytes, Keep: keep})
	if err != nil {
		t.Fatalf("NewRotatingWriter: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < writes; j++ {
				if _, err := w.Write([]byte(strings.Repeat("x", payload))); err != nil {
					t.Errorf("并发 Write 失败：%v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		st, err := e.Info()
		if err != nil {
			t.Fatalf("Info %s: %v", e.Name(), err)
		}
		total += st.Size()
	}
	if want := int64(writers * writes * payload); total != want {
		t.Fatalf("并发写后总字节 = %d, want %d（有字节丢失）", total, want)
	}
}

// --- Sweep ---

func TestSweepRemovesJunkKeepsLiveFiles(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	logDir := filepath.Join(dataDir, "logs")
	configDir := filepath.Join(root, "conf")
	opt := LogOptions{MaxBytes: 1, Keep: 2}

	p := &Paths{
		DataDir:   dataDir,
		LogDir:    logDir,
		ConfigDir: configDir,
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	stale := []string{
		filepath.Join(dataDir, "upload.tmp"),
		filepath.Join(dataDir, "page.partial"),
		filepath.Join(dataDir, "edit.swp"),
	}
	for _, f := range stale {
		writeFile(t, f, "stale")
		old := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatalf("Chtimes %s: %v", f, err)
		}
	}
	fresh := filepath.Join(dataDir, "recent.tmp")
	writeFile(t, fresh, "fresh")

	// 轮转日志：Keep=2，.1/.2 保留，.3/.4 删除。
	for _, f := range []string{"app.log.1", "app.log.2", "app.log.3", "app.log.4"} {
		writeFile(t, filepath.Join(logDir, f), "log")
	}
	// 非日志的「点号数字」文件不该被误判成轮转日志。
	writeFile(t, filepath.Join(logDir, "backup.2024"), "keep")

	// 孤儿 -wal/-shm：主库已删。
	orphanWal := filepath.Join(dataDir, "gone.db-wal")
	orphanShm := filepath.Join(dataDir, "gone.db-shm")
	writeFile(t, orphanWal, "orphan")
	writeFile(t, orphanShm, "orphan")
	// 在库文件：伴生文件必须保留。
	liveDB := filepath.Join(dataDir, "live.db")
	writeFile(t, liveDB, "db")
	liveWal := liveDB + "-wal"
	liveShm := liveDB + "-shm"
	writeFile(t, liveWal, "wal")
	writeFile(t, liveShm, "shm")

	// 空目录删掉，有内容的目录留下。
	emptyDir := filepath.Join(dataDir, "cache", "nested")
	writeFile(t, filepath.Join(dataDir, "cache", "keepme"), "x")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	res, err := Sweep(p, opt)
	if err != nil {
		t.Fatalf("Sweep() 报错：%v", err)
	}

	if got, want := sortedCopy(res.RemovedTempFiles), sortedCopy(stale); !equal(got, want) {
		t.Errorf("RemovedTempFiles = %v, want %v", got, want)
	}
	mustExist(t, fresh)

	wantLogs := []string{
		filepath.Join(logDir, "app.log.3"),
		filepath.Join(logDir, "app.log.4"),
	}
	if got := sortedCopy(res.RemovedOldLogs); !equal(got, wantLogs) {
		t.Errorf("RemovedOldLogs = %v, want %v", got, wantLogs)
	}
	mustExist(t, filepath.Join(logDir, "app.log.1"))
	mustExist(t, filepath.Join(logDir, "app.log.2"))
	mustExist(t, filepath.Join(logDir, "backup.2024"))

	if got, want := sortedCopy(res.RemovedOrphanDB), sortedCopy([]string{orphanWal, orphanShm}); !equal(got, want) {
		t.Errorf("RemovedOrphanDB = %v, want %v", got, want)
	}
	mustExist(t, liveDB)
	mustExist(t, liveWal)
	mustExist(t, liveShm)

	if got := res.RemovedEmptyDirs; !equal(sortedCopy(got), []string{emptyDir}) {
		t.Errorf("RemovedEmptyDirs = %v, want %v", got, []string{emptyDir})
	}
	mustExist(t, filepath.Join(dataDir, "cache"))
	// LogDir 是 Paths 的一部分，即便空着也不能被删。
	mustExist(t, logDir)
}

// 清扫绝不能碰配置目录，即使它被塞进了数据目录里面。
func TestSweepNeverTouchesConfigDir(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	configDir := filepath.Join(dataDir, "conf")

	p := &Paths{
		DataDir:   dataDir,
		LogDir:    filepath.Join(dataDir, "logs"),
		ConfigDir: configDir,
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}

	cfgFile := filepath.Join(configDir, "backends.json")
	writeFile(t, cfgFile, `{"backends":[]}`)

	// 配置目录里也放一份「看起来该被扫掉」的东西，验证拦截真的生效。
	oldCfgTemp := filepath.Join(configDir, "draft.tmp")
	writeFile(t, oldCfgTemp, "draft")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldCfgTemp, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	writeFile(t, filepath.Join(configDir, "backup.log.9"), "keep")

	// 数据目录里放一份正常的垃圾，用来证明清扫确实跑过了。
	dataTemp := filepath.Join(dataDir, "junk.tmp")
	writeFile(t, dataTemp, "junk")
	if err := os.Chtimes(dataTemp, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	res, err := Sweep(p, LogOptions{MaxBytes: 1, Keep: 2})
	if err != nil {
		t.Fatalf("Sweep() 报错：%v", err)
	}

	mustExist(t, cfgFile)
	mustExist(t, oldCfgTemp)
	mustExist(t, filepath.Join(configDir, "backup.log.9"))
	mustExist(t, configDir)
	if got := sortedCopy(res.RemovedTempFiles); !equal(got, []string{dataTemp}) {
		t.Errorf("RemovedTempFiles = %v, want %v（不应包含配置目录里的文件）", got, []string{dataTemp})
	}
}

// 仓库目录绝不能被扫：配置在仓库里的 config/ 时，仓库里同名的垃圾文件要留着。
func TestSweepStaysInsideDataDir(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	p := &Paths{
		DataDir:   dataDir,
		LogDir:    filepath.Join(dataDir, "logs"),
		ConfigDir: filepath.Join(root, "conf"),
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	repoTemp := filepath.Join(root, "main.go.tmp")
	writeFile(t, repoTemp, "source")
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(repoTemp, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	if _, err := Sweep(p, LogOptions{}); err != nil {
		t.Fatalf("Sweep() 报错：%v", err)
	}
	mustExist(t, repoTemp)
}

func TestSweepErrorsOnMissingDataDir(t *testing.T) {
	root := t.TempDir()
	p := &Paths{
		DataDir:   filepath.Join(root, "nope"),
		LogDir:    filepath.Join(root, "nope", "logs"),
		ConfigDir: filepath.Join(root, "conf"),
	}
	if _, err := Sweep(p, LogOptions{}); err == nil {
		t.Fatal("数据目录不存在时应当返回错误（调用顺序错了，要让调用方看见）")
	}

	file := filepath.Join(root, "plain")
	writeFile(t, file, "x")
	p.DataDir = file
	if _, err := Sweep(p, LogOptions{}); err == nil {
		t.Fatal("数据路径是普通文件时应当返回错误")
	}

	var nilPaths *Paths
	if _, err := Sweep(nilPaths, LogOptions{}); err == nil {
		t.Fatal("nil Paths 应当返回错误而不是 panic")
	}
}

func TestLogGeneration(t *testing.T) {
	cases := []struct {
		name string
		want int
		ok   bool
	}{
		{"app.log.1", 1, true},
		{"app.log.42", 42, true},
		{"router.stderr.log.3", 3, true},
		{"app.log", 0, false},
		{"app.log.0", 0, false},
		{"app.log.x", 0, false},
		{"backup.2024", 0, false},
		{"app.log.2024", 2024, true},
		{"2024.log", 0, false}, // .log 结尾但不是轮转代数，是普通文件名
		{"log", 0, false},
	}
	for _, c := range cases {
		got, ok := logGeneration(c.name)
		if got != c.want || ok != c.ok {
			t.Errorf("logGeneration(%q) = (%d, %v), want (%d, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// --- Housekeeper ---

func TestHousekeeperRunOnce(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, AppName+".db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	// 单连接：sqlite 对并发写者敏感，测试里只要能验证语句能跑通。
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`,
		`INSERT INTO t (v) VALUES ('a'),('b'),('c')`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			t.Fatalf("Exec %q: %v", pragma, err)
		}
	}

	h := NewHousekeeper(db, time.Minute)
	if err := h.RunOnce(); err != nil {
		t.Fatalf("RunOnce() 报错：%v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("查询失败：%v", err)
	}
	if n != 3 {
		t.Fatalf("保养后数据丢了：count = %d, want 3", n)
	}

	// TRUNCATE checkpoint 应该把 WAL 收干净，再跑一轮确认可重复。
	if err := h.RunOnce(); err != nil {
		t.Fatalf("第二次 RunOnce() 报错：%v", err)
	}
	if _, err := db.Exec(`INSERT INTO t (v) VALUES ('d')`); err != nil {
		t.Fatalf("保养后无法继续写入：%v", err)
	}
}

func TestHousekeeperNilDB(t *testing.T) {
	h := NewHousekeeper(nil, time.Minute)
	if err := h.RunOnce(); err == nil {
		t.Fatal("没有绑定数据库时应返回错误而不是 panic")
	}
}

func TestHousekeeperStartStop(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, AppName+".db"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER)`); err != nil {
		t.Fatalf("建表失败：%v", err)
	}

	h := NewHousekeeper(db, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.Start(ctx)
	h.Start(ctx) // 重复启动不应产生第二个 goroutine

	time.Sleep(60 * time.Millisecond)

	// Stop 可重复调用，并且必须真的等 goroutine 退出。
	done := make(chan struct{})
	go func() {
		h.Stop()
		h.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop 卡住了，没有等后台 goroutine 退出")
	}

	// Stop 之后再 Start，goroutine 应立刻退出。
	h.Start(ctx)
	h.Stop()

	// ctx 取消也要能退出。
	h2 := NewHousekeeper(db, time.Hour)
	ctx2, cancel2 := context.WithCancel(context.Background())
	h2.Start(ctx2)
	cancel2()
	h2.Stop()
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
