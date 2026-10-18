// 会话亲和接线的测试（2026-10-11）。纪律沿 proxy_test.go：
// 全部 httptest 打桩上游，绝无真实外发。这里专门审三条主线：
//
//  1. Affinity=nil 时行为与未接亲和逐位一致（SWRR 轮转原样）；
//  2. 钉住：同一 X-Session-Key 连发不换 backend（救 provider 侧前缀缓存）；
//  3. 逐出/保持判定走 errclass.ReleaseAffinity（429 拆、400/取消/mid-stream
//     不拆）、满载只滞回计数、连续 K 次才迁移、只有最终 200 写表。
package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"shanhaigateway/internal/core/errclass"
	"shanhaigateway/internal/core/upstream"
	"shanhaigateway/internal/service/affinity"
)

// mkAffHandler 在 mkHandler 之上把亲和表装上（表的构造路径与生产装配一致：
// Options.Affinity 指向一张真的 affinity.Table）。
func mkAffHandler(t *testing.T, opt Options, aff affinity.Options, backends ...*upstream.Backend) (http.HandlerFunc, *affinity.Table) {
	t.Helper()
	tbl := affinity.New(aff)
	opt.Affinity = tbl
	h, st, _ := mkHandler(t, opt, backends...)
	_ = st
	return h, tbl
}

// servedBy 发一个（可选带 X-Session-Key 的）请求，接住状态码与 X-Served-By。
func servedBy(h http.HandlerFunc, t *testing.T, sess, body string) (code int, by string) {
	t.Helper()
	r := postReq(t, body)
	if sess != "" {
		r.Header.Set("X-Session-Key", sess)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w.Code, w.Header().Get("X-Served-By")
}

const reqBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
const streamBody = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// ── ① nil 回归：亲和表缺席时，轮转行为与未接亲和逐位一致 ────────────
// 两个等权后端：SWRR 严格轮转 b1,b2,b1 —— 这是「没有亲和」时的地面真相。
// （其余 13 条既有 proxy 测试本身就跑在 Affinity=nil 下，是更全的回归集。）
func TestAffinityNilKeepsSWRRRotation(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	b1 := mkBackend("rota-1", up.srv.URL, 2)
	b2 := mkBackend("rota-2", up.srv.URL, 2)
	h, _, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, b1, b2)

	for i, want := range []string{"rota-1", "rota-2", "rota-1"} {
		_, by := servedBy(h, t, "sess-nil", reqBody)
		if by != want {
			t.Fatalf("request %d served by %q, want %q（nil 亲和时必须仍走 SWRR 轮转）", i+1, by, want)
		}
	}
}

// ── ② 钉住：同一 key 连发全部命中同一后端（低权重也钉得住）──────────
func TestAffinityPinsSameBackend(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	b1 := mkBackend("w04-be", up.srv.URL, 4)
	b1.Weight = 0.4
	b2 := mkBackend("w10-be", up.srv.URL, 4)
	b2.Weight = 1.0

	h, tbl := mkAffHandler(t, Options{QueueWait: 50 * time.Millisecond}, affinity.Options{}, b1, b2)

	// 首发落谁由 SWRR 决定（权重高的 w10-be 先领回合），这一发建立粘性：
	_, first := servedBy(h, t, "sess-pin", reqBody)
	if first == "" {
		t.Fatal("first request got no X-Served-By")
	}
	// 之后同一会话连发 3 次：全部钉回同一后端（weight=0.4 的那家也钉得住）
	for i := 1; i <= 3; i++ {
		_, by := servedBy(h, t, "sess-pin", reqBody)
		if by != first {
			t.Fatalf("request %d served by %q, want pinned %q（weight=0.4 也是钉住的）", i+1, by, first)
		}
	}
	// 换一个会话 key：与粘性无关（亲和只对同 key 生效）
	if _, by := servedBy(h, t, "sess-other", reqBody); by == "" {
		t.Fatal("other session request must be served")
	}
	// 观测口：3 次钉住命中 + 2 次未命中（首发那一发 + other 那一发）
	st := tbl.Stats()
	if st.Hits != 3 || st.Misses != 2 {
		t.Fatalf("sticky counters hits=%d misses=%d want 3/2", st.Hits, st.Misses)
	}
	if cnt := tbl.CountPerBackend()[first]; cnt != 1 {
		t.Fatalf("CountPerBackend[%q]=%d want 1（看板 sticky_count 数据源）", first, cnt)
	}
}

// ── ③ 目标被健康层过滤：本次不钉；成功后写的是新地方，不写回旧值 ────
func TestAffinitySkipsUnhealthyTarget(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	b1 := mkBackend("health-1", up.srv.URL, 2)
	b2 := mkBackend("health-2", up.srv.URL, 2)

	h, tbl := mkAffHandler(t, Options{QueueWait: 50 * time.Millisecond}, affinity.Options{}, b1, b2)

	if _, first := servedBy(h, t, "sess-health", reqBody); first == "" {
		t.Fatal("first request not served")
	}
	pinned, _, ok := tbl.Get("sess-health")
	if !ok || pinned == "" {
		t.Fatalf("pin not established: (%q,%v)", pinned, ok)
	}
	// 目标进了冷却 → usable 把它过滤掉 → 本次不钉；请求落到另一家。
	// 表里那条钉**保留**（等冷却回来还能接着命中局部性），只是不被使用。
	b1.SetCooldown(10*time.Second, errclass.RateLimit429)
	_, second := servedBy(h, t, "sess-health", reqBody)
	if second == "" || second == pinned {
		t.Fatalf("cooling target must not be served again: old=%q got=%q", pinned, second)
	}
	// 最终成功按实际接应方写表：旧值被覆盖，而不是把旧值再写一遍。
	now, _, ok := tbl.Get("sess-health")
	if !ok || now != second {
		t.Fatalf("table points at (%q,%v), want %q —— 旧值不得写回", now, ok, second)
	}
}

// ── ④ 满载滞回：TryAcquire 失败 → contention++ 且粘性保持；连续 K 次才迁移 ─
func TestAffinityFullLoadContendsThenMigrates(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	b := mkBackend("solo-be", up.srv.URL, 1)

	h, tbl := mkAffHandler(t, Options{QueueWait: 60 * time.Millisecond}, affinity.Options{}, b)

	// 先正常成功一次，建立粘性
	if code, _ := servedBy(h, t, "sess-full", reqBody); code != 200 {
		t.Fatalf("first request code=%d", code)
	}
	if pinned, _, ok := tbl.Get("sess-full"); !ok || pinned != "solo-be" {
		t.Fatalf("pin not established: (%q,%v)", pinned, ok)
	}
	// 满载 K 次（K=3 默认）：外部占住唯一并发位，每次选序点 probe 失败
	// → contention++ 且粘性保持；QueueWait 短，排队落空回 429（语义照旧：
	// 满载排队不是降级，60s 排队不当 backend fault，只做滞回计数）。
	for i := 1; i <= 3; i++ {
		if !b.TryAcquire() {
			t.Fatal("pre acquire failed")
		}
		code, _ := servedBy(h, t, "sess-full", reqBody)
		if code != http.StatusTooManyRequests {
			t.Fatalf("saturated request should 429, got %d", code)
		}
		if i < 3 {
			// 滞回未达阈值：粘性保持，consecutive=1..2
			pinned, c, ok := tbl.Get("sess-full")
			if !ok || pinned != "solo-be" || c != i {
				t.Fatalf("contention %d: sticky must be KEPT, got (%q,%d,%v)", i, pinned, c, ok)
			}
		} else {
			// 第 3 次满载 → 达 AFFINITY_CONTENTION_LIMIT：迁移（拆钉）
			if _, _, ok := tbl.Get("sess-full"); ok {
				t.Fatal("3 consecutive contentions must migrate (drop the pin)")
			}
			if got := tbl.Stats().Evictions; got != 1 {
				t.Fatalf("migration evictions=%d want 1", got)
			}
		}
		b.Release()
	}
	// 迁移之后：空位上重新首发成功 → 表重新钉上新地方（滞回计清零）
	if code, _ := servedBy(h, t, "sess-full", reqBody); code != 200 {
		t.Fatalf("post-migration request code=%d", code)
	}
	if pinned, c, ok := tbl.Get("sess-full"); !ok || pinned != "solo-be" || c != 0 {
		t.Fatalf("re-pinned after migration: (%q,%d,%v)", pinned, c, ok)
	}
}

// ── ⑤ 只有最终 200 写表：第一发 429、failover 第二发 200 → 表指向第二家 ─
func TestAffinityPutOnlyOnFinalSuccess(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	// 打桩两路：第一路永远 429，第二路永远 200
	up429 := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	})
	upOK := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
		fmt.Fprint(w, okJSON)
	})
	b1 := mkBackend("first-429", up429.srv.URL, 2)
	b2 := mkBackend("second-200", upOK.srv.URL, 2)

	h, tbl := mkAffHandler(t, Options{QueueWait: 50 * time.Millisecond}, affinity.Options{}, b1, b2)

	if code, _ := servedBy(h, t, "sess-failover", reqBody); code != 200 {
		t.Fatalf("failover should land 200, got %d", code)
	}
	// SWRR 首落的 429 那家绝不能进表——粘性钉的是**接住**它的那家
	if pinned, _, ok := tbl.Get("sess-failover"); !ok || pinned != "second-200" {
		t.Fatalf("table must point at the final-200 backend, got (%q,%v)", pinned, ok)
	}
	if got := tbl.Stats().Evictions; got != 0 {
		t.Fatalf("no pin existed to evict; evictions=%d", got)
	}
}

// ── ⑥ 失败策略接线：判定走 errclass.ReleaseAffinity（同源，不发明第二层）┐
func TestAffinityFailurePolicyWiring(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })

	// toggleUp：第 n97a 发按 want 造响应
	t.Run("429 全败 → 拆钉（配额，不是本轮网络）", func(t *testing.T) {
		up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
			if n == 1 {
				fmt.Fprint(w, okJSON)
				return
			}
			http.Error(w, "rate limited", http.StatusTooManyRequests)
		})
		b1 := mkBackend("drop-429-a", up.srv.URL, 2)
		b2 := mkBackend("drop-429-b", up.srv.URL, 2)
		h, tbl := mkAffHandler(t, Options{QueueWait: 50 * time.Millisecond}, affinity.Options{}, b1, b2)

		if code, _ := servedBy(h, t, "sess-429", reqBody); code != 200 {
			t.Fatalf("pin-establishing request code=%d", code)
		}
		if pinned, _, ok := tbl.Get("sess-429"); !ok || pinned == "" {
			t.Fatalf("pin not established: (%q,%v)", pinned, ok)
		}
		// 同会话再发一发：钉住的那家 429 → ReleaseAffinity(429)=true → 拆钉；
		// failover 到另一家也 429 → 全败。覆盘：表里无钉、逐出恰一次。
		if code, _ := servedBy(h, t, "sess-429", reqBody); code != http.StatusTooManyRequests && code != http.StatusBadGateway {
			t.Fatalf("all-429 failover response, got %d", code)
		}
		if _, _, ok := tbl.Get("sess-429"); ok {
			t.Fatal("429 must relinquish the sticky")
		}
		if got := tbl.Stats().Evictions; got != 1 {
			t.Fatalf("evictions=%d want 1（拆钉只发生一次，重复 Forget 不重复计数）", got)
		}
	})

	t.Run("400 全败 → 保持（客户端的锅，别迁怒 backend）", func(t *testing.T) {
		up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
			if n == 1 {
				fmt.Fprint(w, okJSON)
				return
			}
			http.Error(w, "bad body", http.StatusBadRequest)
		})
		b1 := mkBackend("keep-400-a", up.srv.URL, 2)
		b2 := mkBackend("keep-400-b", up.srv.URL, 2)
		h, tbl := mkAffHandler(t, Options{QueueWait: 50 * time.Millisecond}, affinity.Options{}, b1, b2)

		if code, _ := servedBy(h, t, "sess-400", reqBody); code != 200 {
			t.Fatalf("pin-establishing request code=%d", code)
		}
		pinned, _, _ := tbl.Get("sess-400")
		if code, _ := servedBy(h, t, "sess-400", reqBody); code != http.StatusBadRequest {
			t.Fatalf("all-400 response, got %d", code)
		}
		if backend, _, ok := tbl.Get("sess-400"); !ok || backend != pinned {
			t.Fatalf("400 must KEEP the sticky, got (%q,%v)", backend, ok)
		}
		if got := tbl.Stats().Evictions; got != 0 {
			t.Fatalf("evictions=%d want 0", got)
		}
	})

	t.Run("客户端取消 → 保持 + 占位归还", func(t *testing.T) {
		up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
		b1 := mkBackend("keep-cancel", up.srv.URL, 2)
		h, tbl := mkAffHandler(t, Options{QueueWait: 50 * time.Millisecond}, affinity.Options{}, b1)

		if code, _ := servedBy(h, t, "sess-cancel", reqBody); code != 200 {
			t.Fatalf("pin-establishing request code=%d", code)
		}
		pinned, _, _ := tbl.Get("sess-cancel")
		// 取消发生在进 handler 之前：亲和选序点照常运行（表命中并占位），
		// 但取消分支必须在 return 前把占下的那一位放回去。
		r := postReq(t, "still-json-body")
		r.Header.Set("X-Session-Key", "sess-cancel")
		ctx, cancel := context.WithCancel(r.Context())
		cancel()
		r = r.WithContext(ctx)
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != 499 {
			t.Fatalf("cancelled request code=%d body=%s", w.Code, w.Body.String())
		}
		if inFlight := b1.InFlight(); inFlight != 0 {
			t.Fatalf("affinity probe slot leaked on cancel: in_flight=%d", inFlight)
		}
		if backend, _, ok := tbl.Get("sess-cancel"); !ok || backend != pinned {
			t.Fatalf("cancel must KEEP the sticky, got (%q,%v)", backend, ok)
		}
		if got := tbl.Stats().Evictions; got != 0 {
			t.Fatalf("evictions=%d want 0", got)
		}
		if cnt := up.count(); cnt != 1 {
			t.Fatalf("cancelled request must not reach upstream, calls=%d", cnt)
		}
	})

	t.Run("mid-stream（已吐字后断流）→ 保持", func(t *testing.T) {
		up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
			if n == 1 {
				fmt.Fprint(w, okJSON) // 建立粘性用（非流式）
				return
			}
			// 第二发走流式：吐一个 chunk 后直接断流（不发 [DONE]）
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"半途\"}}]}\n\n")
			w.(http.Flusher).Flush()
		})
		b1 := mkBackend("keep-mid", up.srv.URL, 2)
		h, tbl := mkAffHandler(t, Options{QueueWait: 50 * time.Millisecond}, affinity.Options{}, b1)

		if code, _ := servedBy(h, t, "sess-mid", reqBody); code != 200 {
			t.Fatalf("pin-establishing request code=%d", code)
		}
		pinned, _, _ := tbl.Get("sess-mid")
		// 流式 mid-stream：响应已开始（res.wrote=true）→ 亲和完全不受影响，
		// 绝不拆钉、绝不再切后端（upstream 第二次调用就是最后一发）。
		r := postReq(t, streamBody)
		r.Header.Set("X-Session-Key", "sess-mid")
		w := httptest.NewRecorder()
		h(w, r)
		if cnt := up.count(); cnt != 2 {
			t.Fatalf("mid-stream must stop failover, upstream calls=%d", cnt)
		}
		if backend, _, ok := tbl.Get("sess-mid"); !ok || backend != pinned {
			t.Fatalf("mid-stream must KEEP the sticky, got (%q,%v)", backend, ok)
		}
		if got := tbl.Stats().Evictions; got != 0 {
			t.Fatalf("evictions=%d want 0", got)
		}
	})
}

// ── ⑦ 重试请求（X-Router-Rid）→ 不复钉；成功后按实际接应方迁移 ──────
func TestAffinityRetryRequestNotRePinned(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	b1 := mkBackend("retry-a", up.srv.URL, 2)
	b2 := mkBackend("retry-b", up.srv.URL, 2)

	h, tbl := mkAffHandler(t, Options{QueueWait: 50 * time.Millisecond}, affinity.Options{}, b1, b2)

	if code, by := servedBy(h, t, "sess-retry", reqBody); code != 200 || by == "" {
		t.Fatalf("first request code=%d by=%q", code, by)
	}
	if _, _, ok := tbl.Get("sess-retry"); !ok {
		t.Fatal("pin not established")
	}
	hits0 := tbl.Stats().Hits
	// 重试请求：亲和不复钉（重试语义就是换后端），命中别的后端成功后，
	// 粘性按**实际接应方**迁移过去。
	r := postReq(t, "still-json-body")
	r.Header.Set("X-Session-Key", "sess-retry")
	r.Header.Set("X-Router-Rid", "rid-1")
	r.Header.Set("X-Router-Failed-Backend", "retry-a")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != 200 {
		t.Fatalf("retry request code=%d", w.Code)
	}
	if got := w.Header().Get("X-Served-By"); got == "retry-a" {
		t.Fatalf("retry semantics = rotate away from failed backend, served by %q", got)
	}
	// 不复钉也不假命中：isRetry 那一发不消耗 sticky_hits。
	// ⚠️ 必须在读表断言之前比对——Get 本身就是一次「命中」，会污染 hits 计数。
	if got := tbl.Stats().Hits; got != hits0 {
		t.Fatalf("retry request must not count a sticky hit: %d→%d", hits0, got)
	}
	if backend, _, ok := tbl.Get("sess-retry"); !ok || backend == "retry-a" {
		t.Fatalf("table should be migrated to the actual server, got (%q,%v)", backend, ok)
	}
}

// ── ⑧ 目标后端 Death（名字从路由表消失）：拆钉，跟着新拓扑走 ──────
// 热重载换掉 AtomicRouter 后旧 backend 名不复存在 → pb==nil 分支 Forget。
func TestAffinityVanishedBackendGetsForgotten(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	b1 := mkBackend("dying-be", up.srv.URL, 2)
	b2 := mkBackend("fresh-be", up.srv.URL, 2)

	h0, tbl := mkAffHandler(t, Options{QueueWait: 50 * time.Millisecond}, affinity.Options{}, b1, b2)
	if code, _ := servedBy(h0, t, "sess-dead", reqBody); code != 200 {
		t.Fatalf("pin-establishing request code=%d", code)
	}
	pinned, _, ok := tbl.Get("sess-dead")
	if !ok || pinned == "" {
		t.Fatalf("pin not established: (%q,%v)", pinned, ok)
	}
	// 新拓扑：dying-be 整个消失（热重载换装），同一张亲和表交给新 handler。
	// isRetry=false、表命中、但 rt.ByName(pinned)==nil → 拆钉；
	// 本轮 SWRR 首落唯一候选 fresh-be，成功后 Put 新地方（拆+写一步闭环）。
	hNew, _, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond, Affinity: tbl}, b2)
	if code, by := servedBy(hNew, t, "sess-dead", reqBody); code != 200 || by != "fresh-be" {
		t.Fatalf("request on new topology code=%d by=%q", code, by)
	}
	if backend, _, ok := tbl.Get("sess-dead"); !ok || backend != "fresh-be" {
		t.Fatalf("dead-name pin must be dropped and re-pinned to the actual server, got (%q,%v)", backend, ok)
	}
}
