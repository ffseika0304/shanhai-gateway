package runtime

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// staleTempAge 判定临时文件「陈旧」的门槛。
// 之所以要留一小时的窗口：正在进行的下载、备份、打包会在临时文件上写很久，
// 启动时看到 .tmp 就删，等于打断用户自己正在做的事。一小时后还没人管，
// 那才是真正的垃圾。
const staleTempAge = time.Hour

// tempSuffixes 是清扫时认定的临时文件后缀。
var tempSuffixes = []string{".tmp", ".partial", ".swp", ".swx"}

// SweepResult 记录清扫实际删掉了什么。
//
// 返回清单而不是只返回一个错误，是为了可观测：启动日志里打一行
// 「清了 3 个临时文件 / 2 个超代日志」比一句「清扫完成」有用得多，
// 磁盘莫名其妙涨起来时这份清单就是线索。
type SweepResult struct {
	// RemovedTempFiles 是删掉的陈旧临时文件。
	RemovedTempFiles []string

	// RemovedOldLogs 是删掉的超出保留代数的轮转日志。
	RemovedOldLogs []string

	// RemovedOrphanDB 是删掉的孤儿 -wal / -shm（对应的 .db 已经不存在）。
	RemovedOrphanDB []string

	// RemovedEmptyDirs 是删掉的空目录。
	RemovedEmptyDirs []string
}

// Sweep 清扫 p.DataDir 之下的运行时垃圾，目标是跑完之后数据目录干净。
//
// 清理四类东西：
//  1. *.tmp / *.partial / *.swp 等临时文件，且修改时间超过一小时；
//  2. 超出 Keep 代数的轮转日志（app.log.4 之类）；
//  3. 孤儿 -wal / -shm：X-wal 存在但 X 不存在；
//  4. 空目录。
//
// 三条硬规则：
//   - 只在 DataDir 之下动刀，绝不碰 ConfigDir 和仓库目录。配置是用户手写的，
//     删掉就找不回来了；所以这里除了只遍历 DataDir，还额外显式跳过 ConfigDir
//     —— 万一有人用环境变量把 ConfigDir 放进了 DataDir（例如为了单目录部署）。
//   - 单个文件删除失败不中断整体，记一条日志后继续。清扫是锦上添花的
//     维护动作，不能因为一个删不掉的只读文件就让服务起不来。
//   - LogDir 本身不删。它是 Paths 的组成部分、紧接着就要往里写日志，
//     刚被 EnsureDirs 建出来又删掉没有意义。
//
// 只有「DataDir 不存在或不是目录」才返回 error：那属于调用顺序错误
// （应该先 EnsureDirs），需要让调用方看见。
func Sweep(p *Paths, opt LogOptions) (SweepResult, error) {
	var res SweepResult
	if p == nil {
		return res, fmt.Errorf("runtime: Paths 为 nil，无法清扫")
	}
	root := filepath.Clean(p.DataDir)
	if root == "" || root == "." {
		return res, fmt.Errorf("runtime: 数据目录无效（%q），请检查 %s", p.DataDir, envDataDir)
	}
	st, err := os.Stat(root)
	if err != nil {
		return res, fmt.Errorf("runtime: 访问数据目录 %s 失败：%w", root, err)
	}
	if !st.IsDir() {
		return res, fmt.Errorf("runtime: 数据目录 %s 不是目录", root)
	}
	opt = opt.withDefaults()

	// 显式的保护名单。ConfigDir 只在恰好落在 DataDir 里面时才需要拦截。
	protected := map[string]bool{root: true}
	if logDir := filepath.Clean(p.LogDir); logDir != "" {
		protected[logDir] = true
	}
	configDir := filepath.Clean(p.ConfigDir)
	configUnderRoot := configDir != "" && configDir != "." && isUnder(root, configDir)
	if configUnderRoot {
		protected[configDir] = true
	}

	// WalkDir 是字典序的：父目录一定排在所有子目录前面。
	// 收集下来倒序处理，空目录自底向上删才删得干净。
	var dirs []string

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// 某个条目读不到（比如中途被删）不该中断整体清扫。
			log.Printf("[runtime/sweep] 跳过 %s：%v", path, err)
			return nil
		}
		if configUnderRoot && (path == configDir || isUnder(configDir, path)) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if !protected[filepath.Clean(path)] {
				dirs = append(dirs, path)
			}
			return nil
		}

		name := d.Name()
		switch {
		case hasTempSuffix(name):
			// 只有够老的临时文件才算垃圾。
			info, ierr := d.Info()
			if ierr != nil || time.Since(info.ModTime()) < staleTempAge {
				return nil
			}
			removeCollect(path, &res.RemovedTempFiles)
		case isLogGeneration(name, opt.Keep):
			removeCollect(path, &res.RemovedOldLogs)
		case isSidecarName(name):
			// X-wal / X-shm 必须有 X 才有效。X 没了说明数据库被删了或搬走了，
			// 留着只会白白占空间（WAL 很容易长到几十上百 MB）。
			if _, serr := os.Stat(sidecarBase(path, name)); serr != nil && os.IsNotExist(serr) {
				removeCollect(path, &res.RemovedOrphanDB)
			}
		}
		return nil
	})
	if walkErr != nil {
		return res, fmt.Errorf("runtime: 遍历数据目录 %s 失败：%w", root, walkErr)
	}

	for i := len(dirs) - 1; i >= 0; i-- {
		dir := dirs[i]
		if err := os.Remove(dir); err == nil {
			res.RemovedEmptyDirs = append(res.RemovedEmptyDirs, dir)
		}
		// 非空说明还有东西在用，Remove 返回 ENOTEMPTY 是正常结果，不是错误。
	}
	return res, nil
}

// removeCollect 删除单个文件，成功才记进清单。失败只记日志，不中断整体。
func removeCollect(path string, into *[]string) {
	if err := os.Remove(path); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[runtime/sweep] 删除 %s 失败：%v", path, err)
		}
		return
	}
	*into = append(*into, path)
}

// hasTempSuffix 判断文件名是否是约定的临时文件。
func hasTempSuffix(name string) bool {
	for _, suffix := range tempSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// isLogGeneration 判断文件名是否是超出保留代数的轮转日志
// （形如 app.log.4，必须以 .log 结尾 + 数字）。
func isLogGeneration(name string, keep int) bool {
	gen, ok := logGeneration(name)
	return ok && gen > keep
}

// logGeneration 解析轮转代数，形如 "app.log.3" 返回 3。
// 要求 .log 前缀是为了只命中 RotatingWriter 产生的名字，
// 避免把 "backup.2024" 之类的普通文件名误判成日志代数。
func logGeneration(name string) (int, bool) {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 {
		return 0, false
	}
	gen, err := strconv.Atoi(name[i+1:])
	if err != nil || gen < 1 {
		return 0, false
	}
	if !strings.HasSuffix(name[:i], ".log") {
		return 0, false
	}
	return gen, true
}

// isSidecarName 判断是否是 SQLite 的伴生文件（-wal / -shm）。
func isSidecarName(name string) bool {
	return strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm")
}

// sidecarBase 去掉 -wal / -shm 后缀，得到主库文件名。
// 必须按固定后缀裁剪：filepath.Ext("app.db-wal") 返回的是 ".db-wal"，
// 用它会把文件名削成 "app"，孤儿判定就全错了。
func sidecarBase(path, name string) string {
	base := strings.TrimSuffix(name, "-wal")
	base = strings.TrimSuffix(base, "-shm")
	return filepath.Join(filepath.Dir(path), base)
}

// isUnder 判断 path 是否等于 root 或位于 root 之下。
// 参数顺序是 (root, path)：写反了在 Sweep 里会表现为「整个数据目录被跳过」，
// 是那种不报任何错、只是什么都不删的静默 bug。
func isUnder(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
