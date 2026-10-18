package proxy

import (
	"strings"
	"testing"
)

func TestCaptureTeeTruncation(t *testing.T) {
	tee := &captureTee{max: 10}
	tee.Write([]byte("12345"))
	if tee.truncated || string(tee.buf) != "12345" {
		t.Fatalf("未超限应原样累积: buf=%q truncated=%v", tee.buf, tee.truncated)
	}
	// 第二段跨过上限：截断保留前 10，置 truncated
	tee.Write([]byte("abcdefghij"))
	if !tee.truncated || len(tee.buf) != tee.max || string(tee.buf) != "12345abcde" {
		t.Fatalf("超限应截断到上限: buf=%q len=%d truncated=%v", tee.buf, len(tee.buf), tee.truncated)
	}
	// 已截断后不再追加
	tee.Write([]byte("ZZZ"))
	if string(tee.buf) != "12345abcde" {
		t.Fatalf("截断后不应再写入: %q", tee.buf)
	}
}

func TestCleanJobIDKeepsAllowedAndIsNumber(t *testing.T) {
	// 白名单内字符保留：字母/数字 + .-_:
	cases := []struct {
		in, want string
	}{
		{"abc123_-.XYZ:09", "abc123_-.XYZ:09"},
		// 非法字符被洗掉
		{"job id!@#$%^&*()", "jobid"},
		// ④ Unicode No 类：IsDigit 认不出（会洗成空），IsNumber 必须保留 —— 这就是
		// 用 unicode.IsNumber 而不是 IsDigit 的原因，SLR 上踩过。
		{"½", "½"},         // ½ 是 No 类（fraction），IsDigit=false, IsNumber=true
		{"Job-②", "Job-②"}, // 带圈数字 ② 是 No 类（保留）；'-' 也在白名单内
	}
	for _, c := range cases {
		if got := cleanJobID(c.in); got != c.want {
			t.Errorf("cleanJobID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// 空输入返回空
	if got := cleanJobID(""); got != "" {
		t.Errorf("cleanJobID(\"\") = %q, want empty", got)
	}
	// 超过 64 rune 截断到 64
	long := strings.Repeat("a", 200)
	if got := cleanJobID(long); len([]rune(got)) != 64 {
		t.Errorf("cleanJobID 超长应截 64 rune, got %d", len([]rune(got)))
	}
}

func TestScrubCapture(t *testing.T) {
	cases := []struct{ in, wantSub string }{
		// 内部路径 + sk- 密钥（sk- 命中通用 [key]）
		{`/home/seika/scf-dev/server.py 和 /opt/smart-ops/.env sk-abc12345def67890`, `[home]/scf-dev/server.py 和 [opt] [key]`},
		// /home 只掩用户名留项目名（有意为之）：scf-dev/server.py 应保留
		{`deploy at /home/seika/scf-dev/`, `deploy at [home]/scf-dev/`},
		// 私有/保留 IPv4 段全部掩成 [ip]
		{`server 192.168.31.23, db 10.0.0.5, gw 172.16.0.1, lo 127.0.0.1, ll 169.254.1.1`,
			`server [ip], db [ip], gw [ip], lo [ip], ll [ip]`},
		// 公网 IP 保留不掩（不要过度脱敏；RFC5737 文档段，非真实地址）
		{`public 203.0.113.7`, `public 203.0.113.7`},
		// 中国手机号
		{`联系 13800138000 或 19912345678`, `联系 [phone] 或 [phone]`},
		// URL query 凭据
		{`url?token=abc123&api_key=xyz&access_token=opq&secret=s&auth=u:p&password=p&pwd=q`, `url?token=[redacted]&api_key=[redacted]&access_token=[redacted]&secret=[redacted]&auth=[redacted]&password=[redacted]&pwd=[redacted]`},
		// 鉴权请求行（Bearer 被通用 Authorization 规则整段吞）
		{`Authorization: Bearer eyJhbGciOi.abcdef.xxxx`, `Authorization: [redacted]`},
		// 裸 Bearer 自由文本（无 Authorization: 前缀）也要盖
		{`用 Bearer tok12345 调接口`, `用 Bearer [redacted] 调接口`},
		{`X-Api-Key: abcdef1234567890`, `X-Api-Key: [redacted]`},
		{`Cookie: session=abc; token=def`, `Cookie: [redacted]`},
		// 具体静态密钥先落 [token]（在通用前缀之前）
		{`调用 slr_2e61800473a99f997cef6d6027d328ce`, `调用 [token]`},
		// 通用密钥前缀
		{`keys ark-abc12345 sk-ws-xyz12345 slr_universal12345 gp-key12345`, `keys [key] [key] [key] [key]`},
	}
	for _, c := range cases {
		got := scrubCapture(c.in)
		if got != c.wantSub {
			t.Errorf("scrubCapture(%q)\n  got %q\n want %q", c.in, got, c.wantSub)
		}
	}
}
