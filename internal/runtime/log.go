package runtime

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// defaultMaxBytes 单个日志文件 32MB。
	// 上限太大，磁盘被日志吃满是常见的线上事故；太小则日志被切得太碎，
	// 一次故障的上下文会被拦腰截断。32MB 大约相当于几万行，
	// 配合下面的保留代数，磁盘占用是封顶的。
	defaultMaxBytes int64 = 32 << 20

	// defaultKeep 保留 3 代（当前文件 + .1 + .2）。
	// 3 代够覆盖「出问题时回看昨晚」的常见诉求，再多只是白白占磁盘。
	defaultKeep = 3
)

// LogOptions 控制 RotatingWriter 的轮转行为。
type LogOptions struct {
	// MaxBytes 单文件字节上限；<=0 时取默认 32MB。
	MaxBytes int64

	// Keep 保留代数（含当前文件之外的历史代数）；<=0 时取默认 3。
	Keep int
}

// withDefaults 填零值。做成值方法而不是在构造函数里逐字段判断，
// 是为了让 RotatingWriter 和 Sweep 用同一套默认 —— 两边默认不一致的话，
// 「保留 3 代」的承诺会在清扫时被自己破坏掉。
func (o LogOptions) withDefaults() LogOptions {
	if o.MaxBytes <= 0 {
		o.MaxBytes = defaultMaxBytes
	}
	if o.Keep <= 0 {
		o.Keep = defaultKeep
	}
	return o
}

// RotatingWriter 是按大小轮转的 io.WriteCloser，可直接交给 log.SetOutput。
//
// 轮转命名：app.log → app.log.1 → app.log.2 …… 超过 Keep 的直接删除。
// 数字越大越老（.1 是上一代），这样最新的日志永远在固定路径，
// tail -F 不用改命令就能持续跟进。
type RotatingWriter struct {
	mu     sync.Mutex
	path   string
	opt    LogOptions
	file   *os.File
	size   int64
	closed bool
}

// NewRotatingWriter 打开（或创建）日志文件并返回轮转写入器。
//
// 失败一律返回 error，绝不 panic：日志初始化失败是运行期可预期的状况
// （磁盘满、目录只读、路径挂着一个普通文件），调用方需要有机会退回 stderr，
// 而不是整个进程崩掉。父目录不存在会自动创建。
func NewRotatingWriter(path string, opt LogOptions) (*RotatingWriter, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("runtime: 日志路径为空")
	}
	opt = opt.withDefaults()

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("runtime: 创建日志目录 %s 失败：%w", dir, err)
		}
	}
	// O_APPEND：进程重启后不会覆盖旧日志，且多个写入者的追加位置由内核保证，
	// 自己维护的 size 只用于判断何时轮转。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("runtime: 打开日志文件 %s 失败：%w", path, err)
	}

	var size int64
	if st, err := f.Stat(); err == nil {
		size = st.Size()
	}
	return &RotatingWriter{path: path, opt: opt, file: f, size: size}, nil
}

// Path 返回当前日志文件路径，便于调用方在启动日志里打印它。
func (w *RotatingWriter) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path
}

// Write 写入一段日志，必要时先轮转。
//
// 轮转判断放在写入之前（当前大小 + 本次写入 > 上限 则先轮转），
// 这样单文件永远不会越过上限。如果单条记录本身就大于上限，
// 那就让它撑爆一个新文件 —— 把一条日志拦腰截断比超限更糟。
func (w *RotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return 0, fmt.Errorf("runtime: 日志写入器 %s 已关闭", w.path)
	}
	if w.file == nil {
		return 0, fmt.Errorf("runtime: 日志文件 %s 未打开", w.path)
	}
	if w.size+int64(len(p)) > w.opt.MaxBytes {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}

	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

// Close 关闭底层文件。可重复调用。
func (w *RotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}
	w.closed = true
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// rotateLocked 执行一轮轮转：最老的先让位，再整体后移一代。
// 调用方必须已持有 w.mu。
func (w *RotatingWriter) rotateLocked() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return fmt.Errorf("runtime: 关闭日志文件 %s 失败：%w", w.path, err)
		}
		w.file = nil
	}

	// 第 Keep 代即将被淘汰，直接删除；随后 .(Keep-1) 依次后移。
	oldest := w.genPath(w.opt.Keep)
	if err := os.Remove(oldest); err != nil && !os.IsNotExist(err) {
		w.reopenLocked()
		return fmt.Errorf("runtime: 删除旧日志 %s 失败：%w", oldest, err)
	}
	for i := w.opt.Keep - 1; i >= 1; i-- {
		if err := os.Rename(w.genPath(i), w.genPath(i+1)); err != nil && !os.IsNotExist(err) {
			w.reopenLocked()
			return fmt.Errorf("runtime: 日志轮转 %s -> %s 失败：%w", w.genPath(i), w.genPath(i+1), err)
		}
	}
	if err := os.Rename(w.path, w.genPath(1)); err != nil && !os.IsNotExist(err) {
		w.reopenLocked()
		return fmt.Errorf("runtime: 日志轮转 %s -> %s 失败：%w", w.path, w.genPath(1), err)
	}

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("runtime: 轮转后重新打开 %s 失败：%w", w.path, err)
	}
	w.file = f
	w.size = 0
	return nil
}

// reopenLocked 在轮转出错后把写入器拉回可用状态。
// 宁可退回「继续往同一个文件追加、这次不轮转」，也不要让后续所有日志
// 因为一次 rename 失败而彻底丢失。
func (w *RotatingWriter) reopenLocked() {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		w.file = nil
		return
	}
	w.file = f
	w.size = 0
	if st, err := f.Stat(); err == nil {
		w.size = st.Size()
	}
}

// genPath 返回第 gen 代的文件名。gen 从 1 开始，数字越大越老。
func (w *RotatingWriter) genPath(gen int) string {
	return fmt.Sprintf("%s.%d", w.path, gen)
}

// 编译期确认它满足标准库日志所需的接口。
var _ io.WriteCloser = (*RotatingWriter)(nil)
