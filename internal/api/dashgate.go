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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"shanhai-gateway/internal/runtime"
	"shanhai-gateway/internal/service/proxy"
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

// dashToken 取当前生效的 key。优先级（2026-10-19 面板改 key 功能）：
//  1. DASH_TOKEN 环境变量显式设置——含空串（systemd `Environment=DASH_TOKEN=`）
//     = 整条通道关闭。env 是部署者的最终决定，此时持久化文件被无视；
//  2. 持久化文件 DashTokenFile——部署者没配 env 时，看板上 POST /api/dashkey
//     改的 key 原子落盘这里，即改即生效（免重启）；
//  3. 内置默认——统一密钥只当「进门槛」用：默认值随源码公开，任何部署都该在
//     到手后第一时间于面板上换掉它。
//
// 文件每次门检现读（微小文件，量级同 quota.json 静态文件），换零缓存失效逻辑。
func (h *Handlers) dashToken() string {
	if v, ok := os.LookupEnv("DASH_TOKEN"); ok {
		return strings.TrimSpace(v)
	}
	if h.DashTokenFile != "" {
		if b, err := os.ReadFile(h.DashTokenFile); err == nil {
			return strings.TrimSpace(string(b))
		}
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
func (h *Handlers) dashTokenOK(r *http.Request) bool {
	want := h.dashToken()
	if want == "" {
		return false
	}
	got := dashBearer(r)
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// dashKeyHandler 看板文档页的静态 key 端点，两口共用 mux（见 router.go 注册处）：
//   - GET/HEAD：把当前生效的 key 吐给文档页，供就地复制 / 拼 curl 示例。
//   - POST：改 key（2026-10-19 用户指令「统一密钥到手，让别人自己改，只要后端
//     认就行」）。body {"token":"<新key>"}，trim 后原子落盘 DashTokenFile
//     （0600）并即时生效——dashToken 每次门检现读，不用重启；提交空串 =
//     持久化关闭通道。DASH_TOKEN env 显式设置时 409 拒改（改了也不生效，
//     直说免得白高兴）。旧 key 立即作废。
//
// 公网口被 dashGate 拦着，**必须先登录**才碰得到；LAN 口 :18006 按文档 §九
// 本就免鉴权，能随便调 LLM 的人本就能改 key，不新增攻击面。
func (h *Handlers) dashKeyHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		tok := h.dashToken()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":   tok,
			"enabled": tok != "",
			"env":     "DASH_TOKEN",
		})
	case http.MethodPost:
		h.dashKeyChange(w, r)
	default:
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
	}
}

// dashKeyChange POST 改 key 的实际逻辑（语义见 dashKeyHandler 注释）。
// 成功后同步 proxy.SetDynamicSecret：新 key 即刻进 capture 脱敏，不进留存库。
func (h *Handlers) dashKeyChange(w http.ResponseWriter, r *http.Request) {
	if _, ok := os.LookupEnv("DASH_TOKEN"); ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "DASH_TOKEN 环境变量显式设置中：改文件不会生效，请先去掉该环境变量再改",
		})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": `body 须为 {"token":"<新key>"} 的 JSON`})
		return
	}
	tok := strings.TrimSpace(body.Token)
	if len(tok) > 256 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "key 过长（上限 256 字符）"})
		return
	}
	if err := h.writeDashTokenFile(tok); err != nil {
		log.Printf("[dashgate] 改 key 落盘失败: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "落盘失败，见服务端日志"})
		return
	}
	proxy.SetDynamicSecret(tok) // capture 脱敏跟新 key 走，新密钥不进留存库
	if tok == "" {
		log.Printf("[dashgate] 看板静态 key 已在面板上清空 = 通道关闭")
	} else {
		log.Printf("[dashgate] 看板静态 key 已在面板上修改并即时生效")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "enabled": tok != ""})
}

// writeDashTokenFile 原子落盘（同目录 tmp + rename，0600）。空串写空文件 =
// 持久化关闭通道。不落日志、不进响应——密钥只在文件与内存里。
func (h *Handlers) writeDashTokenFile(tok string) error {
	if h.DashTokenFile == "" {
		return errors.New("DashTokenFile 未接线（旧二进制？）")
	}
	if err := os.MkdirAll(filepath.Dir(h.DashTokenFile), 0o700); err != nil {
		return err
	}
	tmp := h.DashTokenFile + ".tmp"
	fd, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fd.WriteString(tok); err != nil {
		fd.Close()
		return err
	}
	if err := fd.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, h.DashTokenFile)
}

// EffectiveDashToken 供装配层（main.go）在启动时把当前生效 key 塞进 capture
// 脱敏的动态槽（proxy.SetDynamicSecret）；运行期再改由 dashKeyChange 同步。
func (h *Handlers) EffectiveDashToken() string { return h.dashToken() }

// ---------------------------------------------------------------------------
// 会话：自签 cookie（2026-10-20）
//
// 原来会话表在内存 map 里：网关一重启，**所有已登录 cookie 当场作废**（当天换
// 前端重启三次，用户浏览器直接被踢回登录页，页面一直报"API 连接中断"）。
// 改成**自签 cookie**：值 = id.exp.mac，服务端只验签名与过期，**不存表**
// ⇒ 网关重启无感。
//
// 注销要能作废（自签 cookie 天然做不到"服务端删一条"）⇒ 保留一张「已作废 id」
// 小表落盘，只在校签时查；上限截断（只留最近 1000 条）。
//
// 滑动续期：验签通过且剩余不足 3 天时，重新签一个 7 天下发（无状态，不写盘）。
// ---------------------------------------------------------------------------

const dashSessionIDBytes = 16 // id 随机字节（128bit，暴破无意义）
const dashRevokedMax = 1000

var (
	dashKeyMu       sync.Mutex
	dashKeyByPath   = map[string][]byte{} // 密钥文件路径 → 密钥（**按路径**缓存：换路径=换密钥）
	dashKeyVolatile []byte                // 无路径时的进程内临时密钥（重启即失效）
)

// dashSessionKey 取会话签名密钥：<dataDir>/dash-session.key（0600），
// 首次用时随机生成并原子落盘。放 DataDir 下——同 QuotaFile / DashTokenFile 的理由。
//
// **按路径缓存**（不是全局一份）：不同部署（不同密钥文件）不能互相认 cookie；
// 同一路径的多个 Handlers 实例共享一把 ⇒ 换进程/重启后老 cookie 仍有效。
func (h *Handlers) dashSessionKey() []byte {
	path := h.DashSessionKeyFile
	dashKeyMu.Lock()
	defer dashKeyMu.Unlock()
	if path == "" {
		// 没有路径（理论上不会出现）：退化成进程内临时密钥 ⇒ 重启即失效（旧行为）
		if len(dashKeyVolatile) == 0 {
			b := make([]byte, 32)
			_, _ = rand.Read(b)
			dashKeyVolatile = b
		}
		return dashKeyVolatile
	}
	if k, ok := dashKeyByPath[path]; ok {
		return k
	}
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		dashKeyByPath[path] = b[:32]
		return dashKeyByPath[path]
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		_, _ = rand.Read(b) // crypto/rand 极小概率失败：仍能跑（只是重启会失效）
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err == nil {
			_ = os.Rename(tmp, path) // 原子替换：并发起进程也不会读到半个文件
		}
	}
	dashKeyByPath[path] = b
	return b
}

// dashSessionMac 算签名：HMAC-SHA256(key, id|exp)
func dashSessionMac(key []byte, id string, exp int64) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%s|%d", id, exp)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// dashSignSession 造 cookie 值：id.exp.mac
func (h *Handlers) dashSignSession(id string, exp time.Time) string {
	e := exp.Unix()
	return id + "." + strconv.FormatInt(e, 10) + "." + dashSessionMac(h.dashSessionKey(), id, e)
}

// dashSessionParts 拆 cookie 值
func dashSessionParts(v string) (id string, exp int64, mac string, ok bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return "", 0, "", false
	}
	e, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", 0, "", false
	}
	return parts[0], e, parts[2], true
}

// dashRevokedFile 已作废 id 小表（与密钥同目录）
func (h *Handlers) dashRevokedFile() string {
	if h.DashSessionKeyFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(h.DashSessionKeyFile), "dash-revoked")
}

// dashSessionRevoked 该 id 是否已作废（注销时写进来）
func (h *Handlers) dashSessionRevoked(id string) bool {
	p := h.dashRevokedFile()
	if p == "" {
		return false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == id {
			return true
		}
	}
	return false
}

// dashRevokeSession 作废一个 id（注销）：追加到小表，超上限截断
func (h *Handlers) dashRevokeSession(id string) {
	p := h.dashRevokedFile()
	if p == "" {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(mustRead(p))), "\n")
	lines = append(lines, id)
	if len(lines) > dashRevokedMax {
		lines = lines[len(lines)-dashRevokedMax:]
	}
	_ = os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

func mustRead(p string) []byte {
	b, _ := os.ReadFile(p)
	return b
}

// dashNewSession 造一个已签名的 cookie 值（id + 7 天）
func (h *Handlers) dashNewSession() string {
	b := make([]byte, dashSessionIDBytes)
	if _, err := rand.Read(b); err != nil {
		_, _ = rand.Read(b) // crypto/rand 在 Linux 上不该失败
	}
	id := hex.EncodeToString(b)
	return h.dashSignSession(id, time.Now().Add(dashSessionTTL))
}

// dashValidSession 校验 cookie；有效则顺延续期（剩余不足 3 天时重签下发）
func (h *Handlers) dashValidSession(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie(dashCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	id, exp, mac, ok := dashSessionParts(c.Value)
	if !ok {
		return false
	}
	want := dashSessionMac(h.dashSessionKey(), id, exp)
	if subtle.ConstantTimeCompare([]byte(mac), []byte(want)) != 1 {
		return false
	}
	if time.Now().After(time.Unix(exp, 0)) {
		return false
	}
	if h.dashSessionRevoked(id) {
		return false
	}
	// 滑动续期：剩余不足 3 天就重签一个 7 天的（无状态，不写盘）
	if time.Until(time.Unix(exp, 0)) < 3*24*time.Hour {
		http.SetCookie(w, &http.Cookie{
			Name:     dashCookieName,
			Value:    h.dashSignSession(id, time.Now().Add(dashSessionTTL)),
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(dashSessionTTL / time.Second),
		})
	}
	return true
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
// h 是 Handlers（静态 key 校验要读它上面的生效 key），next 是业务 mux。
func dashGate(h *Handlers, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		if path == "/login" {
			switch r.Method {
			case http.MethodGet, http.MethodHead:
				if h.dashValidSession(w, r) {
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
					sid := h.dashNewSession()
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
				h.dashRevokeSession(c.Value)
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
		if h.dashValidSession(w, r) || h.dashTokenOK(r) {
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
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
