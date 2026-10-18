// Package runtime 是合并后代码库的依赖地基。
//
// 它只承担两件事，且只依赖标准库（外加 modernc.org/sqlite，仅 dbhousekeeping 使用）：
//
//  1. 项目身份 —— AppName / AppVersion 是身份的唯一来源。改名、换域名、对外开源
//     只需要改 identity.go 这一个文件。
//  2. 运行时产物管理 —— 数据/日志/配置落在哪、日志怎么轮转、启动时清扫什么、
//     SQLite 怎么定期保养。
//
// 其它包一律通过这里取路径，不允许自己 filepath.Join 拼绝对路径。
// 原因很实际：手拼的路径既不能被环境变量覆盖，也无法在测试里重定向到临时目录，
// 于是所有依赖它的测试都必须往真实路径写东西 —— 这正是当前代码库里
// 产物散落、测试不敢跑的历史由来。
package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	// AppName 是本程序的名字。它参与数据目录名、数据库文件名和日志文件名，
	// 因此必须保持中性、可开源：不要塞进作者名或旧项目名。
	AppName = "shg"

	// AppVersion 与发布版本号同步更新。
	// 用户已经有一份安装，只需要改这里就能让版本号跟着走遍
	// User-Agent、/healthz 和日志前缀，不用全局搜字符串。
	AppVersion = "0.5.1"
)

// AppID 返回 "gateway/0.4.0" 形式的标识，用作 UA、日志前缀、指标标签。
//
// 之所以做成函数而不是再加一个常量：常量需要人工同步两处，漏改一处就会
// 让日志和 UA 对不上；这里只留一个拼装点，改版本号不会漏。
func AppID() string {
	return AppName + "/" + AppVersion
}

// DefaultDataDir 返回用户级数据目录，在 XDG 规范下是 ~/.local/share/<AppName>。
//
// 为什么不用「程序目录 + data」：可执行文件所在目录在打包、systemd、容器里
// 含义各不相同，可能是只读镜像层，也可能在升级时被整体替换，写进去要么失败、
// 要么重启就丢。用户级数据目录是 Linux 上唯一在各种部署形态下都稳定可写、
// 且升级不动的位置。
//
// 先读 XDG_DATA_HOME（XDG 规范要求的高优先级），未设置时回退到
// $HOME/.local/share；非绝对路径的 XDG_DATA_HOME 按规范忽略。
// 两者都没有时返回错误：此时宁可让调用方明确失败，也不要偷偷退回当前目录，
// 把产物写到一个用户根本想不到的地方。
func DefaultDataDir() (string, error) {
	if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, AppName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("runtime: 定位数据目录失败：HOME 未设置（%w）", err)
	}
	return filepath.Join(home, ".local", "share", AppName), nil
}
