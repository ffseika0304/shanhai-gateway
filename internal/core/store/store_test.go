package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// openTest 单元测试用的库：临时目录，进程退出由 testing 清理。
func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenCreatesOnlyUnifiedTables(t *testing.T) {
	s := openTest(t)
	rows, err := s.db.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var name string
		rows.Scan(&name)
		got[name] = true
	}
	for _, want := range []string{"attempts", "captures", "users", "sessions", "user_api_keys"} {
		if !got[want] {
			t.Errorf("表 %s 未建", want)
		}
	}
	// 三张被淘汰的表必须确认消失：qr_tickets（扫码登录已移除）、
	// counters/requests（attempts 的功能子集）。
	for _, gone := range []string{"qr_tickets", "counters", "requests"} {
		if got[gone] {
			t.Errorf("表 %s 不该再建", gone)
		}
	}
	// 关键约束在场：openid 唯一（同 subject 不能建两个用户）、
	// sessions.token 主键。
	if _, err := s.db.Exec("INSERT INTO users(openid, created_at) VALUES('dup', 1)"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := s.db.Exec("INSERT INTO users(openid, created_at) VALUES('dup', 2)"); err == nil {
		t.Error("users.openid 唯一约束缺失：重复插入竟然成功")
	}
	if err := s.storeNotNil(); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestOpenRejectsEmptyPathAndNilDB(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("空路径应报错")
	}
	if _, err := New(nil); err == nil {
		t.Error("nil *sql.DB 应报错")
	}
}

func TestUpsertUserAndSessionsCRUD(t *testing.T) {
	s := openTest(t)

	id1, err := s.UpsertUser("subject_a", "Alice")
	if err != nil {
		t.Fatalf("UpsertUser: %v", err)
	}
	id2, err := s.UpsertUser("subject_a", "Alice2")
	if err != nil {
		t.Fatalf("UpsertUser again: %v", err)
	}
	if id1 != id2 {
		t.Errorf("同 subject 应复用用户：got %d vs %d", id1, id2)
	}
	// 首建用户昵称会被后续非空昵称覆盖
	u, err := s.GetUser(id2)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.Nickname != "Alice2" || u.Quota != 1000 || u.Used != 0 {
		t.Errorf("用户字段不符: %+v", u)
	}
	// 空昵称不覆盖
	if _, err := s.UpsertUser("subject_a", ""); err != nil {
		t.Fatalf("UpsertUser empty nick: %v", err)
	}
	if u, _ := s.GetUser(id1); u.Nickname != "Alice2" {
		t.Errorf("空昵称不该覆盖昵称: %q", u.Nickname)
	}
	// 不存在的用户
	if _, err := s.GetUser(9999); !errors.Is(err, ErrUserMissing) {
		t.Errorf("缺用户应 ErrUserMissing, got %v", err)
	}

	// sessions：先建后查，删了就查不到
	now := float64(time.Now().Unix())
	if err := s.CreateSession("tok1", id1, now+3600, now); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	uid, exp, err := s.FindSession("tok1")
	if err != nil || uid != id1 || exp != now+3600 {
		t.Errorf("FindSession: uid=%d exp=%.0f err=%v", uid, exp, err)
	}
	if err := s.DeleteSession("tok1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, _, err := s.FindSession("tok1"); !errors.Is(err, ErrSessionInvalid) {
		t.Errorf("删后应 ErrSessionInvalid, got %v", err)
	}
}

func TestUserKeysCRUD(t *testing.T) {
	s := openTest(t)
	uid, _ := s.UpsertUser("subject_k", "Keys")

	// 5 把 key 全部落库（上限 5 是 HTTP 层策略，存储层只数）
	for i := 0; i < 5; i++ {
		if err := s.GenerateKey(uid, fmt.Sprintf("sk-user-%02d", i), fmt.Sprintf("key%d", i)); err != nil {
			t.Fatalf("GenerateKey %d: %v", i, err)
		}
	}
	n, err := s.CountKeys(uid)
	if err != nil || n != 5 {
		t.Errorf("CountKeys = %d, %v", n, err)
	}
	keys, err := s.ListKeys(uid)
	if err != nil || len(keys) != 5 {
		t.Fatalf("ListKeys = %d keys, %v", len(keys), err)
	}
	// ORDER BY id DESC：最新的排最前
	if keys[0].Key != "sk-user-04" || !keys[0].Enabled || keys[0].Name != "key4" {
		t.Errorf("ListKeys 顺序/字段不符: %+v", keys[0])
	}
	// 删一把自己的
	if err := s.DeleteKey(fmt.Sprintf("%d", keys[0].ID), uid); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	if n, _ := s.CountKeys(uid); n != 4 {
		t.Errorf("删除后 CountKeys = %d", n)
	}
	// 跨用户删除不得生效：other 删不掉 subject_k 名下的 id=2
	other, _ := s.UpsertUser("subject_o", "Other")
	if err := s.DeleteKey("2", other); err != nil {
		t.Fatalf("DeleteKey cross: %v", err)
	}
	if n, _ := s.CountKeys(uid); n != 4 {
		t.Errorf("跨用户删除不得生效, CountKeys = %d", n)
	}
}

func TestLookupAndConsumeUserKey(t *testing.T) {
	s := openTest(t)
	uid, _ := s.UpsertUser("user_x", "X")
	if err := s.GenerateKey(uid, "sk-user-ok", "ok"); err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	id, quota, used, err := s.LookupUserKey("sk-user-ok")
	if err != nil {
		t.Fatalf("LookupUserKey: %v", err)
	}
	if err != nil {
		t.Fatalf("LookupUserKey: %v", err)
	}
	if id != uid || quota != 1000 || used != 0 {
		t.Errorf("LookupUserKey = %d/%d/%d", id, quota, used)
	}
	// 额度顶满（存 used=quota）后由鉴权方拒绝；store 只回报告
	if _, err := s.db.Exec("UPDATE users SET used = quota WHERE id = ?", uid); err != nil {
		t.Fatalf("seed used=quota: %v", err)
	}
	if _, q, u, err := s.LookupUserKey("sk-user-ok"); err != nil || q != 1000 || u != 1000 {
		t.Errorf("额度顶满仍应查得到（判定在鉴权方）: q=%d u=%d err=%v", q, u, err)
	}
	// Consume +1
	if err := s.ConsumeUserKey("sk-user-ok"); err != nil {
		t.Fatalf("ConsumeUserKey: %v", err)
	}
	if _, _, u, _ := s.LookupUserKey("sk-user-ok"); u != 1001 {
		t.Errorf("used 应 +1, got %d", u)
	}
	// 未启用 / 不存在的 key
	if _, err := s.db.Exec("UPDATE user_api_keys SET enabled = 0 WHERE key = 'sk-user-ok'"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, _, _, err := s.LookupUserKey("sk-user-ok"); !errors.Is(err, ErrKeyUnknown) {
		t.Errorf("禁用 key 应 ErrKeyUnknown, got %v", err)
	}
	if _, _, _, err := s.LookupUserKey("sk-user-nope"); !errors.Is(err, ErrKeyUnknown) {
		t.Errorf("未知 key 应 ErrKeyUnknown, got %v", err)
	}
}

func TestAttemptsWriteAndRecentRing(t *testing.T) {
	s := openTest(t)

	// 溢出环形缓冲：只留最近 200 条，最新在前
	const n = 250
	for i := 0; i < n; i++ {
		s.Emit(Attempt{Rid: fmt.Sprintf("r%03d", i), Backend: "b", JobID: fmt.Sprintf("job%03d", i), OK: true})
	}
	rec := s.Recent()
	if len(rec) != recentRingSize {
		t.Fatalf("Recent len = %d, want %d", len(rec), recentRingSize)
	}
	if rec[0].Rid != "r249" {
		t.Errorf("最新应在最前: got %s", rec[0].Rid)
	}
	if rec[len(rec)-1].Rid != fmt.Sprintf("r%03d", n-recentRingSize) {
		t.Errorf("最旧应是第 %d 条: got %s", n-recentRingSize, rec[len(rec)-1].Rid)
	}

	// DB 侧：Start 消费通道 → 批量落库
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		s.Start(ctx, &buf)
		close(done)
	}()
	// 环形缓冲里的 250 条已同时入通道；等 flush（ticker 250ms × 2 冗余）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var total int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM attempts").Scan(&total); err == nil && total >= n {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	var total, withBackend int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM attempts").Scan(&total); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if total != n {
		t.Errorf("DB 里应有 %d 条 attempts, got %d", n, total)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM attempts WHERE backend='b'").Scan(&withBackend); err != nil || withBackend != n {
		t.Errorf("backend 列不符: %d %v", withBackend, err)
	}
	// job_id 列（2026-10-18 与 SLR 对齐）：Emit 带什么 DB 就该有什么
	var withJob int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM attempts WHERE job_id='job249'").Scan(&withJob); err != nil || withJob != 1 {
		t.Errorf("job_id 列不符: %d %v", withJob, err)
	}
	// 先收工再读 buf：worker 与测试主协程对 buf 的读写不并发（17:52 的 race）
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start 未随 ctx 退出")
	}
	// JSONL 出口
	if lines := strings.Count(buf.String(), "\n"); lines != n {
		t.Errorf("JSONL 应有 %d 行, got %d", n, lines)
	}
	// 谢幕 flush：通道里再塞几条，重新 Start 后 cancel，应当仍落库
	s.Emit(Attempt{Rid: "tail", OK: true})
	ctx2, cancel2 := context.WithCancel(context.Background())
	go s.Start(ctx2, nil)
	time.Sleep(100 * time.Millisecond)
	cancel2()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var cnt int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM attempts WHERE rid='tail'").Scan(&cnt); err == nil && cnt == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var cnt int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM attempts WHERE rid='tail'").Scan(&cnt); err != nil || cnt != 1 {
		t.Errorf("谢幕 flush 未落库: %d %v", cnt, err)
	}
	cancel2()
}

func TestEmitAndRecentRace(t *testing.T) {
	s := openTest(t)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s.Emit(Attempt{Rid: fmt.Sprintf("w%d-%d", w, i), OK: true})
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s.Recent()
			}
		}()
	}
	wg.Wait()
	// 无 race 报错即通过；顺手确认 Recent 有实质内容
	if got := len(s.Recent()); got == 0 {
		t.Error("Recent 为空")
	}
}

func TestWALModeOn(t *testing.T) {
	s := openTest(t)
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("期望 WAL, got %q", mode)
	}
}

func TestLookupUserKeyEnabledUserRequired(t *testing.T) {
	s := openTest(t)
	uid, _ := s.UpsertUser("blocked", "B")
	s.GenerateKey(uid, "sk-user-x", "")
	if _, err := s.db.Exec("UPDATE users SET enabled = 0 WHERE id = ?", uid); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, _, _, err := s.LookupUserKey("sk-user-x"); !errors.Is(err, ErrKeyUnknown) {
		t.Errorf("用户被禁用后 key 不可用, got %v", err)
	}
}

// TestCapturesWriteAndSemanticDefaults 验证 captures 留存写路径：
// EmitCapture → 后台 worker 批量落库；语义字段写死默认值兜住完整响应，
// 且 model 记 effModel 口径、backend/account/partial_ratio 都能 round-trip。
func TestCapturesWriteAndSemanticDefaults(t *testing.T) {
	s := openTest(t)
	const n = 30 // 略大于 captureFlushBatch(25)，触发批量阈值落库一次
	for i := 0; i < n; i++ {
		s.EmitCapture(Capture{
			Rid:          fmt.Sprintf("rid-%03d", i),
			Backend:      "volc_plan",
			Account:      "volc",
			Model:        "ark-code-latest", // effModel 口径
			ReqBody:      `{"messages":[{"role":"user","content":"hi"}]}`,
			RespBody:     `{"choices":[]}`,
			Stream:       true,
			Status:       200,
			LatMs:        900,
			PartialRatio: 0.42, // partial 前缀
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Start(ctx, nil)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var total int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM captures").Scan(&total); err == nil && total >= n {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM captures").Scan(&total); err != nil {
		t.Fatalf("count captures: %v", err)
	}
	if total != n {
		t.Fatalf("DB 应有 %d 条 captures, got %d", n, total)
	}
	// 抽查一条：effModel/backend/account 不在 join 时错行；语义默认值兜底正确
	var model, backend, account string
	var stream, status int
	var partialRatio float64
	var usable, truncated int
	if err := s.db.QueryRow(
		"SELECT model,backend,account,stream,status,partial_ratio,usable,truncated FROM captures WHERE rid='rid-000'",
	).Scan(&model, &backend, &account, &stream, &status, &partialRatio, &usable, &truncated); err != nil {
		t.Fatalf("scan capture: %v", err)
	}
	if model != "ark-code-latest" || backend != "volc_plan" || account != "volc" {
		t.Errorf("rid join 关键列不符: model=%q backend=%q account=%q", model, backend, account)
	}
	if stream != 1 || status != 200 {
		t.Errorf("stream/status round-trip: %d %d", stream, status)
	}
	if partialRatio != 0.42 {
		t.Errorf("partial_ratio 应为 0.42, got %v", partialRatio)
	}
	if usable != 0 {
		t.Errorf("partial 入库 usable 应默认不可用(0), got %d", usable)
	}
	if truncated != 0 {
		t.Errorf("未截断行 truncated 应为 0, got %d", truncated)
	}
	// 完整响应行：partial_ratio 默认 1.0（漏填时兜住完整语义）
	s.EmitCapture(Capture{Rid: "rid-full", Backend: "b2", Model: "m2"})
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var cnt int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM captures WHERE rid='rid-full'").Scan(&cnt); err == nil && cnt == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var pr float64
	if err := s.db.QueryRow("SELECT partial_ratio FROM captures WHERE rid='rid-full'").Scan(&pr); err != nil || pr != 1.0 {
		t.Errorf("完整响应 partial_ratio 默认应 1.0, got %v (err=%v)", pr, err)
	}
	var src string
	if err := s.db.QueryRow("SELECT usage_source FROM captures WHERE rid='rid-full'").Scan(&src); err != nil || src != "unavailable" {
		t.Errorf("漏填 UsageSource 默认应 'unavailable', got %q (err=%v)", src, err)
	}
	cancel()
}

// storeNotNil 只是给上面的测试里留个断言钩子（Store 空指针防护显式检查）。
func (s *Store) storeNotNil() error {
	if s == nil || s.db == nil {
		return errors.New("Store.db 为 nil")
	}
	return nil
}
