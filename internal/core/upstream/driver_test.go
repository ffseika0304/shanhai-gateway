package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// Driver 是怪癖层/协议族名（core/quirk 的注册表 key），纯 config 字段：
//   - 填了且已注册 → 该后端走这家的怪癖；
//   - 未填 / 没注册 → quirk.Get 回落 OpenAI 兼容通用行为（= 不接怪癖层的今天行为）。
//
// 这条测试钉两头：反序列化要认它，序列化要带回来（admin 热改回写配置不能丢字段）。
func TestBackendDriverJSONField(t *testing.T) {
	var b Backend
	raw := `{"name":"volc-main","model":"m","driver":"volc"}`
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatal(err)
	}
	if b.Driver != "volc" {
		t.Fatalf("Driver = %q, 期望 volc", b.Driver)
	}
	out, err := json.Marshal(&b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"driver":"volc"`) {
		t.Fatalf("序列化丢了 driver 字段: %s", out)
	}

	// 未填 = 空串（旧配置反序列化后不许凭空多出一个 driver）
	var d Backend
	if err := json.Unmarshal([]byte(`{"name":"plain","model":"m"}`), &d); err != nil {
		t.Fatal(err)
	}
	if d.Driver != "" {
		t.Fatalf("Driver = %q, 期望空串（未填）", d.Driver)
	}
}
