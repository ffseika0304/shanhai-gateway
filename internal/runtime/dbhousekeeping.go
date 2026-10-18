package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// defaultHousekeepInterval 是 interval <= 0 时的兜底周期。
// 十分钟一次足够压制 WAL 增长，又不至于在忙的时候给数据库添乱。
const defaultHousekeepInterval = 10 * time.Minute

// Housekeeper 定期给 SQLite 做保养：WAL checkpoint(TRUNCATE) + PRAGMA optimize。
//
// 为什么需要它：WAL 模式下一个跑了几天、没关过的进程会攒下巨大的 -wal 文件
// （读多写少的负载下尤其明显），而「PRAGMA optimize」缺失会让 SQLite 缺少
// 最新的统计信息、查询计划越跑越差。两者都只需要毫秒级。
//
// 为什么不做 VACUUM：VACUUM 要重写整个库文件，而且拿的是排他锁，
// 库有多大就锁多久。几 GB 的线上库跑一次 VACUUM 等于停服，
// 这笔买卖不划算。日常用 checkpoint + optimize 就够了。
type Housekeeper struct {
	db       *sql.DB
	interval time.Duration
	logger   *log.Logger

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
	wg        sync.WaitGroup
}

// NewHousekeeper 构造一个保养器。interval <= 0 时取默认周期。
//
// 之所以让调用方决定周期而不是写死：不同部署的写入量差别很大，
// 固定周期要么对高写入实例太频繁，要么对低写入实例太慢。
func NewHousekeeper(db *sql.DB, interval time.Duration) *Housekeeper {
	if interval <= 0 {
		interval = defaultHousekeepInterval
	}
	return &Housekeeper{
		db:       db,
		interval: interval,
		stop:     make(chan struct{}),
	}
}

// Start 在后台起一个 goroutine，按周期执行保养，直到 ctx 取消或 Stop 被调用。
//
// 不在 Start 里立刻跑一轮：进程刚起来时数据库连接可能还没预热，
// 此时做 TRUNCATE checkpoint 只会平白制造一次写放大。启动时的清理由 Sweep 负责。
// 重复调用是安全的，只会起一个 goroutine。
func (h *Housekeeper) Start(ctx context.Context) {
	if h == nil {
		return
	}
	h.startOnce.Do(func() {
		h.wg.Add(1)
		go func() {
			defer h.wg.Done()
			ticker := time.NewTicker(h.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-h.stop:
					return
				case <-ticker.C:
					// RunOnce 内部已吞掉并记录错误，这里不重复处理。
					_ = h.RunOnce()
				}
			}
		}()
	})
}

// Stop 停止后台循环并等待 goroutine 退出。可重复调用。
//
// 用 channel + WaitGroup 而不是 bool 标志：标志解决不了「Stop 在 Start 之前调用」
// 这类顺序问题，而 WaitGroup 的零计数会让等待立即返回，
// 先 Stop 后 Start 的 goroutine 也会因为 stop 已关闭而立刻退出。
func (h *Housekeeper) Stop() {
	if h == nil {
		return
	}
	h.stopOnce.Do(func() { close(h.stop) })
	h.wg.Wait()
}

// RunOnce 手动跑一轮保养。
//
// 出错只记日志、不 panic：保养是后台的锦上添花，
// 数据库正忙时 checkpoint 失败是常态，不该把整个进程带下去。
// 返回 error 是为了测试和「立即执行一次」的场景能观察到失败。
func (h *Housekeeper) RunOnce() error {
	if h == nil || h.db == nil {
		return fmt.Errorf("runtime: Housekeeper 未绑定数据库")
	}
	var errs []error

	// TRUNCATE 而不是 PASSIVE：前者把 -wal 真正缩回 0 字节并归还磁盘，
	// 后者只是搬走已提交页，文件照样一直涨。
	if _, err := h.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		h.logf("[runtime/db] WAL checkpoint 失败：%v", err)
		errs = append(errs, fmt.Errorf("wal_checkpoint: %w", err))
	}
	// 让 SQLite 重新分析表统计信息，刷新查询计划。
	if _, err := h.db.Exec(`PRAGMA optimize`); err != nil {
		h.logf("[runtime/db] optimize 失败：%v", err)
		errs = append(errs, fmt.Errorf("optimize: %w", err))
	}
	return errors.Join(errs...)
}

// logf 走可替换的 logger，未设置时退回标准库全局 logger。
func (h *Housekeeper) logf(format string, args ...any) {
	if h.logger != nil {
		h.logger.Printf(format, args...)
		return
	}
	log.Printf(format, args...)
}
