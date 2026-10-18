package runtime

import (
	"fmt"
	"os"
	"time"
)

// 环境变量读取的唯一出口。
//
// 搬入前这组三个函数（envOrDefault / durEnv / numEnv）是 seika_llm_router
// main.go 的包内辅助函数。搬到 internal/{core,service} 两层时，各抄了一份
// durEnv + numEnv —— 两处逐字相同，改了一处忘了另一处，就是一个静默的
// 参数分叉：同一个 COOLDOWN_429 在健康状态机里按 60s 算、在兜底隔离里按
// 别的值算，而代码里看不出任何区别。
//
// 解析逻辑照搬，一行都没有"改进"：
//   - numEnv 用 fmt.Sscanf("%f") 而不是 strconv.ParseFloat。区别只在畸形的
//     输入上（"50abc"、"abc"），而这一条正是不该"顺手改好"的地方：运维在
//     故障时手敲环境变量，敲错了就该让这个函数**按它能读到的数生效**，而不是
//     静悄悄回落到另一个值。静默回落出来的配置比配错更难查 —— 没人会想到
//     去核对"我刚才设的 50 是不是根本没生效"。换成 ParseFloat 是行为变更，
//     不是搬家。
//   - 空字符串视为未设置；解析失败一律回落默认值，不 panic、不打日志。
//     理由同上：环境变量是运维在故障时手改的最后手段，任何一次读取失败
//     都不该把正在跑的网关带下去。
//
// 依赖方向：本包是地基，不 import 任何项目内包，所以 core / service 层可以
// 放心依赖它而不会成环。

// EnvString 取字符串环境变量，空字符串视为未设置，返回 def。
func EnvString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// EnvDuration 取时长。解析失败或未设置返回 def。
// 形如 "30s" "5m" "1h20m"（time.ParseDuration 的写法）。
func EnvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return def
}

// EnvFloat 取浮点。解析失败或未设置返回 def。
func EnvFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var f float64
	if _, err := fmt.Sscanf(v, "%f", &f); err == nil {
		return f
	}
	return def
}
