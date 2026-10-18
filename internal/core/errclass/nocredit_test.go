package errclass

import "testing"

// NoCreditBody 是探测层 403 分支的全局默认词汇（原内联在 config/validate.go）。
// 逐条钉死：词表一个子串的增删都是「某家后端要不要被拉黑」的语义变更。
func TestNoCreditBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"英文欠费 overdue", `{"error":"payment is overdue"}`, true},
		{"英文余额不足 balance", "Your balance is not enough", true},
		{"英文 insufficient", "insufficient funds to complete the call", true},
		{"英文 quota", "quota exceeded for this month", true},
		{"中文欠费", "账户已欠费，请充值后重试", true},
		{"中文额度", "当前账号额度已用完", true},
		{"大小写混合", "OverDue: please recharge", true},
		{"无关文案不许误伤", `{"error":"access denied by edge policy"}`, false},
		{"模型下架文案不许被算成欠费", `{"error":"model not found"}`, false},
		{"空串", "", false},
	}
	for _, c := range cases {
		if got := NoCreditBody(c.body); got != c.want {
			t.Errorf("%s: NoCreditBody(%q) = %v, 期望 %v", c.name, c.body, got, c.want)
		}
	}
}
