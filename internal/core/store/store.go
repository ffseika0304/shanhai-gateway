// Package store 是控制面的唯一存储层：一张 SQLite 库装全部表。
//
// 合并了两个来源的表：
//
//   - attempts          来自 seika_llm_router（请求 × 后端 × key 的明细统计，
//     含 rid/model/token，是 metrics 的唯一事实来源）
//   - users/sessions/user_api_keys 来自公版 gateway（控制面：登录会话 + 用户额度）
//
// 刻意**不建**的三张表：
//
//   - qr_tickets   小程序 / PC 扫码登录已在 Stage 0 移除，残留无意义
//   - counters     公版的键值计数器，是 attempts 的功能子集；合并后 metrics
//     只走 attempts，调用方需要 started 这类计数值时用 attempts 重造（后续批次）
//   - requests     同上，同样是 attempts 的子集
//
// 全局变量已消灭：这里没有任何包级 *sql.DB 句柄 —— 公版在包级声明后多处直接
// 引用，崩溃或写并发竞态时无法定位归属。改为一律 *Store 的方法，*sql.DB 内部
// 持有、由构造注入。
//
// 依赖方向：本包谁都不依赖（runtime 也不依赖——DB 路径由调用方传入，不自己拼），
// 属于 core 层，services 才可以依赖它。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	_ "modernc.org/sqlite"
)

// maxOpenConns 连接池上限。**决策记录，改之前先读**：
//
// 两个来源的旧代码都是 db.SetMaxOpenConns(1) + WAL + synchronous=NORMAL
// （seika_llm_router/go/db.go 与公版 gateway db.go 一字不差）。
// MaxOpenConns=1 把整个进程的所有 DB 操作串到一条连接上 —— 请求路径要写
// attempts（明细统计）、sessions、user quota，只要某个查询稍慢，后面所有请求
// 都排在一条连接后面；这正是网关高并发下最容易出现的假死模式。
//
// 我们这边 seika_llm_router 的 metrics.go 用的是另一极端：不设上限的连接池 +
// WAL + busy_timeout=8000，生产跑过没出锁问题（WAL 下读写不互斥，写靠
// SQLite 文件锁 + busy_timeout 排队）。
//
// 折中取 maxOpenConns=4：写仍要经 SQLite 文件锁串行（SQLite 物理上单写者，
// 这是存储引擎的事实，绕不开），但①应用层不再自己排队，慢查询不会拖死其它
// 请求的连接获取；②busy_timeout 把写锁竞争变成短暂等待而不是 SQLITE_BUSY
// 报错。注意 4 不是随便拍的：太大只会让写锁竞争更碎，没有收益。
const maxOpenConns = 4

// 一条 DDL 占一项而不是散着 Exec：表名单一来源，测试直接对这张表断言
// qr_tickets / counters / requests 确实没进来。
var schemas = []struct {
	name string
	ddl  string
}{
	// 用户表（照搬公版 db.go 原文，字段一字不改）
	{"users", `
		CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			openid TEXT UNIQUE NOT NULL,
			nickname TEXT DEFAULT '',
			avatar_url TEXT DEFAULT '',
			quota INTEGER DEFAULT 1000,
			used INTEGER DEFAULT 0,
			enabled INTEGER DEFAULT 1,
			created_at REAL NOT NULL,
			last_login_at REAL
		)
	`},
	// 会话表（照搬公版）
	{"sessions", `
		CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			expires_at REAL NOT NULL,
			created_at REAL NOT NULL
		)
	`},
	{"idx_sessions_user", "CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)"},
	// 用户专属 API Key（照搬公版）
	{"user_api_keys", `
		CREATE TABLE IF NOT EXISTS user_api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			key TEXT UNIQUE NOT NULL,
			name TEXT DEFAULT 'default',
			enabled INTEGER DEFAULT 1,
			created_at REAL NOT NULL
		)
	`},
	{"idx_uaiks_user", "CREATE INDEX IF NOT EXISTS idx_uaiks_user ON user_api_keys(user_id)"},
	// attempts 照搬 seika_llm_router metrics.go 的建表语句（counters 部分删掉）。
	// job_id（2026-10-18 加）与 SLR attempts 对齐：按 job 归因/缓存分析的事实键，
	// 迁移后 SLR 侧的分析脚本原样可用。已存在的库（shadow.db）走下方列迁移——
	// CREATE TABLE IF NOT EXISTS 对老库不加新列，这条注释里"本包是新建库、
	// 不需要迁移"的旧假设在 shadow-db 已在产后不再成立；"旧 attempts 库是
	// 另一个文件、另一个进程的事，不接管"指 SLR metrics.db，仍然成立。
	{"attempts", `
		CREATE TABLE IF NOT EXISTS attempts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts REAL NOT NULL,
			rid TEXT, backend TEXT, key_idx INTEGER, model TEXT,
			stream INTEGER, seq INTEGER, status INTEGER,
			lat_ms INTEGER, ft_ms INTEGER,
			class TEXT, err TEXT, ok INTEGER,
			prompt_tokens INTEGER DEFAULT 0,
			completion_tokens INTEGER DEFAULT 0,
			total_tokens INTEGER DEFAULT 0,
			cached_tokens INTEGER DEFAULT 0,
			job_id TEXT DEFAULT ''
		)
	`},
	{"idx_attempts_ts", "CREATE INDEX IF NOT EXISTS idx_attempts_ts ON attempts(ts)"},
	{"idx_attempts_backend", "CREATE INDEX IF NOT EXISTS idx_attempts_backend ON attempts(backend)"},
	// captures 是本次新增的「请求输入输出留存快照」表，与 attempts 彻底分离：
	// attempts=轻量统计，captures=重载原文（脱敏后 req/resp body），将来供
	// OpenViking/RAG 消费。关联契约：与 attempts 用 rid 对应。
	//
	// 语义字段默认值**写死**（不吃马上三态坑）：
	//   partial_ratio = 已累积程度，完整=1.0；usable=是否可直接消费，partial 一律 0，
	//   判据留给消费层（OpenViking）；truncated=tee 超上限截断。
	// 三者的默认值保证「漏填的行视为完整响应」，消费方不碰 NULL。
	{"captures", `
		CREATE TABLE IF NOT EXISTS captures (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts REAL NOT NULL,
			rid TEXT,
			backend TEXT,
			account TEXT,
			model TEXT,
			stream INTEGER,
			status INTEGER,
			lat_ms INTEGER,
			ft_ms INTEGER,
			prompt_tokens INTEGER DEFAULT 0,
			completion_tokens INTEGER DEFAULT 0,
			total_tokens INTEGER DEFAULT 0,
			cached_tokens INTEGER DEFAULT 0,
			req_body TEXT,
			resp_body TEXT,
			task_type TEXT,
			job_id TEXT,
			partial_ratio REAL NOT NULL DEFAULT 1.0,
			usable BOOLEAN NOT NULL DEFAULT 0,
			truncated BOOLEAN NOT NULL DEFAULT 0,
			usage_source TEXT NOT NULL DEFAULT 'unavailable'
		)
	`},
	{"idx_captures_rid", "CREATE INDEX IF NOT EXISTS idx_captures_rid ON captures(rid)"},
	{"idx_captures_ts", "CREATE INDEX IF NOT EXISTS idx_captures_ts ON captures(ts)"},
	{"idx_captures_backend", "CREATE INDEX IF NOT EXISTS idx_captures_backend ON captures(backend)"},
}

// Store 控制面存储。所有方法都挂在它上面，没有包级句柄。
type Store struct {
	db *sql.DB

	attemptCh chan Attempt // 来自 emitAttempt：通道满则丢统计不丢请求

	// 留存队列：captures 是旁路层。captureCh 满则丢（丢留存不拖请求），
	// captureDiscard 累计被丢条数，供 A8 丢弃日志监控。
	captureCh      chan Capture
	captureDiscard atomic.Int64

	recentMu  sync.Mutex
	recent    []Attempt // 最近请求环形缓冲（原 recentLog，200 条）
	recentPos int
}

// New 基于一个已有的 *sql.DB 组装 Store：设置连接池与 PRAGMA、建表。
// db 不接受 nil —— 静默的 nil 只会把 panic 推迟到第一个请求。
func New(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("store: *sql.DB 为 nil，无法组装 Store")
	}
	db.SetMaxOpenConns(maxOpenConns)
	// 两个来源的 PRAGMA 集合并集：WAL + synchronous=NORMAL 来自 db.go，
	// busy_timeout=8000 来自 metrics.go —— 后者在这套多连接池下是必需的。
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=8000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			return nil, fmt.Errorf("store: %s: %w", pragma, err)
		}
	}
	for _, sc := range schemas {
		if _, err := db.Exec(sc.ddl); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", sc.name, err)
		}
	}
	// 列迁移：已存在的库（shadow.db 在产中）靠 ALTER 补齐后加的列。
	// 先 PRAGMA 查列再 ALTER——迁移幂等，重启第二次不能 duplicate column 报错。
	for _, mc := range []struct{ table, col, ddl string }{
		{"attempts", "job_id", "ALTER TABLE attempts ADD COLUMN job_id TEXT DEFAULT ''"},
	} {
		rows, err := db.Query("PRAGMA table_info(" + mc.table + ")")
		if err != nil {
			return nil, fmt.Errorf("store: migrate probe %s: %w", mc.table, err)
		}
		has := false
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    any
			pk      int
		)
		for rows.Next() {
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: migrate scan %s: %w", mc.table, err)
			}
			if name == mc.col {
				has = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: migrate rows %s: %w", mc.table, err)
		}
		if !has {
			if _, err := db.Exec(mc.ddl); err != nil {
				return nil, fmt.Errorf("store: migrate %s add %s: %w", mc.table, mc.col, err)
			}
		}
	}
	return &Store{
		db:        db,
		attemptCh: make(chan Attempt, attemptQueueCap),
		recent:    make([]Attempt, recentRingSize),
		captureCh: make(chan Capture, captureQueueCap),
	}, nil
}

// Open 打开（必要时创建）path 处的 SQLite 库：建目录、连接、建表。
// 路径来自调用方（约定是 runtime.Paths.DBFile），本包不自己拼路径。
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: 库文件路径为空")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s, err := New(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭底层连接池。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Ping 探活：给接线层的启动自检用（旧代码 initDB 用 log.Fatalf 兜底，
// 包化后错误交回调用方）。
func (s *Store) Ping() error {
	if s == nil || s.db == nil {
		return errors.New("store: Store 未初始化")
	}
	return s.db.Ping()
}
