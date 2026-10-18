package apikey

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"shanhaigateway/internal/runtime"
)

// mockLookup 记账式假实现：只存 (userID, quota, used)，并数 Consume 调用。
type mockLookup struct {
	mu       sync.Mutex
	entries  map[string][3]int64 // key → {userID, quota, used}
	consumed map[string]int
}

func newMock() *mockLookup {
	return &mockLookup{entries: map[string][3]int64{}, consumed: map[string]int{}}
}

func (m *mockLookup) put(key string, userID, quota, used int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = [3]int64{userID, quota, used}
}

func (m *mockLookup) LookupUserKey(key string) (int64, int64, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return 0, 0, 0, errors.New("no such key")
	}
	return e[0], e[1], e[2], nil
}

func (m *mockLookup) ConsumeUserKey(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[key]; !ok {
		return errors.New("no such key")
	}
	m.consumed[key]++
	return nil
}

func (m *mockLookup) consumeCount(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.consumed[key]
}

func pathsFor(t *testing.T, file string) *runtime.Paths {
	t.Helper()
	return &runtime.Paths{KeysFile: file, DataDir: t.TempDir(), ConfigDir: t.TempDir()}
}

func writeKeysFile(t *testing.T, body string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatalf("write keys.json: %v", err)
	}
	return f
}

func keysJSONFixture() string {
	return `[
		{"name":"primary","key":"sk-admin-primary","quota":10,"used":0,"enabled":true},
		{"name":"unlimit","key":"sk-admin-unlimit","quota":-1,"used":0,"enabled":true},
		{"name":"spent","key":"sk-admin-spent","quota":5,"used":5,"enabled":true},
		{"name":"off","key":"sk-admin-off","quota":100,"used":0,"enabled":false}
	]`
}

func TestNewKeyStoreGuards(t *testing.T) {
	if _, err := NewKeyStore(nil, nil); err == nil {
		t.Error("nil Paths 应报错")
	}
	if _, err := NewKeyStore(&runtime.Paths{}, nil); err == nil {
		t.Error("空 KeysFile 应报错")
	}
}

func TestStaticKeysAuthQuotaAndConsume(t *testing.T) {
	f := writeKeysFile(t, keysJSONFixture())
	ks, err := NewKeyStore(pathsFor(t, f), newMock())
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}

	// 正常 key
	k, ok := ks.Auth("sk-admin-primary")
	if !ok || k == nil || k.Name != "primary" || k.Quota != 10 {
		t.Errorf("Auth(primary) = %v %v", k, ok)
	}
	// 不限额（quota=-1）
	if _, ok := ks.Auth("sk-admin-unlimit"); !ok {
		t.Error("quota=-1 应视为不限额、放行")
	}
	// 额度用满 → 拒绝
	if _, ok := ks.Auth("sk-admin-spent"); ok {
		t.Error("used>=quota 应拒绝")
	}
	// 禁用 → 拒绝
	if _, ok := ks.Auth("sk-admin-off"); ok {
		t.Error("enabled=false 应拒绝")
	}
	// 不存在 → 拒绝
	if _, ok := ks.Auth("sk-admin-nope"); ok {
		t.Error("未知 key 应拒绝")
	}
	// Consume 内存扣
	ks.Consume("sk-admin-primary")
	ks.Consume("sk-admin-primary")
	ks.Consume("sk-admin-nope") // 无副作用
	ks.mu.RLock()
	if got := ks.keys["sk-admin-primary"].Used; got != 2 {
		t.Errorf("两次 Consume 后 used = %d", got)
	}
	ks.mu.RUnlock()
}

func TestUserKeyBranchWithMockStore(t *testing.T) {
	// sk-user-* 走接口分支：与 mock 的交互即「store 交互」的替身
	mock := newMock()
	mock.put("sk-user-quota", 7, 100, 100) // 已用满
	mock.put("sk-user-ok", 42, 1000, 12)
	mock.put("sk-user-unlimit", 8, -1, 1<<40)

	f := writeKeysFile(t, keysJSONFixture())
	ks, err := NewKeyStore(pathsFor(t, f), mock)
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}

	// 用满 → 拒绝（恰在 used==quota）
	if _, ok := ks.Auth("sk-user-quota"); ok {
		t.Error("used==quota 应拒绝")
	}
	// 不限额 → 放行
	if k, ok := ks.Auth("sk-user-unlimit"); !ok || k.Quota != -1 {
		t.Errorf("quota=-1 应放行: %v %v", k, ok)
	}
	// 正常 → 放行，返回的 APIKey 带上用户号
	k, ok := ks.Auth("sk-user-ok")
	if !ok || k == nil || k.Name != "user_42" || k.Quota != 1000 || k.Used != 12 {
		t.Errorf("Auth(sk-user-ok) = %+v %v", k, ok)
	}
	// Consume → 调了接口的 ConsumeUserKey
	ks.Consume("sk-user-ok")
	if n := mock.consumeCount("sk-user-ok"); n != 1 {
		t.Errorf("ConsumeUserKey 应被调 1 次, got %d", n)
	}
	// keys.json 的 key 不得走 DB 分支
	if _, ok := ks.Auth("sk-admin-primary"); !ok {
		t.Error("静态 key 不得被分支误伤")
	}
	// mock 里没有的 sk-user key → 拒绝
	if _, ok := ks.Auth("sk-user-ghost"); ok {
		t.Error("查无此用户 key 应拒绝")
	}
}

func TestReloadIfChanged(t *testing.T) {
	f := writeKeysFile(t, keysJSONFixture())
	ks, err := NewKeyStore(pathsFor(t, f), newMock())
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	// 改写文件：换掉 primary、加一把新的
	time.Sleep(10 * time.Millisecond) // 保证 mtime 有机会前进
	newer := `[{"name":"v2","key":"sk-admin-newkey","quota":-1,"used":0,"enabled":true}]`
	if err := os.WriteFile(f, []byte(newer), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	// 文件系统 mtime 粒度不可靠，显式推 mtime
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(f, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	ks.ReloadIfChanged()
	if _, ok := ks.Auth("sk-admin-newkey"); !ok {
		t.Error("热加载后新 key 应可用")
	}
	if _, ok := ks.Auth("sk-admin-primary"); ok {
		t.Error("旧 key 旧文件已被替换，不应可用")
	}
}

func TestReloadIfChangedMissingFile(t *testing.T) {
	// 文件缺失静默跳过（公版原语义：仅打日志的 load error，不炸）
	f := filepath.Join(t.TempDir(), "absent.json")
	ks, err := NewKeyStore(pathsFor(t, f), nil)
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	if _, ok := ks.Auth("anything"); ok {
		t.Error("文件缺失时不应有可用 key")
	}
	ks.ReloadIfChanged() // 不应 panic
	if _, ok := ks.Auth("anything"); ok {
		t.Error("热加载缺文件后不应有可用 key")
	}
}

func TestNilLookupRejectsUserKeys(t *testing.T) {
	// 纯 keys.json 部署：sk-user-* 一律拒绝而不是 panic
	f := writeKeysFile(t, fmt.Sprintf(`[{"name":"a","key":"sk-admin-a","quota":-1,"used":0,"enabled":true}]`))
	ks, err := NewKeyStore(pathsFor(t, f), nil)
	if err != nil {
		t.Fatalf("NewKeyStore: %v", err)
	}
	if _, ok := ks.Auth("sk-user-anything"); ok {
		t.Error("无 Lookup 时 sk-user-* 应拒绝")
	}
	ks.Consume("sk-user-anything") // 不应 panic
}
