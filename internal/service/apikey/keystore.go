// Package apikey 是下游 /v1 API key 的鉴权与额度。
//
// 覆盖两类 key，合并自公版 main.go 的 KeyStore 段：
//
//   - sk-user-*   控制面用户专属 key：走 DB（user_api_keys JOIN users），检查
//     key 与用户都 enabled，再核额度（quota<0 = 不限）。DB 访问通过
//     UserKeyLookup 接口注入 —— 本包**不 import core/store 的具体类型**，
//     缘由见接口注释。
//   - 其余 key    管理员静态 key：keys.json 一次性读入内存 map，mtime 变了
//     才重读（reloadIfChanged 原样保留，导出为 ReloadIfChanged 供代理路径
//     每请求调用一次 —— 与旧 proxyHandler 的调用位置一致）。
//
// 文件路径不自己拼：NewKeyStore 收 *runtime.Paths，只认 Paths.KeysFile ——
// 路径唯一来源于 runtime（旧代码的 KEYS_FILE env 读取散在 main 里，接线时由
// runtime.Paths 的 env 覆盖机制承担）。
//
// 依赖方向：本包 → internal/runtime + 标准库。与 service/auth 平级、互不依赖。
package apikey

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shanhaigateway/internal/runtime"
)

// UserKeyLookup 是本包对「用户 key 存取」需要的最小能力面。
//
// 为什么接口是这两个方法、为什么由 core/store 实现而不是 import store 类型：
//   - KeyStore 对用户 key 的一切认知只有两件事：①这把 key 存不存在且
//     enabled、额度/已用多少（LookupUserKey）；②响应成功后 used+1
//     （ConsumeUserKey）。除此之外不关心 users/user_api_keys 的 schema、
//     昵称、session —— 拿具体 *store.Store 类型会把两件无关的东西焊死，
//     且是 service 层越过 service 边界挑 schema 的开头。
//   - 依赖方向硬性要求 core 不反向依赖 service；由 store 实现接口、apikey
//     持有接口，实现细节留在实现方，本包测试也只需一个 map 假实现。
//   - 方法名与 core/store 的方法一致（ LookupUserKey/ConsumeUserKey ），
//     接线一行、不出第二套词汇。
type UserKeyLookup interface {
	// LookupUserKey 查用户 key：不存在 / key 或用户被禁用 → 返回非 nil err。
	// quota<0 表示不限额；used>=quota 的**判定**在 Auth 里做（见 quotaExceeded），
	// 判定归属保持公版原位（KeyStore.Auth 拒绝），存储层只回报告。
	LookupUserKey(key string) (userID, quota, used int64, err error)
	// ConsumeUserKey 成功响应后 used+1。
	ConsumeUserKey(key string) error
}

// APIKey 一把静态 key（keys.json 里的条目，公版原文）。
type APIKey struct {
	Name    string `json:"name"`
	Key     string `json:"key"`
	Quota   int    `json:"quota"`
	Used    int64  `json:"used"`
	Enabled bool   `json:"enabled"`
}

// KeyStore 下游鉴权入口：用户 key（DB，经接口）+ 管理员 key（keys.json）。
// users 可为 nil（纯 keys.json 部署），此时 sk-user-* 一律拒绝 —— 与旧
// 「查库无行」走同一分支。
type KeyStore struct {
	users UserKeyLookup

	mu   sync.RWMutex
	keys map[string]*APIKey
	file string
	mod  time.Time
}

// NewKeyStore 组装：路径只认 paths.KeysFile，不自己拼。
func NewKeyStore(paths *runtime.Paths, users UserKeyLookup) (*KeyStore, error) {
	if paths == nil {
		return nil, errors.New("apikey: runtime.Paths 为 nil")
	}
	if strings.TrimSpace(paths.KeysFile) == "" {
		return nil, errors.New("apikey: Paths.KeysFile 为空")
	}
	ks := &KeyStore{users: users, keys: make(map[string]*APIKey), file: paths.KeysFile}
	ks.load()
	return ks, nil
}

// load 一次性读 keys.json（照搬公版，log 语句原样保留）。
func (ks *KeyStore) load() {
	data, err := os.ReadFile(ks.file)
	if err != nil {
		log.Printf("[keystore] load error: %v", err)
		return
	}
	fi, _ := os.Stat(ks.file)
	var keys []*APIKey
	if err := json.Unmarshal(data, &keys); err != nil {
		log.Printf("[keystore] parse error: %v", err)
		return
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	ks.keys = make(map[string]*APIKey)
	for _, k := range keys {
		ks.keys[k.Key] = k
	}
	ks.mod = fi.ModTime()
	log.Printf("[keystore] loaded %d keys", len(keys))
}

// ReloadIfChanged mtime 热加载（原 reloadIfChanged，导出化）。
// 代理路径每请求先调它再 Auth —— keys.json 属于管理员手改的文件，
// SIGHUP/重启都不值得为之停下来。
func (ks *KeyStore) ReloadIfChanged() {
	fi, err := os.Stat(ks.file)
	if err != nil {
		return
	}
	ks.mu.RLock()
	needReload := fi.ModTime().After(ks.mod)
	ks.mu.RUnlock()
	if needReload {
		ks.load()
	}
}

// Auth 鉴权一把下游 key。sk-user-* 走 DB 分支（经接口），其余走 keys.json。
func (ks *KeyStore) Auth(key string) (*APIKey, bool) {
	// 先查用户专属 Key（sk-user-*）
	if strings.HasPrefix(key, "sk-user-") {
		if ks.users == nil {
			return nil, false
		}
		userID, quota, used, err := ks.users.LookupUserKey(key)
		if err != nil {
			return nil, false
		}
		if quotaExceeded(quota, used) {
			return nil, false
		}
		return &APIKey{
			Name:    fmt.Sprintf("user_%d", userID),
			Key:     key,
			Quota:   int(quota),
			Used:    used,
			Enabled: true,
		}, true
	}
	// 回退到管理员 Key（keys.json）
	ks.mu.RLock()
	defer ks.mu.RUnlock()
	k, ok := ks.keys[key]
	if !ok || !k.Enabled {
		return nil, false
	}
	if quotaExceeded(int64(k.Quota), k.Used) {
		return nil, false
	}
	return k, true
}

// Consume 请求成功后扣额度。
func (ks *KeyStore) Consume(key string) {
	// 用户 Key 扣额度写回 DB
	if strings.HasPrefix(key, "sk-user-") {
		if ks.users != nil {
			ks.users.ConsumeUserKey(key)
		}
		return
	}
	// 管理员 Key 内存扣
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if k, ok := ks.keys[key]; ok {
		atomic.AddInt64(&k.Used, 1)
	}
}
