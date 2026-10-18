package config

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"shanhai-gateway/internal/core/errclass"
	"shanhai-gateway/internal/core/upstream"
)

// 接线④：probe 的 403 分支改调 errclass.NoCreditBody（词表从本包内联挪成全局默认词汇）。
//
// 语义逐位不变，这条测试把「不变」钉死：
//   - 403 + 欠费/额度文案 → 永久失效，Reason 前缀 "403 no-credit: "；
//   - 403 + 无关文案      → 仍按 403 永久失效处理（照旧），只是 Reason 前缀
//     "403 forbidden: " —— 403 是否拉黑与文案无关，文案只决定日志/黑名单里的理由。
//     本批只搬词表归属，不改这条判定；要改判定得单独开一批并重新评审拉黑语义。
func TestProbeOnce403SplitByCreditVocabulary(t *testing.T) {
	var body atomic.Value
	body.Store(`{"error":"pending"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, body.Load().(string))
	}))
	t.Cleanup(srv.Close)
	b := upstream.New("probe-403-be", "m", srv.URL, nil)

	// 欠费文案 → 永久失效 + no-credit 理由（语义同改动前）
	body.Store("Your account is overdue, balance insufficient. 请充值：账户已欠费、额度已用完")
	res := probeOnce(context.Background(), b, "k")
	if !res.Permanent {
		t.Fatalf("403 欠费必须判永久失效: %+v", res)
	}
	if !strings.HasPrefix(res.Reason, "403 no-credit: ") {
		t.Fatalf("Reason = %q, 期望以 403 no-credit: 开头", res.Reason)
	}
	if !errclass.NoCreditBody("账户已欠费") {
		t.Fatal("词表应认领中文欠费文案")
	}

	// 无关文案 → 不被 NoCreditBody 认领，走 403 通用理由；403 的永久语义照旧
	body.Store(`{"error":"access denied by edge policy"}`)
	res2 := probeOnce(context.Background(), b, "k")
	if errclass.NoCreditBody("access denied by edge policy") {
		t.Fatal("词表不许认领无关文案")
	}
	if !strings.HasPrefix(res2.Reason, "403 forbidden: ") {
		t.Fatalf("Reason = %q, 期望以 403 forbidden: 开头", res2.Reason)
	}
	if !res2.Permanent {
		t.Fatalf("403 一律判永久失效是既有语义（本批不改）: %+v", res2)
	}
}
