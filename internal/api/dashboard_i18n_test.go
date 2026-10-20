package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 看板前端的两条硬约束（2026-10-20 踩过的坑：页面整个打不开，
// 报 "Uncaught ReferenceError: Cannot access 't' before initialization"）。
//
// ① i18n 字典 T 里不能出现 `:t(` 自调用。T 是**字面量**，求值发生在脚本加载时；
//    而 `const t` 若定义在字典之后 ⇒ 字典里任何 t() 调用都踩暂时性死区(TDZ)，
//    整个脚本抛异常、页面 JS 全死（静态 HTML 还在，动态部分全空）。
//    gofmt / node --check 都查不出这个——它们只查语法，不查求值顺序。
//    （本仓已把 const t/tf 挪到 T 字面量之前，字典里用 t() 也安全；但自调用仍是
//    语义错误——t() 查的就是这个正在求值的键 ⇒ 取不到值。）
// ② 每个 data-i18n="key" 在 zh / en 两段都要有，否则切语言留空。
func TestDashboardI18nIntegrity(t *testing.T) {
	raw, err := os.ReadFile("../../web/dashboard/index.html")
	if err != nil {
		t.Fatalf("读看板失败: %v", err)
	}
	s := string(raw)

	ti := strings.Index(s, "const T = {")
	if ti < 0 {
		t.Fatal("看板里找不到 const T = {")
	}
	lit := jsBlock(s, strings.Index(s[ti:], "{")+ti)

	// ① 字典里不得自调用 t(...)
	for _, line := range strings.Split(lit, "\n") {
		if regexp.MustCompile(`:\s*t\(`).MatchString(line) {
			t.Errorf("i18n 字典里出现自调用 t(...)：%q\n"+
				"字典是字面量、求值时 t 仍在 TDZ ⇒ 整个脚本抛异常、页面 JS 全死。"+
				"请直接写字符串字面量。", strings.TrimSpace(line))
		}
	}

	// ①b const t 必须定义在 T 字面量之前（否则字典里任何 t() 都是 TDZ）
	tDef := strings.Index(s, "const t = (k) =>")
	if tDef < 0 {
		t.Fatal("看板里找不到 const t = (k) =>")
	}
	if tDef > ti {
		t.Errorf("const t 定义在 T 字面量之后（t@%d > T@%d）：字典求值时 t 仍在 TDZ ⇒ 页面 JS 全死",
			tDef, ti)
	}

	// ② data-i18n 的每个键在 zh / en 两段都要有
	used := map[string]bool{}
	for _, m := range regexp.MustCompile(`data-i18n="([^"]+)"`).FindAllStringSubmatch(s, -1) {
		used[m[1]] = true
	}
	if len(used) == 0 {
		t.Fatal("看板里没有 data-i18n 用法，i18n 机制变了？")
	}
	for _, lang := range []string{"zh", "en"} {
		marker := lang + ":"
		li := strings.Index(lit, marker)
		if li < 0 {
			t.Fatalf("i18n 字典里找不到 %s 段", lang)
		}
		sec := jsBlock(lit, strings.Index(lit[li:], "{")+li)
		for k := range used {
			if !regexp.MustCompile(`(?m)(^|[\s,{])` + regexp.QuoteMeta(k) + `:`).MatchString(sec) {
				t.Errorf("data-i18n=%q 在 %s 段里缺键", k, lang)
			}
		}
	}
}

// jsBlock 从 s[i]=='{' 处返回配平后的大括号块；跳过字符串字面量与注释，
// 免得 "{n}" 这类占位符或代码里的括号把配平算歪。
func jsBlock(s string, i int) string {
	depth := 0
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '"', '\'', '`':
			q := s[j]
			j++
			for j < len(s) {
				if s[j] == '\\' {
					j++
					continue
				}
				if s[j] == q {
					break
				}
				j++
			}
		case '/':
			if j+1 < len(s) && s[j+1] == '/' {
				for j < len(s) && s[j] != '\n' {
					j++
				}
				continue
			}
			if j+1 < len(s) && s[j+1] == '*' {
				j += 2
				for j+1 < len(s) && !(s[j] == '*' && s[j+1] == '/') {
					j++
				}
				j++
			}
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[i : j+1]
			}
		}
	}
	return s[i:]
}
