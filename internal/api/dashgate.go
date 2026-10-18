// dashgate.go —— 公网看板登录门（搬自 seika_llm_router/go/dashauth.go，2026-09-27
// 深夜 用户指令：slr 上公网 + 开页密钥认证）
//
// 背景：LAN 口 :18006 维持"无鉴权"定调不变（文档 §九，SCF 等客户端零改动）；
//
//	新增**仅绑回环**的公网口（LISTEN_PUBLIC，经 frp 送阿里云 :18012），
//	该口**全路径**（含 /v1 LLM 接口与 /admin 调参）都必须先过登录门，
//	杜绝公网扫号机不登录就白嫖配额 / 改并发。
//
// 套路照抄同门公网版：密码 → session 表 → 中间件。
// 凭据默认 admin / admin —— 内置示例值，生产部署必须用 DASH_USER / DASH_PASS 环境变量覆盖。
//
// 另有第二条**静态 API key** 通道（DASH_TOKEN，2026-09-28 用户指令）：
// 其他设备的脚本 / OpenAI SDK 只带 `Authorization: Bearer <key>` 就能过门，
// 免登录换 cookie，也不受"网关一重启 session 全失效"影响。key 从看板
// 文档页 /api/dashkey 自取，改 DASH_TOKEN 环境变量即可换。
//
// 实现要点：
//   - session 存内存（map[cookie]过期时刻），值 = 随机 32 字节 hex，
//     滑动续期 7 天 + 懒清扫；进程重启即全失效（重新登录即可，符合预期）。
//   - 未登录分流：浏览器页面导航（GET + Accept:text/html）→ 302 /login；
//     fetch 类接口 → 401 JSON（防止前端把登录页当 JSON 吃崩）。
//   - 暴力穷举护栏：失败按 IP 计数，10 次/10 分钟锁 5 分钟，
//     每次失败先延迟 300ms（公网裸端口 + 短密码必须有的基本盘）。
//
// 搬入说明（2026-10-11 接线批）：逻辑**原样**，唯一改动是凭据来源 ——
// 原文用 main 包的 envOrDefault，本包统一改用 runtime.EnvString（语义逐位
// 一致：空串视为未设置）。变量/常量/注释一字未动。

package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"shanhaigateway/internal/runtime"
)

const (
	dashCookieName   = "gw_sid"
	dashSessionTTL   = 7 * 24 * time.Hour // 滑动续期
	dashFailWindow   = 10 * time.Minute   // 失败计数窗
	dashFailLimit    = 10                 // 窗内失败上限 → 锁
	dashLockDuration = 5 * time.Minute    // 锁定时长
	dashFailDelay    = 300 * time.Millisecond
)

var (
	dashSessMu sync.Mutex
	dashSess   = map[string]time.Time{} // cookie 值 → 过期时刻

	dashFailMu sync.Mutex
	dashFail   = map[string]*dashFailState{} // 客户端 IP →失败状态
)

type dashFailState struct {
	fails  int
	window time.Time // 计数窗起点
	locked time.Time // 锁到时刻（零值=未锁）
}

// ---------------------------------------------------------------------------
// 凭据
// ---------------------------------------------------------------------------

func dashCreds() (user, pass string) {
	return runtime.EnvString("DASH_USER", "admin"), runtime.EnvString("DASH_PASS", "admin")
}

// dashCheck 恒定时间比较用户名与密码（都对才算过）
func dashCheck(user, pass string) bool {
	u, p := dashCreds()
	au := subtle.ConstantTimeCompare([]byte(user), []byte(u))
	ap := subtle.ConstantTimeCompare([]byte(pass), []byte(p))
	return au == 1 && ap == 1
}

// ---------------------------------------------------------------------------
// 静态 API key（2026-09-28 用户指令：加一个 key，写进前端文档方便复制）
// ---------------------------------------------------------------------------
//
// 为什么是 header 不是 ?token=：query string 会进 access 日志 / 中间代理日志，
// key 一泄露等于把门钥匙挂在门口（09-27 拒绝 `?token=` 的理由**不变**）。
// header 不进日志，所以放行 header 这一条。
//
// 为什么不挂进暴力穷举护栏（**故意的，不是漏了**）：key 是 128bit 随机数，
// 暴破没有意义；而公网口经 frpc 转进来，dashClientIP 拿到的永远是 127.0.0.1
//（router 日志里全部登录记录一律记 ip=127.0.0.1 可证）——一旦按失败计数，
// 任何一个客户端手抖 10 次就把**所有**公网客户端连看板一起锁 5 分钟，
// 等于给自己埋自爆雷。宁可不计数、不写日志：失败就回 401，无副作用。

const dashTokenDefault = "slr_2e61800473a99f997cef6d6027d328ce"

// dashToken 取当前生效的 key。未设 DASH_TOKEN → 用内置默认；
// 显式设成空串（systemd 里 `Environment=DASH_TOKEN=`）→ 整条通道关闭。
func dashToken() string {
	if v, ok := os.LookupEnv("DASH_TOKEN"); ok {
		return strings.TrimSpace(v)
	}
	return dashTokenDefault
}

// dashBearer 抠出 Authorization: Bearer 的 key 部分；缺头 / 非 Bearer 方案 → ""
func dashBearer(r *http.Request) string {
	const pfx = "bearer "
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) < len(pfx) || !strings.EqualFold(h[:len(pfx)], pfx) {
		return ""
	}
	return strings.TrimSpace(h[len(pfx):])
}

// dashTokenOK 恒定时间比对静态 key（key 配成空串时恒 false = 通道关闭）
func dashTokenOK(r *http.Request) bool {
	want := dashToken()
	if want == "" {
		return false
	}
	got := dashBearer(r)
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// dashKeyHandler 把当前生效的 key 吐给看板文档页，供就地复制 / 拼 curl 示例。
// 公网口被 dashGate 拦着，**必须先登录**才读得到；
// LAN 口 :18006 按文档 §九 本就免鉴权，能读到 key 的人早就是能随便调 LLM 的人，
// 不新增攻击面。故挂在两个口共用的 mux 上（见 router.go 注册处）。
func dashKeyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	tok := dashToken()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":   tok,
		"enabled": tok != "",
		"env":     "DASH_TOKEN",
	})
}

// ---------------------------------------------------------------------------
// session 表
// ---------------------------------------------------------------------------

// dashNewSession 造随机 session id 并登记，返回 cookie 值
func dashNewSession() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 在 Linux 上不该失败；真失败就用时间戳兜底（保登录可用）
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	id := hex.EncodeToString(b)
	dashSessMu.Lock()
	now := time.Now()
	// 懒清扫：顺手扔掉已过期的（session 量级为个位数，全表扫无压力）
	for k, exp := range dashSess {
		if now.After(exp) {
			delete(dashSess, k)
		}
	}
	dashSess[id] = now.Add(dashSessionTTL)
	dashSessMu.Unlock()
	return id
}

// dashValidSession 校验请求携带的 cookie；有效则顺延续期
func dashValidSession(r *http.Request) bool {
	c, err := r.Cookie(dashCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	dashSessMu.Lock()
	defer dashSessMu.Unlock()
	now := time.Now()
	exp, ok := dashSess[c.Value]
	if !ok || now.After(exp) {
		delete(dashSess, c.Value) // 未登录或已过期（删不存在的键无害）
		return false
	}
	dashSess[c.Value] = now.Add(dashSessionTTL) // 滑动续期
	return true
}

func dashDropSession(id string) {
	dashSessMu.Lock()
	delete(dashSess, id)
	dashSessMu.Unlock()
}

// ---------------------------------------------------------------------------
// 失败限速（按 IP）
// ---------------------------------------------------------------------------

func dashClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// dashLocked 查该 IP 是否处于锁定中（顺手清过期条目，防 map 无限长）
func dashLocked(ip string) bool {
	dashFailMu.Lock()
	defer dashFailMu.Unlock()
	st := dashFail[ip]
	if st == nil {
		return false
	}
	now := time.Now()
	if st.locked.IsZero() {
		if now.Sub(st.window) > dashFailWindow {
			delete(dashFail, ip) // 窗过期且未锁 → 清
		}
		return false
	}
	if now.Before(st.locked) {
		return true
	}
	delete(dashFail, ip) // 锁到期 → 清
	return false
}

// dashRecordFail 记一次失败；返回是否刚触发锁定
func dashRecordFail(ip string) (justLocked bool) {
	dashFailMu.Lock()
	defer dashFailMu.Unlock()
	now := time.Now()
	st := dashFail[ip]
	if st == nil || now.Sub(st.window) > dashFailWindow {
		dashFail[ip] = &dashFailState{fails: 1, window: now}
		return false
	}
	st.fails++
	if st.fails >= dashFailLimit {
		st.locked = now.Add(dashLockDuration)
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 登录页
// ---------------------------------------------------------------------------

const dashLoginPage = `<!doctype html>
<html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Model Gateway · 登录</title>
<style>
  body{background:#0d1117;color:#e6edf3;margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
       font-family:system-ui,-apple-system,"PingFang SC","Microsoft YaHei",sans-serif}
  .box{width:320px;background:#161b22;border:1px solid #30363d;border-radius:12px;padding:28px}
  h1{font-size:18px;margin:0 0 4px}
  .sub{color:#8b949e;font-size:12px;margin:0 0 18px}
  label{display:block;font-size:12px;color:#8b949e;margin:12px 0 4px}
  input{width:100%;box-sizing:border-box;background:#0d1117;color:#e6edf3;border:1px solid #30363d;
        border-radius:8px;padding:9px 10px;font-size:14px;outline:none}
  input:focus{border-color:#58a6ff}
  button{width:100%;margin-top:18px;background:#238636;color:#fff;border:0;border-radius:8px;
         padding:10px;font-size:14px;cursor:pointer}
  button:hover{background:#2ea043}
  .err{color:#f85149;font-size:12px;margin-top:10px;display:none}
  .err.on{display:block}
  footer{position:fixed;bottom:14px;left:0;right:0;text-align:center;color:#484f58;font-size:11px}
</style></head><body>
<div class="box">
  <h1>Model Gateway</h1>
  <p class="sub">公网看板 · 输入账号密码进入</p>
  <form method="post" action="/login">
    <label>用户</label>
    <input name="user" autocomplete="username" required autofocus>
    <label>密码</label>
    <input name="pass" type="password" autocomplete="current-password" required>
    <button type="submit">进入看板</button>
    <div class="err __ERR__">用户或密码不对</div>
  </form>
</div>
<footer>局域网直连 18006 免登录 · 本页仅公网通道</footer>
<script>if(location.search.indexOf("err=1")>=0)document.querySelector(".err").className="err on";</script>
</body></html>`

func dashRenderLogin(w http.ResponseWriter, showErr bool) {
	page := dashLoginPage
	if showErr {
		page = strings.Replace(page, "__ERR__", "err on", 1)
	} else {
		page = strings.Replace(page, "__ERR__", "err", 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(page))
}

// wantsHTML 判断这是不是浏览器页面导航（决定未登录时 302 还是 401）
func wantsHTML(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// ---------------------------------------------------------------------------
// 中间件
// ---------------------------------------------------------------------------

// dashGate 包住整个业务 mux：/login、/logout 自治，其余全要求有效 session。
// 只挂在公网口（LISTEN_PUBLIC）上，LAN 口 :18006 不经过这里。
func dashGate(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		if path == "/login" {
			switch r.Method {
			case http.MethodGet, http.MethodHead:
				if dashValidSession(r) {
					http.Redirect(w, r, "/", http.StatusFound)
					return
				}
				dashRenderLogin(w, r.URL.Query().Get("err") != "")
			case http.MethodPost:
				ip := dashClientIP(r)
				if dashLocked(ip) {
					http.Error(w, `{"error":"失败次数过多，请5分钟后再试"}`, http.StatusTooManyRequests)
					return
				}
				_ = r.ParseForm()
				user, pass := r.PostFormValue("user"), r.PostFormValue("pass")
				if dashCheck(user, pass) {
					sid := dashNewSession()
					http.SetCookie(w, &http.Cookie{
						Name:     dashCookieName,
						Value:    sid,
						Path:     "/",
						HttpOnly: true,
						SameSite: http.SameSiteLaxMode,
						MaxAge:   int(dashSessionTTL / time.Second),
					})
					log.Printf("[dashauth] 登录成功 ip=%s user=%s", ip, user)
					http.Redirect(w, r, "/", http.StatusFound)
					return
				}
				time.Sleep(dashFailDelay)
				if dashRecordFail(ip) {
					log.Printf("[dashauth] ip=%s 连续失败达上限，锁定 5 分钟", ip)
					http.Error(w, `{"error":"失败次数过多，请5分钟后再试"}`, http.StatusTooManyRequests)
					return
				}
				log.Printf("[dashauth] 登录失败 ip=%s user=%q", ip, user)
				http.Redirect(w, r, "/login?err=1", http.StatusFound)
			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
			return
		}

		if path == "/logout" {
			if c, err := r.Cookie(dashCookieName); err == nil {
				dashDropSession(c.Value)
			}
			http.SetCookie(w, &http.Cookie{
				Name: dashCookieName, Value: "", Path: "/",
				HttpOnly: true, MaxAge: -1,
			})
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		// 两条过门路子：浏览器走 session cookie，其他设备的 SDK 走静态 key。
		// 顺序无所谓（都不过才判未登录），但 Cache-Control: no-store 必须两条
		// 通道都盖上——quota.json 是磁盘静态文件（带 Last-Modified，会被浏览器
		// 启发式缓存），/api/* 走公网代理链，任何一环缓存住旧 JSON 都会伪装成
		// "看板不自动刷新"。
		if dashValidSession(r) || dashTokenOK(r) {
			w.Header().Set("Cache-Control", "no-store")
			h.ServeHTTP(w, r)
			return
		}
		// 未登录：页面导航 → 302 去登录页；接口/fetch → 401 JSON
		if wantsHTML(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized"}`))
	})
}
