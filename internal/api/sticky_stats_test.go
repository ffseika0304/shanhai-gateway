package api

// 会话亲和观测（2026-10-11 接线）的统计形状钉死：/api/stats 的
// sticky_hits / sticky_misses / sticky_evictions 三个全局计数 + 每后端
// sticky_count。两种形态各钉一遍：
//   - 表 nil（SESSION_AFFINITY=off）→ 三个计数回零值、sticky_count=0，
//     JSON 形状与开态完全一致（前端不必分支）；
//   - 表非 nil → 计数与每后端 pinned 数取真实数据。

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"shanhai-gateway/internal/core/upstream"
	"shanhai-gateway/internal/service/affinity"
	"shanhai-gateway/internal/service/routing"
)

// statsBody 直接打 handleStats（不走 LAN 门：本测试只看形状），返回解码后的 JSON。
func statsBody(t *testing.T, h *Handlers) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	h.handleStats(rr, httptest.NewRequest("GET", "/api/stats", nil))
	if rr.Code != 200 {
		t.Fatalf("stats code=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("stats not JSON: %s", rr.Body.String())
	}
	return out
}

// stickyShape 前端要的三键必须在场（无论表 nil 与否，形状稳定）。
// affinityShape：2026-10-18 加的 SLR 同形对象（hits/misses/puts/pinned_jobs），
// 迁移期消费方无差别读两边，所以两套键都要钉住。
func stickyShape(t *testing.T, bd map[string]any) (uint64, uint64, uint64) {
	t.Helper()
	mustKeys := []string{"sticky_hits", "sticky_misses", "sticky_evictions", "backends", "affinity"}
	for _, k := range mustKeys {
		if _, has := bd[k]; !has {
			t.Fatalf("stats missing key %q: %v", k, bd)
		}
	}
	be := bd["backends"].([]any)
	if len(be) != 1 {
		t.Fatalf("backends=%v want 1 stub", be)
	}
	first := be[0].(map[string]any)
	sc, has := first["sticky_count"]
	if !has {
		t.Fatal("backend entry missing sticky_count")
	}
	return ui(bd["sticky_hits"]), ui(bd["sticky_misses"]), ui(sc)
}

func ui(v any) uint64 {
	f, _ := v.(float64)
	return uint64(f)
}

// affinityObj 取出 affinity 对象并钉住四个子键的形状。
func affinityObj(t *testing.T, bd map[string]any) map[string]any {
	t.Helper()
	obj, has := bd["affinity"].(map[string]any)
	if !has {
		t.Fatalf("affinity missing or not an object: %v", bd["affinity"])
	}
	for _, k := range []string{"hits", "misses", "puts", "pinned_jobs"} {
		if _, has := obj[k]; !has {
			t.Fatalf("affinity missing %q: %v", k, obj)
		}
	}
	return obj
}

func TestStatsStickyNilTableZeroShape(t *testing.T) {
	// 表 nil（默认/SESSION_AFFINITY=off）：三计数全 0，sticky_count 也在场为 0
	b := upstream.New("stub", "m", deadURL, nil)
	b.Enabled = true
	h := &Handlers{Router: routing.NewAtomicRouter(routing.New([]*upstream.Backend{b}))}
	bd := statsBody(t, h)
	hits, misses, ev := stickyShape(t, bd)
	if hits != 0 || misses != 0 || ev != 0 {
		t.Fatalf("nil table must report zeros, got hits=%d misses=%d evictions=%d", hits, misses, ev)
	}
	// affinity 对象同形状全零（前端/脚本不必分支）
	aff := affinityObj(t, bd)
	for _, k := range []string{"hits", "misses", "puts", "pinned_jobs"} {
		if got := ui(aff[k]); got != 0 {
			t.Fatalf("nil table affinity[%s]=%d want 0", k, got)
		}
	}
}

func TestStatsStickyRealTableRealNumbers(t *testing.T) {
	// 表非 nil：计数值与 CountPerBackend 进 JSON
	b := upstream.New("stub", "m", deadURL, nil)
	b.Enabled = true
	tbl := affinity.New(affinity.Options{})
	tbl.Put("sess-a", "stub")
	tbl.Put("sess-b", "stub")
	tbl.Put("sess-c", "other")
	tbl.Get("sess-a") // hit
	tbl.Get("ghost")  // miss
	tbl.Forget("sess-b")
	tbl.Forget("sess-c") // 2 evictions（sess-c 钉在缺席后端，看板只显示它自己有 1）
	h := &Handlers{Router: routing.NewAtomicRouter(routing.New([]*upstream.Backend{b})), Affinity: tbl}
	bd := statsBody(t, h)
	hits, misses, sc := stickyShape(t, bd)
	// CountPerBackend：sess-a 还钉在 stub（sess-b 已逐出）→ sticky_count=1；
	// sess-c 的钉在后端 "other" 上，stub 的看板只看得见自己名下的。
	if hits != 1 || misses != 1 || sc != 1 {
		t.Fatalf("sticky shape wrong: hits=%d misses=%d sticky_count=%d want 1/1/1", hits, misses, sc)
	}
	if got := ui(bd["sticky_evictions"]); got != 2 {
		t.Fatalf("sticky_evictions=%d want 2", got)
	}
	// affinity 对象：puts=3（三次 Put，同键/异键都计）、pinned_jobs=1
	// （a/b/c 三个钉，Forget 掉 b/c 后只剩 a；Len 在 CountPerBackend 清理
	// 之前取，与 SLR 的裸长度口径一致）。
	aff := affinityObj(t, bd)
	if got := ui(aff["puts"]); got != 3 {
		t.Fatalf("affinity.puts=%d want 3", got)
	}
	if got := ui(aff["pinned_jobs"]); got != 1 {
		t.Fatalf("affinity.pinned_jobs=%d want 1", got)
	}
}
