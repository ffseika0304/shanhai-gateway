// dashauth_test.go —— 公网门两条过门路子（session cookie / 静态 key）的回归锁。
//
// 为什么值得写：门一旦漏，公网口上 /v1 的配额和 /admin 的并发就等于白送。
// 这几个用例把「错 key 必须 401」「DASH_TOKEN 配空 = 通道关闭」「换了 key
// 旧的必须失效」「cookie 老路子不许被新 key 挤掉」四件事钉死。
// slr 此前没有 Go 测试，这是第一个（2026-09-28）。

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type gateResp struct {
	code int
	body string
	loc  string
}

// newGate 起一个装了哑 handler 的门，返回能直接打的服务器
func newGate(t *testing.T) *httptest.Server {
	t.Helper()
	h := dashGate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

// do 打一发；不跟随重定向（否则 302 会被自动跳成 /login 的 200，测不出 302）
func do(t *testing.T, method, url, bearer, cookie string, html bool) gateResp {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		req.Header.Add("Cookie", dashCookieName+"="+cookie)
	}
	if html {
		req.Header.Set("Accept", "text/html")
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return gateResp{code: resp.StatusCode, body: string(b), loc: resp.Header.Get("Location")}
}

// 静态 key 主路子：带对了直接过，不碰 cookie
func TestDashGateStaticKey(t *testing.T) {
	t.Setenv("DASH_TOKEN", dashTokenDefault)
	s := newGate(t)

	if r := do(t, "GET", s.URL+"/api/stats", dashTokenDefault, "", false); r.code != 200 || !strings.Contains(r.body, `"ok":true`) {
		t.Fatalf("对的 key 应放行，got %d %s", r.code, r.body)
	}
	if r := do(t, "POST", s.URL+"/v1/chat/completions", dashTokenDefault, "", false); r.code != 200 {
		t.Fatalf("POST 也应放行，got %d", r.code)
	}
	// 页面导航带 key 同样放行（key 是完整凭据，不只是 /v1 的门票）
	if r := do(t, "GET", s.URL+"/", dashTokenDefault, "", true); r.code != 200 {
		t.Fatalf("带 key 的页面导航应放行，got %d", r.code)
	}
}

// 错 key / 无凭据一律 401；页面导航无凭据仍 302 去登录页（老行为不许变）
func TestDashGateRejects(t *testing.T) {
	t.Setenv("DASH_TOKEN", dashTokenDefault)
	s := newGate(t)

	if r := do(t, "GET", s.URL+"/api/stats", "slr_00000000000000000000000000000000", "", false); r.code != 401 || !strings.Contains(r.body, "unauthorized") {
		t.Fatalf("错 key 应 401 JSON，got %d %s", r.code, r.body)
	}
	if r := do(t, "GET", s.URL+"/api/stats", "", "", false); r.code != 401 {
		t.Fatalf("裸请求应 401，got %d", r.code)
	}
	if r := do(t, "GET", s.URL+"/", "", "", true); r.code != 302 || !strings.Contains(r.loc, "/login") {
		t.Fatalf("裸页面导航应 302 /login，got %d loc=%s", r.code, r.loc)
	}
	// 非 Bearer 方案（Basic）不算数——只认 Bearer，免得被别的 scheme 骗过门
	req, _ := http.NewRequest("GET", s.URL+"/api/stats", nil)
	req.Header.Set("Authorization", "Basic "+dashTokenDefault)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("Basic 方案应 401，got %d", resp.StatusCode)
	}
}

// DASH_TOKEN 配成空串 = 整条通道关闭（systemd 里 `Environment=DASH_TOKEN=`）
func TestDashTokenDisabled(t *testing.T) {
	t.Setenv("DASH_TOKEN", "")
	s := newGate(t)

	if got := dashToken(); got != "" {
		t.Fatalf("显式置空应得到空 key，got %q", got)
	}
	if r := do(t, "GET", s.URL+"/api/stats", dashTokenDefault, "", false); r.code != 401 {
		t.Fatalf("通道关闭后默认 key 也不该过门，got %d", r.code)
	}
	// 门关了，cookie 老路子必须照常工作
	sid := dashNewSession()
	if r := do(t, "GET", s.URL+"/api/stats", "", sid, false); r.code != 200 {
		t.Fatalf("key 通道关闭不影响 cookie，got %d", r.code)
	}
	// 自取口要如实回报 enabled=false，前端才好显示「未启用」。
	// 这里直打 handler：测试用的门里挂的是哑 handler，没有注册真路由。
	rr := httptest.NewRecorder()
	dashKeyHandler(rr, httptest.NewRequest("GET", "/api/dashkey", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"enabled":false`) {
		t.Fatalf("dashkey 应回报关闭态，got %d %s", rr.Code, rr.Body.String())
	}
}

// 换 key 后旧 key 必须立刻失效，且 cookie 老路子全程不受影响
func TestDashTokenRotationAndCookieLane(t *testing.T) {
	t.Setenv("DASH_TOKEN", "slr_rotated_value")
	s := newGate(t)

	if r := do(t, "GET", s.URL+"/api/stats", "slr_rotated_value", "", false); r.code != 200 {
		t.Fatalf("新 key 应放行，got %d", r.code)
	}
	if r := do(t, "GET", s.URL+"/api/stats", dashTokenDefault, "", false); r.code != 401 {
		t.Fatalf("换 key 后默认 key 应失效，got %d", r.code)
	}
	sid := dashNewSession()
	if r := do(t, "GET", s.URL+"/api/stats", "", sid, false); r.code != 200 {
		t.Fatalf("cookie 通道应全程可用，got %d", r.code)
	}
	if r := do(t, "GET", s.URL+"/api/stats", "slr_rotated_value", sid, false); r.code != 200 {
		t.Fatalf("key + cookie 同时给也该过，got %d", r.code)
	}
}

// Bearer 解析：大小写、空格、非 Bearer 方案
func TestDashBearer(t *testing.T) {
	cases := []struct{ hdr, want string }{
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"},
		{"BEARER   abc  ", "abc"},
		{"Basic abc", ""},
		{"abc", ""},
		{"", ""},
		{"Bearer", ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		if c.hdr != "" {
			r.Header.Set("Authorization", c.hdr)
		}
		if got := dashBearer(r); got != c.want {
			t.Errorf("dashBearer(%q) = %q, want %q", c.hdr, got, c.want)
		}
	}
}
