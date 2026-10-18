package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shanhai-gateway/internal/core/store"
	"shanhai-gateway/internal/core/upstream"
	"shanhai-gateway/internal/service/routing"
)

// 测试纪律：所有上游都是 httptest 打桩，绝不打真实上游。

// fakeUp 一个打桩上游：记请求数/请求体/请求头，按 respFn 造响应。
type fakeUp struct {
	srv      *httptest.Server
	mu       sync.Mutex
	calls    int
	bodies   []string
	sessHdrs []string
	respFn   func(nth int, r *http.Request, w http.ResponseWriter)
}

func newFakedUp(t *testing.T, respFn func(nth int, r *http.Request, w http.ResponseWriter)) *fakeUp {
	t.Helper()
	f := &fakeUp{respFn: respFn}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls++
		n := f.calls
		f.bodies = append(f.bodies, string(body))
		f.sessHdrs = append(f.sessHdrs, r.Header.Get("x-opencode-session"))
		f.mu.Unlock()
		respFn(n, r, w)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUp) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// mkHandler 拼 Options + Router + Store 出 handler。
func mkHandler(t *testing.T, opt Options, backends ...*upstream.Backend) (http.HandlerFunc, *store.Store, *routing.Router) {
	t.Helper()
	for _, b := range backends {
		b.Enabled = true
		b.Init(map[string]*upstream.Account{})
	}
	rt := routing.New(backends)
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return Handler(rt, st, opt), st, rt
}

func mkBackend(name, url string, conc int) *upstream.Backend {
	b := upstream.New(name, "", url, nil)
	b.Concurrency = conc
	return b
}

func postReq(t *testing.T, body string) *http.Request {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

const okJSON = `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3}}}`

// ── 主流程：非流式 200 + usage 解析 + X-Served-By ──────────────────
func TestProxyNonStreamOK(t *testing.T) {
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, okJSON)
	})
	b := mkBackend("test-be", up.srv.URL, 2)
	h, st, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond, DefaultModel: "default-model"}, b)

	w := httptest.NewRecorder()
	h(w, postReq(t, `{"model":"m","messages":[{"role":"user","content":"hello"}]}`))

	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Served-By"); got != "test-be" {
		t.Fatalf("X-Served-By=%q", got)
	}
	if !strings.Contains(w.Body.String(), `"content":"hi"`) {
		t.Fatalf("body passthrough broken: %s", w.Body.String())
	}
	recs := st.Recent()
	if len(recs) == 0 {
		t.Fatal("no attempt recorded")
	}
	a := recs[0]
	if !a.OK || a.Status != 200 {
		t.Fatalf("attempt not ok: %+v", a)
	}
	if a.PromptTokens != 10 || a.CompletionTokens != 5 || a.TotalTokens != 15 || a.CachedTokens != 3 {
		t.Fatalf("usage parse wrong: %+v", a)
	}
}

// ── 流式：SSE 逐行、[DONE]、首帧前不写响应头 ─────────────────────────
func TestProxyStreamingDone(t *testing.T) {
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		// 上游自己把响应头和首帧写出去 —— 网关侧的"首帧前不写头"体现在
		// attempt 的 beginWrite 只在拿到首个有效 chunk 时才调用。
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"效果\"}}]}\n\n")
		w.(http.Flusher).Flush()
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	b := mkBackend("stream-be", up.srv.URL, 2)
	h, st, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, b)

	w := httptest.NewRecorder()
	h(w, postReq(t, `{"model":"m","stream":true,"messages":[]}`))

	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type=%q", got)
	}
	if got := w.Header().Get("X-Served-By"); got != "stream-be" {
		t.Fatalf("X-Served-By=%q", got)
	}
	out := w.Body.String()
	for _, want := range []string{"data: {\"choices\":[{\"delta\":{\"content\":\"效果\"}}]}", "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in stream out: %s", want, out)
		}
	}
	recs := st.Recent()
	if len(recs) != 1 || !recs[0].OK || recs[0].Stream != true {
		t.Fatalf("attempt: %+v", recs)
	}
	if recs[0].TotalTokens != 5 {
		t.Fatalf("stream usage not parsed: %+v", recs[0])
	}
}

// ── X-Session-Key → session_header 映射 ────────────────────────────
func TestSessionKeyMapped(t *testing.T) {
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
		fmt.Fprint(w, okJSON)
	})
	b := mkBackend("sess-be", up.srv.URL, 2)
	b.SessionHeader = "x-opencode-session"
	h, _, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, b)

	r := postReq(t, `{"model":"m","messages":[]}`)
	r.Header.Set("X-Session-Key", "splicer/test-ai")
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := up.sessHdrs[0]; got != "splicer/test-ai" {
		t.Fatalf("session header upstream=%q", got)
	}
}

// ── 模型改写：客户端 model → EffModel（后端 Model 优先，DefaultModel 兜底）──
func TestModelRewrite(t *testing.T) {
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
		fmt.Fprint(w, okJSON)
	})
	b := mkBackend("model-be", up.srv.URL, 2)
	b.Model = "opencode/gpt-4o"
	h, _, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond, DefaultModel: "default-m"}, b)

	w := httptest.NewRecorder()
	h(w, postReq(t, `{"model":"whatever-playing","messages":[]}`))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(up.bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "opencode/gpt-4o" {
		t.Fatalf("model not rewritten: %v", sent["model"])
	}
}

// ── reasoning_effort 注入：客户端没指定才注入；显式指定则尊重；空配置不注入 ──
func TestReasoningEffortInject(t *testing.T) {
	// ① 客户端没带 → 注入 opt.ReasoningEffort
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	b := mkBackend("re-be", up.srv.URL, 2)
	b.Model = "m" // 生产里每个后端都有 model（wantModel 恒非空），注入块才生效
	h, _, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond, ReasoningEffort: "low"}, b)
	w := httptest.NewRecorder()
	h(w, postReq(t, `{"model":"m","messages":[]}`))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(up.bodies[0]), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort not injected: %v", sent["reasoning_effort"])
	}

	// ② 客户端显式 high → 尊重，不覆盖
	up2 := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	b2 := mkBackend("re-be2", up2.srv.URL, 2)
	b2.Model = "m"
	h2, _, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond, ReasoningEffort: "low"}, b2)
	w2 := httptest.NewRecorder()
	h2(w2, postReq(t, `{"model":"m","reasoning_effort":"high","messages":[]}`))
	if w2.Code != 200 {
		t.Fatalf("status=%d body=%s", w2.Code, w2.Body.String())
	}
	var sent2 map[string]any
	if err := json.Unmarshal([]byte(up2.bodies[0]), &sent2); err != nil {
		t.Fatal(err)
	}
	if sent2["reasoning_effort"] != "high" {
		t.Fatalf("explicit reasoning_effort overridden: %v", sent2["reasoning_effort"])
	}

	// ③ opt.ReasoningEffort 空 → 不注入
	up3 := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	b3 := mkBackend("re-be3", up3.srv.URL, 2)
	b3.Model = "m"
	h3, _, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond, ReasoningEffort: ""}, b3)
	w3 := httptest.NewRecorder()
	h3(w3, postReq(t, `{"model":"m","messages":[]}`))
	if w3.Code != 200 {
		t.Fatalf("status=%d body=%s", w3.Code, w3.Body.String())
	}
	var sent3 map[string]any
	if err := json.Unmarshal([]byte(up3.bodies[0]), &sent3); err != nil {
		t.Fatal(err)
	}
	if _, has := sent3["reasoning_effort"]; has {
		t.Fatalf("empty ReasoningEffort should not inject: %v", sent3["reasoning_effort"])
	}
}

// ── 满载路径三连：分散兜底 / 排队 / 429 skip_capacity ────────────────
func TestSaturatedDistributesToFallback(t *testing.T) {
	logBuf := &bytes.Buffer{}
	log.SetOutput(logBuf)
	t.Cleanup(func() { log.SetOutput(io.Discard) })

	upP := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	upF := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	bp := mkBackend("primary-full", upP.srv.URL, 1)
	bf := mkBackend("fallback-be", upF.srv.URL, 1)
	bf.IsFallback = true
	// 占住首发的唯一并发位
	if !bp.TryAcquire() {
		t.Fatal("pre acquire failed")
	}
	t.Cleanup(bp.Release)
	h, _, _ := mkHandler(t, Options{QueueWait: 80 * time.Millisecond}, bp, bf)

	w := httptest.NewRecorder()
	h(w, postReq(t, `{"model":"m","messages":[]}`))
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Served-By"); got != "fallback-be" {
		t.Fatalf("served by=%q", got)
	}
	if upP.count() != 0 || upF.count() != 1 {
		t.Fatalf("call counts p=%d f=%d", upP.count(), upF.count())
	}
	if !strings.Contains(logBuf.String(), "分散到兜底 fallback-be (原 primary-full 满载)") {
		t.Fatalf("missing distribution log: %s", logBuf.String())
	}
}

func TestAllFullThenQueueSuccess(t *testing.T) {
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	b := mkBackend("queue-be", up.srv.URL, 1)
	if !b.TryAcquire() {
		t.Fatal("pre acquire failed")
	}
	h, _, _ := mkHandler(t, Options{QueueWait: 3 * time.Second}, b)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		h(w, postReq(t, `{"model":"m","messages":[]}`))
		done <- w
	}()
	time.Sleep(200 * time.Millisecond)
	b.Release() // 让出并发位，排队者应被唤醒继续走 attempt
	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not finish after release")
	}
	if w.Code != 200 {
		t.Fatalf("queued request status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAllFullTimesOut429(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	b := mkBackend("sat-be", up.srv.URL, 1)
	if !b.TryAcquire() {
		t.Fatal("pre acquire failed")
	}
	t.Cleanup(b.Release)
	h, st, _ := mkHandler(t, Options{QueueWait: 120 * time.Millisecond}, b)

	w := httptest.NewRecorder()
	h(w, postReq(t, `{"model":"m","messages":[]}`))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra != "1" { // 0.12s → 0 → 强制 1
		t.Fatalf("Retry-After=%q", ra)
	}
	if !strings.Contains(w.Body.String(), "router saturated") {
		t.Fatalf("body: %s", w.Body.String())
	}
	recs := st.Recent()
	if len(recs) != 1 || recs[0].Class != "skip_capacity" {
		t.Fatalf("attempt: %+v", recs)
	}
}

// ── 429 → 立刻冷却 + break keyLoop（两个 key 也只打一枪）────────────
func Test429ImmediateCooldownBreaksKeyLoop(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	})
	b := mkBackend("rate-be", up.srv.URL, 2)
	b.APIKeys = []string{"k1", "k2"}
	h, _, rt0 := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, b)

	w := httptest.NewRecorder()
	h(w, postReq(t, `{"model":"m","messages":[]}`))
	if up.count() != 1 {
		t.Fatalf("key loop should break on 429, calls=%d", up.count())
	}
	_ = rt0
	// 立刻冷却：后续请求应被 skip_cooldown 跳过（不再打上游）
	if reason := b.SkipReason(time.Now()); reason != "skip_cooldown" {
		t.Fatalf("expected skip_cooldown, got %q", reason)
	}

	// 再发一个请求验证不再打上游（上游 count 仍为 1）
	wio := httptest.NewRecorder()
	h(wio, postReq(t, `{"model":"m","messages":[]}`))
	if up.count() != 1 {
		t.Fatalf("cooldown not effective after first 429, calls=%d", up.count())
	}
}

// ── 400 → break keyLoop（换 key 没用）───────────────────────────────
func Test400BreaksKeyLoop(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
		http.Error(w, "bad body", http.StatusBadRequest)
	})
	b := mkBackend("bad-req-be", up.srv.URL, 2)
	b.APIKeys = []string{"k1", "k2"}
	h, _, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, b)

	w := httptest.NewRecorder()
	h(w, postReq(t, `{"model":"m","messages":[]}`))
	if up.count() != 1 {
		t.Fatalf("key loop should break on 400, calls=%d", up.count())
	}
	// 400 = 客户端的锅：后端不应冷却，但应切到下一个后端（此处只有一个）：
	if reason := b.SkipReason(time.Now()); reason != "" {
		t.Fatalf("backend should NOT be cooled on 400, got %q", reason)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "bad_request_400") {
		t.Fatalf("failures detail missing: %s", w.Body.String())
	}
}

// ── 客户端取消 → 499，且后端不被冷却（10-03 回归用例）───────────────
func TestClientCancel499NoCooldown(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	b := mkBackend("cancel-be", up.srv.URL, 2)
	h, _, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, b)

	r := postReq(t, `{"model":"m","messages":[]}`)
	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	cancel() // 先取消，再进 handler —— select 在每轮循环必命中

	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != 499 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "client cancelled request") {
		t.Fatalf("body: %s", w.Body.String())
	}
	if reason := b.SkipReason(time.Now()); reason != "" {
		t.Fatalf("backend should NOT be cooled on client cancel (10-03 regression), got %q", reason)
	}
	if up.count() != 0 {
		t.Fatalf("cancelled request must not reach upstream, calls=%d", up.count())
	}
}

// ── mid-stream：响应已开始后不再切后端 ─────────────────────────────
func TestMidStreamStopsFailover(t *testing.T) {
	logBuf := &bytes.Buffer{}
	log.SetOutput(logBuf)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"部分\"}}]}\n\n")
		w.(http.Flusher).Flush()
		// 不发 [DONE]，直接断流
	})
	b1 := mkBackend("mid-1", up.srv.URL, 2)
	b2 := mkBackend("mid-2", up.srv.URL, 2) // 两个候选：若 failover 未被抑制会切到 2
	logBuf.Reset()

	h, st, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, b1, b2)
	recsBefore := 0

	streamReq := postReq(t, `{"model":"m","stream":true,"messages":[]}`)
	w := httptest.NewRecorder()
	h(w, streamReq)
	_ = recsBefore

	out := w.Body.String()
	if !strings.Contains(out, "data: {\"choices\":[{\"delta\":{\"content\":\"部分\"}}]}") {
		t.Fatalf("first chunk missing: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("endStream should append [DONE]: %s", out)
	}
	if up.count() != 1 {
		t.Fatalf("must stop failover after response started, calls=%d", up.count())
	}
	if !strings.Contains(logBuf.String(), "response already started; stop failover") {
		t.Fatalf("mid-stream log missing: %s", logBuf.String())
	}
	recs := st.Recent()
	if len(recs) == 0 || recs[0].Class != "mid_stream_fail" {
		t.Fatalf("attempt class: %+v", recs)
	}
}

// ── 200 + error body → err_body，后端不算故障但切后端 ───────────────
func Test200WithErrorBody(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) {
		fmt.Fprint(w, `{"error":{"message":"upstream said no"}}`)
	})
	b := mkBackend("errbody-be", up.srv.URL, 2)
	h, st, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, b)

	w := httptest.NewRecorder()
	h(w, postReq(t, `{"model":"m","messages":[]}`))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"class":"error_body"`) {
		t.Fatalf("failures: %s", w.Body.String())
	}
	recs := st.Recent()
	if len(recs) == 0 || recs[0].Class != "error_body" {
		t.Fatalf("attempt: %+v", recs)
	}
}

// ── 全冷却 → 429 + Retry-After: 60 ─────────────────────────────────
func TestAllCooldown429(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(io.Discard) })
	up := newFakedUp(t, func(n int, r *http.Request, w http.ResponseWriter) { fmt.Fprint(w, okJSON) })
	b := mkBackend("cool-be", up.srv.URL, 2)
	b.SetCooldown(60*time.Second, "skip_cooldown_preset")
	h, _, _ := mkHandler(t, Options{QueueWait: 50 * time.Millisecond}, b)

	w := httptest.NewRecorder()
	h(w, postReq(t, `{"model":"m","messages":[]}`))
	if up.count() != 0 {
		t.Fatalf("cooled backend must not be hit, calls=%d", up.count())
	}
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if ra := w.Header().Get("Retry-After"); ra != "60" {
		t.Fatalf("Retry-After=%q", ra)
	}
	if !strings.Contains(w.Body.String(), "all upstreams cooling down (rate limited)") {
		t.Fatalf("body: %s", w.Body.String())
	}
}
