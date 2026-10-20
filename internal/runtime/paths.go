package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 环境变量名集中在这里而不是散在各文件：调用方排查「为什么我的配置没生效」
// 时只需要看这一组常量，就能列出全部覆盖开关。
const (
	envDataDir       = "GATEWAY_DATA_DIR"
	envConfigDir     = "GATEWAY_CONFIG_DIR"
	envDBFile        = "GATEWAY_DB_FILE"
	envBackendsFile  = "GATEWAY_BACKENDS_FILE"
	envKeysFile      = "GATEWAY_KEYS_FILE"
	envQuotaFile     = "GATEWAY_QUOTA_FILE"
	envDashTokenFile = "GATEWAY_DASH_TOKEN_FILE"
	envDashSessKey   = "GATEWAY_DASH_SESSION_KEY_FILE"
)

// Paths 是运行时所有路径的唯一出口。
//
// 为什么做成值 struct 而不是一堆包级变量：包级变量在测试里改一个漏还原一个
// 就会互相污染，并发测试还会读到中间态。struct 只能通过 Resolve 产生，
// 显式传参，谁持有谁负责，没有隐式全局状态。
type Paths struct {
	// DataDir 是数据根目录：数据库、配额等「程序自己写」的东西都在这儿。
	// 默认 ~/.local/share/<AppName>，可用 GATEWAY_DATA_DIR 覆盖。
	DataDir string

	// LogDir 是日志目录：<DataDir>/logs。
	// 放在 DataDir 下而不是程序目录，理由同 DefaultDataDir 的注释。
	LogDir string

	// ConfigDir 是配置目录，默认 <仓库根>/config，可用 GATEWAY_CONFIG_DIR 覆盖。
	// 这里放的是「用户手写」的东西，程序只读不删。
	ConfigDir string

	// DBFile 是 SQLite 主库文件：<DataDir>/<AppName>.db，可用 GATEWAY_DB_FILE 覆盖。
	DBFile string

	// BackendsFile 是后端列表配置：<ConfigDir>/backends.json，可用 GATEWAY_BACKENDS_FILE 覆盖。
	BackendsFile string

	// BlockedFile 是熔断/封禁状态：<ConfigDir>/backends.blocked.json。
	// 不提供独立环境变量 —— 它必须和 BackendsFile 待在同一个目录，
	// 跟着配置目录一起被搬走；拆开覆盖只会制造「封禁文件找不到」这种故障。
	BlockedFile string

	// KeysFile 是密钥文件：<ConfigDir>/keys.json，可用 GATEWAY_KEYS_FILE 覆盖。
	// 含敏感信息，权限由调用方负责收紧。
	KeysFile string

	// DashSessionKeyFile 是看板会话签名密钥：<DataDir>/dash-session.key（0600）。
	// 落盘的意义：自签 cookie 的签名密钥不变 ⇒ 网关重启后老 cookie 仍有效。
	DashSessionKeyFile string

	// DashTokenFile 是看板静态 key 的持久化文件：<DataDir>/dash-token（0600），
	// 面板上 POST /api/dashkey 原子写它，dashToken 每次门检现读 ⇒ 改 key 即改即生效。
	DashTokenFile string

	// QuotaFile 是配额配置：<DataDir>/quota.json，可用 GATEWAY_QUOTA_FILE 覆盖。
	// 放 DataDir 而不是 ConfigDir：它属于运行状态，会被程序改写。
	QuotaFile string
}

// Resolve 按「环境变量 > 默认值」的优先级算出全部路径。
//
// 重要约定：本函数**只解析、不落盘**，一个目录、一个文件都不会创建。
// 副作用只发生在 Paths.EnsureDirs。这样切分之后，「想知道路径」和
// 「想创建目录」是两个可以分别测试、分别决策的动作，测试也就不会
// 在真实文件系统上留下痕迹。环境变量为空字符串视为未设置。
func Resolve() (*Paths, error) {
	dataDir := strings.TrimSpace(os.Getenv(envDataDir))
	if dataDir == "" {
		d, err := DefaultDataDir()
		if err != nil {
			return nil, err
		}
		dataDir = d
	}
	dataDir = filepath.Clean(dataDir)

	configDir := strings.TrimSpace(os.Getenv(envConfigDir))
	if configDir == "" {
		configDir = defaultConfigDir()
	}
	configDir = filepath.Clean(configDir)

	return &Paths{
		DataDir:            dataDir,
		LogDir:             filepath.Join(dataDir, "logs"),
		ConfigDir:          configDir,
		DBFile:             cleanOrEnv(envDBFile, filepath.Join(dataDir, AppName+".db")),
		BackendsFile:       cleanOrEnv(envBackendsFile, filepath.Join(configDir, "backends.json")),
		BlockedFile:        filepath.Join(configDir, "backends.blocked.json"),
		KeysFile:           cleanOrEnv(envKeysFile, filepath.Join(configDir, "keys.json")),
		QuotaFile:          cleanOrEnv(envQuotaFile, filepath.Join(dataDir, "quota.json")),
		DashTokenFile:      cleanOrEnv(envDashTokenFile, filepath.Join(dataDir, "dash-token")),
		DashSessionKeyFile: cleanOrEnv(envDashSessKey, filepath.Join(dataDir, "dash-session.key")),
	}, nil
}

// EnsureDirs 创建 DataDir / LogDir / ConfigDir（0755），已存在不报错。
//
// 与 Resolve 分开正是为了让副作用可控：解析路径这件事在任何时候都可以随便做，
// 只有真正要落盘的调用点才需要承担创建目录的后果。
// 0755 而不是 0700：数据目录要放日志，日志要能被运维用普通账号 tail；
// keys.json 这类敏感文件不进这几个目录，权限由使用方单独收紧。
func (p *Paths) EnsureDirs() error {
	if p == nil {
		return fmt.Errorf("runtime: Paths 为 nil，无法创建目录")
	}
	// 先父后子，报错时能直接指出是哪个目录。
	for _, dir := range []string{p.DataDir, p.LogDir, p.ConfigDir} {
		if strings.TrimSpace(dir) == "" {
			return fmt.Errorf("runtime: 目录路径为空，请检查相关环境变量")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("runtime: 创建目录 %s 失败：%w", dir, err)
		}
	}
	return nil
}

// cleanOrEnv 读单个环境变量；未设置时用兜底默认值。
func cleanOrEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return filepath.Clean(v)
	}
	return fallback
}

// defaultConfigDir 定位仓库根下的 config 目录。
//
// 候选顺序：可执行文件所在目录向上找 go.mod → 当前工作目录向上找 go.mod →
// cwd/config。
//
// 为什么拿 go.mod 当锚点：模块根目录是 Go 工具链认定的项目边界，和 .gitignore、
// systemd unit、配置约定天然重合，比猜「哪个目录长得像源码根」可靠得多。
// 为什么先看可执行文件：systemd / 打包场景下 cwd 可能是 /，而程序文件必然被
// 放在项目内或其 bin 子目录里，从它向上走更接近真实部署布局。
// 两个方向都找不到（纯源码目录里直接 go run、或发行版把二进制放进了
// 无 go.mod 的目录）时退回 cwd/config —— 这是用户唯一还能看见、改得动的位置。
func defaultConfigDir() string {
	if exe, err := os.Executable(); err == nil {
		// 二进制可能是软链（/usr/local/bin/gateway -> ...），
		// 不解析软链会沿着一串同名目录一路走到 /usr。
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		if root, ok := findModuleRoot(filepath.Dir(exe)); ok {
			return filepath.Join(root, "config")
		}
	}
	if wd, err := os.Getwd(); err == nil {
		if root, ok := findModuleRoot(wd); ok {
			return filepath.Join(root, "config")
		}
		return filepath.Join(wd, "config")
	}
	return "config"
}

// maxModuleRootDepth 限制向上查找层数。
// 没有它的话，从一个位于挂载深处的目录出发可能一路走到 /，白跑几百次 stat。
const maxModuleRootDepth = 32

// findModuleRoot 从 start 向上逐级找带 go.mod 的目录。
func findModuleRoot(start string) (string, bool) {
	dir := start
	for i := 0; i < maxModuleRootDepth; i++ {
		if st, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !st.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}
