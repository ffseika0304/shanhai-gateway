# SLR Router · Quota 冷却序列（脱敏交换件）

> 供 FALSE_ZERO_TRIAD / run_outcome 夹具的 clean/dirty arm 用。
> 数据源自一个生产 LLM 轮转网关（Go，单进程，多 upstream 池化）2026-10-19 → 10-30 的运行日志。

## 字段

### quota-cooldown-sequence.jsonl（冷却事件，主动退避）

```json
{"ts": "2026/10/19 01:09:12.412922", "upstream": "upstream_01", "cooldown": "1m0s", "reason": "timeout"}
```

- `reason ∈ {timeout, rate_limit_429, network}` —— 进入冷却的触发原因
- `cooldown` —— 冷却档位，**同 upstream 连续失败时按指数阶梯升级**：
  `1m0s → 2m0s → 4m0s → 8m0s → 16m0s → 32m0s → 1h4m0s → 2h8m0s → 4h0m0s`
- `upstream` 为匿名 ID；真实标识（厂商/账号/区域）已剥离，映射表不外发。

### circuit-transitions.jsonl（熔断器状态机）

```json
{"ts": "...", "upstream": "upstream_03", "state": "TRIPPED", "detail": "(5 consecutive fails)"}
```

- `state ∈ {TRIPPED, HALF-OPEN}`；触发条件 = 连续 N 次失败（N 递增：5,6,7…）；
  HALF-OPEN 后探针成功即闭合，失败则重新 TRIPPED 并升 N。

### throttle-attempts.jsonl（限流请求回执）

```json
{"ts": "...", "upstream": "upstream_02", "seq": 1, "http": 429, "class": "rate_limit_429"}
```

- `class=skip_capacity`：网关**自身**并发槽满，请求未发出（router saturated）——
  这是「网关层拒绝」与「上游返回 429」的分层，**两者 HTTP 码相同但归属不同**，
  混记会污染上游健康统计（正是 dirty arm 想抓的那类）。

## digest

`quota-cooldown-sequence.jsonl` sha256 前 16 位见随附消息。

## 已知边界（诚实声明）

- 时间戳为日志墙钟，网关与上游时钟未对齐；序列内自洽。
- `cooldown` 取自配置档位而非实测恢复时刻——上游实际限流窗口可能短于档位。
- 无 per-key 配额数字（账号级配额不外发）。
