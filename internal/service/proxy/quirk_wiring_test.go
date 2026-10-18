package proxy

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shanhaigateway/internal/core/errclass"
	"shanhaigateway/internal/core/quirk"
	"shanhaigateway/internal/core/upstream"
)

// ── 怪癖层接线（缝①Decorate / 缝②Classify / 缝③ParseUsage）全链测试 ──
//
// 测试纪律照旧：全部 httptest 打桩，绝不打真实上游。
// 假怪癖在 init 里注册（注册表是全局的），但只有 driver 名对上的后端才会拿到它们 ——
// 其它测试的后端都没配 driver，拿到的仍是 OpenAI 默认实现，行为逐位不变。
// driver 名一律带 test- 前缀，防止与将来真上生产的怪癖撞名（Register 会 panic）。

// 假怪癖①：只认领一家特有的那一句（通用词表不认识它）。
type claimQuirk struct{ quirk.OpenAI }

func (claimQuirk) Name() string { return "test-claim-driver" }

func (claimQuirk) Classify(status int, body string) (errclass.Class, bool) {
	if strings.Contains(body, "UPSTREAM-MODEL-POOL-EMPTY") {
		return errclass.ModelUnavailable, true
	}
	return "", false
}

// 假怪癖②：永不认领 —— (Class,false) 契约的调用方侧回归。
type noClaimQuirk struct{ quirk.OpenAI }

func (noClaimQuirk) Name() string { return "test-no-claim-driver" }

func (noClaimQuirk) Classify(status int, body string) (errclass.Class, bool) {
	return "", false
}

// 假怪癖③：出口加一个该家专属头。
type decorateQuirk struct{ quirk.OpenAI }

func (decorateQuirk) Name() string { return "test-decorate-driver" }

func (decorateQuirk) Decorate(req *http.Request, b *upstream.Backend) {
	req.Header.Set("X-Quirk-Test", "on")
}

// 假怪癖④：该家的 usage 不是 OpenAI 形状，自己解析。
type usageQuirk struct{ quirk.OpenAI }

func (usageQuirk) Name() string { return "test-usage-driver" }

func (usageQuirk) ParseUsage(body []byte) quirk.Usage {
	if !strings.Contains(string(body), `"usage"`) {
		return quirk.Usage{}
	}
	return quirk.Usage{Prompt: 700, Completion: 800, Total: 1500, CachedPrompt: 99, OK: true}
}

func init() {
	quirk.Register(claimQuirk{})
	quirk.Register(noClaimQuirk{})
	quirk.Register(decorateQuirk{})
	quirk.Register(usageQuirk{})
}

func quietLog(t *testing.T) {
	t.Helper()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
}

// ── 接线②：怪癖认领的 body 被改判；不配 driver 的后端走通用（双 backend）──

func TestQuirkClassifyClaimReclassifiesInFullChain(t *testing.T) {
	quietLog(t)
	// 这句只有假怪癖认领：通用词表不认识它 —— 不接怪癖层时它就是 bad_request_400
	const body = `{"error":"UPSTREAM-MODEL-POOL-EMPTY"}`
	mk400 := func(n int, r *http.Request, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, body)
	}
	upClaim := newFakedUp(t, mk400)
	upPlain := newFakedUp(t, mk400)

	bClaim := mkBackend("quirk-claim-be", upClaim.srv.URL, 2)
	bClaim.Driver = "test-claim-driver"
	bClaim.Model = "m1"
	bPlain := mkBackend("quirk-plain-be", upPlain.srv.URL, 2)
	bPlain.Model = "m1"

	hc, stc, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, bClaim)
	hp, stp, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, bPlain)

	wc := httptest.NewRecorder()
	hc(wc, postReq(t, `{"model":"m","messages":[]}`))
	recs := stc.Recent()
	if len(recs) == 0 {
		t.Fatal("quirk 后端没有 attempt 记录")
	}
	if recs[0].Class != string(errclass.ModelUnavailable) {
		t.Fatalf("怪癖认领后应改判 model_unavailable, got %q", recs[0].Class)
	}

	wp := httptest.NewRecorder()
	hp(wp, postReq(t, `{"model":"m","messages":[]}`))
	recsP := stp.Recent()
	if len(recsP) == 0 {
		t.Fatal("默认后端没有 attempt 记录")
	}
	if recsP[0].Class != string(errclass.BadRequest400) {
		t.Fatalf("没配 driver 的后端必须走通用判定（保持 bad_request_400）, got %q", recsP[0].Class)
	}
}

// ── 接线②的另一半：(Class,false) → 调用方回退通用（真实接缝上验）────

func TestQuirkClassifyFalseFallsBackToGenericInFullChain(t *testing.T) {
	quietLog(t)

	// 怪癖不认领、通用词表认领 → 仍要改判成 model_unavailable（通用分支接管）
	upGone := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"model not found"}`)
	})
	bGone := mkBackend("noclaim-gone-be", upGone.srv.URL, 2)
	bGone.Driver = "test-no-claim-driver"
	bGone.Model = "m1"
	h1, st1, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, bGone)
	w1 := httptest.NewRecorder()
	h1(w1, postReq(t, `{"model":"m","messages":[]}`))
	if recs := st1.Recent(); len(recs) == 0 || recs[0].Class != string(errclass.ModelUnavailable) {
		t.Fatalf("怪癖不认领必须回退通用词表改判, recs=%+v", recs)
	}

	// 怪癖不认领、通用也不认领 → 分类一个字节都不动（429 也不许被改判）
	upOther := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "UPSTREAM-MODEL-POOL-EMPTY") // 撞了怪癖文案，但 429 不进缝② 的前置条件
	})
	bOther := mkBackend("quirk-429-be", upOther.srv.URL, 2)
	bOther.Driver = "test-claim-driver" // 认领型怪癖也一样：429/500 不许被改判
	bOther.Model = "m1"
	h2, st2, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, bOther)
	w2 := httptest.NewRecorder()
	h2(w2, postReq(t, `{"model":"m","messages":[]}`))
	if recs := st2.Recent(); len(recs) == 0 || recs[0].Class != string(errclass.RateLimit429) {
		t.Fatalf("429 不许被缝② 改判, recs=%+v", recs)
	}
}

// ── 接线①：怪癖 Decorate 加的头只出现在它那家的上游；默认路径收不到 ──

func TestQuirkDecorateHeaderReachesUpstreamOnlyForItsDriver(t *testing.T) {
	quietLog(t)
	got := make(chan string, 2)
	mk200 := func(n int, r *http.Request, w http.ResponseWriter) {
		got <- r.Header.Get("X-Quirk-Test")
		fmt.Fprint(w, okJSON)
	}
	upDec := newFakedUp(t, mk200)
	upPlain := newFakedUp(t, mk200)

	bDec := mkBackend("decor-be", upDec.srv.URL, 2)
	bDec.Driver = "test-decorate-driver"
	bPlain := mkBackend("decor-plain-be", upPlain.srv.URL, 2) // 未填 driver = 默认

	hDec, _, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, bDec)
	hPlain, _, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, bPlain)

	wDec := httptest.NewRecorder()
	hDec(wDec, postReq(t, `{"model":"m","messages":[]}`))
	if wDec.Code != http.StatusOK {
		t.Fatalf("quirk 后端状态码=%d", wDec.Code)
	}
	wPlain := httptest.NewRecorder()
	hPlain(wPlain, postReq(t, `{"model":"m","messages":[]}`))
	if wPlain.Code != http.StatusOK {
		t.Fatalf("默认后端状态码=%d", wPlain.Code)
	}

	for i, want := range []string{"on", ""} {
		select {
		case gotHdr := <-got:
			if gotHdr != want {
				t.Fatalf("第 %d 个上游收到 X-Quirk-Test=%q, 期望 %q", i, gotHdr, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("上游没收到请求")
		}
	}
}

// ── 接线③：怪癖解析的用量落库；未配 driver 的后端走 OpenAI 默认 ────

func TestQuirkParseUsageWiringFullChain(t *testing.T) {
	quietLog(t)
	mk200 := func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) }

	upQ := newFakedUp(t, mk200)
	bQ := mkBackend("usage-quirk-be", upQ.srv.URL, 2)
	bQ.Driver = "test-usage-driver"
	hQ, stQ, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, bQ)
	wQ := httptest.NewRecorder()
	hQ(wQ, postReq(t, `{"model":"m","messages":[]}`))
	if wQ.Code != http.StatusOK {
		t.Fatalf("quirk 后端状态码=%d", wQ.Code)
	}
	recs := stQ.Recent()
	if len(recs) == 0 {
		t.Fatal("没有 attempt 记录")
	}
	if a := recs[0]; a.PromptTokens != 700 || a.CompletionTokens != 800 ||
		a.TotalTokens != 1500 || a.CachedTokens != 99 {
		t.Fatalf("缝③没走怪癖解析: %+v", a)
	}

	upD := newFakedUp(t, mk200)
	bD := mkBackend("usage-plain-be", upD.srv.URL, 2)
	hD, stD, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, bD)
	wD := httptest.NewRecorder()
	hD(wD, postReq(t, `{"model":"m","messages":[]}`))
	if wD.Code != http.StatusOK {
		t.Fatalf("默认后端状态码=%d", wD.Code)
	}
	recsD := stD.Recent()
	if len(recsD) == 0 {
		t.Fatal("没有 attempt 记录")
	}
	// 未填 driver = OpenAI 默认 = 原 parseUsage 的口径（okJSON: 10/5/15/3）
	if a := recsD[0]; a.PromptTokens != 10 || a.CompletionTokens != 5 ||
		a.TotalTokens != 15 || a.CachedTokens != 3 {
		t.Fatalf("默认路径的用量必须与不接怪癖层逐位一致: %+v", a)
	}
}
