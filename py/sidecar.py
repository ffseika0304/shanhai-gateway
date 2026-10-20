#!/usr/bin/env python3
"""
gateway sidecar 控制器（自调控大脑）

从生产网关原样移植（行为逐位等价，只改边界：下述新增/新增的 env 均为
地址与文件路径的可配置项，未改任何判定语义）。

仅依赖 Python 标准库：json / os / time / urllib.request。
（见 py/requirements.txt —— 本脚本无第三方依赖。）

职责：
  - 每 ~5s 轮询 Go 网关的 /api/stats（细粒度：每后端 attempts/success/failed/失败类型/冷却/熔断/首字延迟）
  - 按**窗口增量**失败率（不是累计值），经 admin API 调并发/启停，实现限流/降级自愈
  - Go 网关负责快路径（429/403/401/400/5xx/慢流/空气泡 的分级降级 + 配额类指数冷却），sidecar 只做慢周期决策
  - 自动剔除持续失败的**兜底**后端（额度用完、模型下线等）
  - **首发失败率只观测不动手**（2026-09-24 定调）：拿首发 enabled=False 是"永久摘除"
    （重启才回来），比网关的指数冷却更糟

上面那句有两条**确定性规则**例外（2026-09-28 用户定，"不用每次都和瞎子一样等熔断"）。
它们读的是配额/余额的**真实数据**而不是失败率，所以不违背"失败率只观测"：

  ① CN 规则（cn_tick，60s）—— wb_cn_hy3 与 wb_cn_ds41 **并存各烧各的**，不做切换：
       · 号池空（两号余额均 < 地板）→ 两个条目一起关（账号级地板由面板
         pool.min_credits_cn 挡聊天，这里只判"池子空了没"，别拿请求撞必然失败的上游）
       · hy3 熄灭信号：连续 >= SIDECAR_CN_EXHAUST_FAILS 个失败窗口，或台账
         cost_per_1k > 0 → **只关 hy3 条目**；标识到每日 CN_RESET_HOUR（默认 00:00）清
       · 台账扣分归因不了是哪条烧的（两条并存后余额下降说明不了什么），所以
         **不做余额下降信号**；台账恒 0 时信号 B 自然不触发
  ② 配额窗规则（quota_tick，60s）——读 quota.json（quota_probe 每 180s 写），
     opencode / 火山 四条首发任一窗（5 小时 / 7 天 / 30 天）打满阈值 → 关该后端，
     全部回落到阈值下 → 开回来。出错 / 缺凭证 / 窗口缺失 / stale → 不动作。

两条规则摘掉的条目**不许失败率恢复分支复活**（_held_reason 闸门），否则两边打架
"摘-复活-再摘"打摆子。规则只在运行期生效：不落盘，网关重启回配置值，侧车重启
按当时的 stats / 面板 / 配额数据重新判定。

边界（符合"唯一通道=接口契约"）：
  - sidecar 只读 stats / 面板 /status / quota.json，只写 admin API，不碰业务数据、不直接调上游

env:
  SIDECAR_URL               网关地址 默认 http://127.0.0.1:18006
  SIDECAR_TOKEN             共享密码。**默认空**（空 = 不发 Authorization 头，适配网关 no-auth 模式）
  SIDECAR_INTERVAL          轮询秒 默认 5
  SIDECAR_BASE_CONC         每后端基准并发 默认 16
  ROUTER_BACKENDS_FILE      后端配置文件路径。**要和 Go 网关用同一个**，否则 sidecar 改配置会改到另一个文件
  GATEWAY_BACKENDS_FILE     同上（Go 网关侧的新名字，作为上一项的兜底；两者都设时以上一个为准）
  SIDECAR_CN_FREE_BACKEND   CN 免费条目（cn:hy3）默认 wb_cn_hy3；与付费条目任一为空 = CN 规则关停
  SIDECAR_CN_PAID_BACKEND   CN 付费条目（cn:deepseek-v4.1-flash）默认取 SIDECAR_GUARD_BACKEND、再默认 wb_cn_ds41
  SIDECAR_GUARD_FLOOR       每号余额地板 默认 500（必须与面板 pool.min_credits_cn 一致）
  SIDECAR_CN_EXHAUST_FAILS  免费条目连续失败窗口阈值 默认 3（到即关 hy3 条目）
  SIDECAR_CN_RESET_HOUR     每日清 CN 熄灭标识的钟点 默认 0（00:00；老名 SIDECAR_GUARD_RESET_HOUR）
  SIDECAR_GUARD_INTERVAL    CN 规则判定周期秒 默认 60
  WB_PANEL_URL              面板地址 默认 http://127.0.0.1:7863
  WB_PANEL_CONFIG           面板 config.json 路径（取面板 api_key；默认 <repo>/workbuddy2api-panel/config.json，
                            即面板与网关并列部署的既有布局；面板单独部署时用它指过去）
  SIDECAR_QUOTA_FILE        配额数据文件 默认 <用户数据目录>/gateway/quota.json（镜像 Go 网关
                            runtime.Paths.QuotaFile 的默认：XDG_DATA_HOME 或 ~/.local/share；
                            也可设 GATEWAY_QUOTA_FILE 与网关取同一路径，判定走同一个数据源。原
                            <repo>/go/static/quota.json 默认指向旧仓库布局，新仓库不存在，故改镜像）
  SIDECAR_QUOTA_INTERVAL    配额规则判定周期秒 默认 60
  SIDECAR_QUOTA_STALE_S     文件比这个更旧就不动作 默认 900（quota_probe 每 180s 写一次）
  SIDECAR_QUOTA_PCT         打满阈值% 默认 100（任一窗 ≥ 即关后端）
"""
from __future__ import annotations

import json
import os
import time
import urllib.request

URL = os.getenv("SIDECAR_URL", "http://127.0.0.1:18006")
# 默认空：不再硬编码共享密码。要鉴权时用 env 给 SIDECAR_TOKEN。
TOKEN = os.getenv("SIDECAR_TOKEN", "")
INTERVAL = float(os.getenv("SIDECAR_INTERVAL", "5"))
BASE_CONC = int(os.getenv("SIDECAR_BASE_CONC", "16"))

FAIL_HIGH = float(os.getenv("SIDECAR_FAIL_HIGH", "0.40"))  # 高于此 → 摘流
FAIL_LOW = float(os.getenv("SIDECAR_FAIL_LOW", "0.10"))    # 低于此 → 恢复
MIN_SAMPLE = int(os.getenv("SIDECAR_MIN_SAMPLE", "5"))     # 样本太少不下判断
AUTO_DEGRADE_THRESHOLD = int(os.getenv("SIDECAR_AUTO_DEGRADE", "3"))   # 连续失败次数阈值，超过则降级
AUTO_DISABLE_THRESHOLD = int(os.getenv("SIDECAR_AUTO_DISABLE", "8"))  # 连续失败次数阈值，超过则禁用

_prev: dict[str, tuple[int, int]] = {}  # name -> (attempts, failed)
_prev_bd: dict[str, dict[str, int]] = {}  # name -> {class: 累计次数} 上轮快照（用来算**本窗口增量**）
_consecutive_fails: dict[str, int] = {}  # name -> 连续失败次数
_observed: dict[str, bool] = {}          # name -> 最近一次成功拉到的 enabled（规则层据此下发）


def _auth(req: urllib.request.Request) -> None:
    if TOKEN:
        req.add_header("Authorization", "Bearer " + TOKEN)


def _get_stats() -> dict | None:
    req = urllib.request.Request(URL + "/api/stats")
    _auth(req)
    try:
        with urllib.request.urlopen(req, timeout=5) as r:
            return json.loads(r.read().decode("utf-8"))
    except Exception as e:  # noqa: BLE001
        print(f"[sidecar] stats fetch failed: {e}", flush=True)
        return None


def _patch(name: str, concurrency: int | None = None,
           weight: float | None = None, enabled: bool | None = None) -> bool:
    body = {}
    if concurrency is not None:
        body["concurrency"] = concurrency
    if weight is not None:
        body["weight"] = weight
    if enabled is not None:
        body["enabled"] = enabled
    if not body:
        return True
    req = urllib.request.Request(URL + f"/admin/backend/{name}",
                                 data=json.dumps(body).encode(), method="POST")
    req.add_header("Content-Type", "application/json")
    _auth(req)
    try:
        with urllib.request.urlopen(req, timeout=5) as r:
            print(f"[sidecar] PATCH {name} {body} -> {r.read().decode().strip()}", flush=True)
            return True
    except Exception as e:  # noqa: BLE001
        print(f"[sidecar] PATCH {name} {body} failed: {e}", flush=True)
        return False


def _config_path() -> str:
    """和 Go 网关取同一个配置文件：优先 ROUTER_BACKENDS_FILE，再兜底 Go 网关侧的
    GATEWAY_BACKENDS_FILE，否则回退到仓库内的相对路径（<repo>/config/backends.json，
    与 Go 网关 runtime.Paths.BackendsFile 的仓库内默认同位）。
    两边必须一致——sidecar 会删/降级后端，路径不一致就会改到另一个文件（症状极隐蔽）。"""
    p = os.getenv("ROUTER_BACKENDS_FILE", "")
    if not p:
        p = os.getenv("GATEWAY_BACKENDS_FILE", "")
    if p:
        return p
    return os.path.join(os.path.dirname(__file__), "..", "config", "backends.json")


def _load_backends_list(path: str) -> tuple[dict | list, list]:
    """读取配置，兼容新格式 {accounts, backends} 与旧格式（顶层数组）。
    返回 (原始配置对象, backends 列表)。"""
    with open(path, "r", encoding="utf-8") as f:
        cfg = json.load(f)
    backends = cfg.get("backends", []) if isinstance(cfg, dict) else cfg
    return cfg, backends


def _configured_map() -> dict[str, dict]:
    """读配置里各后端的 enabled / concurrency。
    concurrency 恢复时用**配置值**而不是固定基准 16，否则一次 re-enable 会把配置为 8
    的后端悄悄改成 16（账号额度对不上）；enabled 用来别替配置就关着的条目开闸。"""
    out: dict[str, dict] = {}
    try:
        _, backends = _load_backends_list(_config_path())
        for b in backends:
            if b.get("name"):
                out[b["name"]] = {
                    "enabled": bool(b.get("enabled", True)),
                    "concurrency": int(b.get("concurrency") or 0),
                }
    except Exception as e:  # noqa: BLE001
        print(f"[sidecar] read config failed: {e}", flush=True)
    return out


def _degrade_backend(name: str) -> None:
    """降级后端：运行期把并发压到 1 —— **立即生效**（走 admin API）。
    不写配置文件：老做法只改文件、网关运行期根本不重读，等于假动作；
    且配置被改成 1 后没有任何自动恢复路径，重启都回不去。
    现在配置保持健康原值，降级是纯运行期状态，失败率回到低位时自动恢复配置并发。
    （weight 已于 2026-09-28 接入首发轮转；降级仍只压并发——weight 是长期
    流量配比，不做故障瞬时手段，动它会把三家主力的配比打乱。）"""
    _patch(name, concurrency=1)


# ============ 规则 ①：CN 双条目并存（2026-09-28 取代"烧分三态状态机"） ============
# 规格（09-28 用户定）：
#   · wb_cn_hy3（cn:hy3，每日免费额度 x0.00）和 wb_cn_ds41（cn:deepseek-v4.1-flash
#     x0.11）**同时在轮转**，各自 weight 0.5 各烧各的——没有 free/paid/hold 切换，
#     没有 cnburn.json 落盘，没有余额下降归因（两条并存后归因不了）
#   · 号池空（两号均 < 地板）→ 两个条目一起关；任一号爬回地板 → 开回来
#   · hy3 耗尽只关 hy3 条目，标识每日 CN_RESET_HOUR（默认 00:00）清一次从头再试
#   · 号池空是**活条件**（每轮按面板实况重算）；耗尽/台账是**标识**（到点才清）
#   · 关停整个规则：SIDECAR_CN_FREE_BACKEND= 或 SIDECAR_CN_PAID_BACKEND= 空串
CN_FREE_BACKEND = os.getenv("SIDECAR_CN_FREE_BACKEND", "wb_cn_hy3")
CN_PAID_BACKEND = os.getenv("SIDECAR_CN_PAID_BACKEND",
                            os.getenv("SIDECAR_GUARD_BACKEND", "wb_cn_ds41"))
CN_ENTRIES = tuple(n for n in (CN_FREE_BACKEND, CN_PAID_BACKEND) if n)
CN_PANEL_URL = os.getenv("WB_PANEL_URL", "http://127.0.0.1:7863")
CN_FLOOR = float(os.getenv("SIDECAR_GUARD_FLOOR", "500"))    # 与面板 pool.min_credits_cn 一致
CN_INTERVAL = float(os.getenv("SIDECAR_GUARD_INTERVAL", "60"))
CN_RESET_HOUR = int(os.getenv("SIDECAR_CN_RESET_HOUR",
                              os.getenv("SIDECAR_GUARD_RESET_HOUR", "0")))
CN_EXHAUST_FAILS = int(os.getenv("SIDECAR_CN_EXHAUST_FAILS", "3"))

_cn_hold: dict[str, str] = {}        # 条目 -> 熄灭标识（耗尽/台账），每日 CN_RESET_HOUR 清
_cn_pool_hold = False                # 号池空（活条件，每轮按面板重算）
_cn_cleared_at = time.time()         # 上次每日清零时刻（不落盘：重启即从头判）
_cn_last_poll = 0.0
_cn_exhaust_w = 0                    # 免费条目连续失败窗口计数（tick() 侧维护）
_cn_exhaust_at = 0.0                 # 最近一次失败窗口时刻（陈旧计数不采信）


def _cn_reset_point(now_ts: float) -> float:
    """最近一个**已过**的本地 CN_RESET_HOUR（默认 00:00）的 unix 秒。
    早于它的熄灭标识才有资格清，否则 00:30 会把 00:10 刚立的标识立刻清掉。"""
    lt = time.localtime(now_ts)
    point = time.mktime((lt.tm_year, lt.tm_mon, lt.tm_mday, CN_RESET_HOUR, 0, 0, 0, 0, -1))
    if now_ts < point:
        point -= 86400
    return point


def _cn_read_status() -> dict | None:
    """读面板 /status → {credits:{uid:int}, hy3_paid:bool}（只取 CN 账号）。
    面板 key 单一来源 = 面板自己的 config.json，不复制密钥到这边。
    config.json 的路径默认沿用「面板与网关并列部署」的既有相对布局，
    面板单独部署时用 WB_PANEL_CONFIG 指过去（只改边界，不改读写语义）。"""
    try:
        cfg_path = os.getenv(
            "WB_PANEL_CONFIG",
            os.path.join(os.path.dirname(__file__), "..",
                         "workbuddy2api-panel", "config.json"))
        with open(cfg_path, "r", encoding="utf-8") as f:
            key = json.load(f).get("api_key", "")
        if not key:
            return None
        req = urllib.request.Request(CN_PANEL_URL + "/status")
        req.add_header("Authorization", "Bearer " + key)
        with urllib.request.urlopen(req, timeout=5) as r:
            data = json.loads(r.read().decode("utf-8"))
        credits: dict[str, int] = {}
        hy3_paid = False
        for a in data.get("accounts", []):
            if a.get("realm") != "cn" or not a.get("uid"):
                continue
            uid = str(a["uid"])
            credits[uid] = int(a.get("credits", 0) or 0)
            for mc in a.get("model_costs") or []:
                m = str(mc.get("model", ""))
                if m in ("hy3", "cn:hy3") and float(mc.get("cost_per_1k", 0) or 0) > 0:
                    hy3_paid = True
        if not credits:
            return None
        return {"credits": credits, "hy3_paid": hy3_paid}
    except Exception as e:  # noqa: BLE001
        print(f"[cn] panel /status read failed: {e}", flush=True)
        return None


# ============ 规则 ②：配额窗摘除（opencode / 火山 首发） ============
# quota_probe.py 每 180s 写一次 quota.json，本层只读文件不碰上游。
# 判据是"配额用了多少"这种能直接算出来的事实——不等 429/熔断瞎子摸象。
#
# 默认路径镜像 Go 网关 runtime.Paths.QuotaFile 的默认（XDG 数据目录下
# gateway/quota.json）：网关 /quota.json 端点供的就是这一个文件，判定必须
# 吃同一份数据。GATEWAY_QUOTA_FILE 是网关侧的覆盖口（runtime/paths.go），
# 跟着走可以保证网关与 sidecar 永远读同一个文件；SIDECAR_QUOTA_FILE 仍是
# 本脚本自己的优先开关（env 兼容，优先级不变）。旧仓库布局下的
# <repo>/go/static/quota.json 已不存在，故默认值改到镜像路径。
def _default_quota_file() -> str:
    """镜像 Go 网关 runtime.DefaultDataDir() + QuotaFile 默认值的解析。"""
    data_home = os.getenv("XDG_DATA_HOME", "")
    if not os.path.isabs(data_home):
        data_home = os.path.join(os.path.expanduser("~"), ".local", "share")
    return os.path.join(data_home, "gateway", "quota.json")


_quota_env = os.getenv("SIDECAR_QUOTA_FILE", "")
if not _quota_env:
    _quota_env = os.getenv("GATEWAY_QUOTA_FILE", "")
QUOTA_FILE = os.path.normpath(_quota_env) if _quota_env else _default_quota_file()
QUOTA_INTERVAL = float(os.getenv("SIDECAR_QUOTA_INTERVAL", "60"))
QUOTA_STALE_S = float(os.getenv("SIDECAR_QUOTA_STALE_S", "900"))
QUOTA_FULL_PCT = float(os.getenv("SIDECAR_QUOTA_PCT", "100"))
# 后端名 -> quota.json 里的路径（分组键见 quota_probe.py）
QUOTA_MAP: dict[str, tuple[str, ...]] = {
    "opencodefsq": ("opencode", "fsq"),
    "opencode827": ("opencode", "827"),
    "volc_pro": ("volc", "coding"),   # 旧格式别名（采集器已改按后端名分组，保留作回落）
    "volc_plan": ("volc", "agent"),   # 同上
}
QUOTA_LANES = ("rolling", "weekly", "monthly")   # 5 小时 / 7 天 / 30 天
# 多订阅（2026-10-20）：volc 侧配额组键 = **后端名**（采集器按后端分组），
# 所以新订阅加进 backends.json 就自动被配额规则管到——不用改这张表。
# opencode 侧组键是账号短名（多个后端可共用一个账号），仍走静态表。
VOLC_PLAN_PATHS = ("/api/coding/v3", "/api/plan/v3")


def _quota_map_effective() -> dict[str, tuple[str, ...]]:
    """后端名 -> 生效的组路径。

    volc 套餐后端：("volc", 后端名)（per-订阅，新格式）；其余后端取静态表。
    """
    out: dict[str, tuple[str, ...]] = {}
    try:
        _, backends = _load_backends_list(_config_path())
    except Exception:  # noqa: BLE001
        backends = []
    for b in backends:
        name = b.get("name")
        if not name:
            continue
        if any(p in (b.get("base_url") or "") for p in VOLC_PLAN_PATHS):
            out[name] = ("volc", name)
    for name, path in QUOTA_MAP.items():
        out.setdefault(name, path)
    return out


def _quota_candidates(name: str) -> list[tuple[str, ...]]:
    """后端名 -> 它在 quota.json 里可能的组路径（按优先级）。

    volc 套餐后端：先试 ("volc", 后端名)（新格式，per-订阅），再试静态别名（旧格式）。
    其余后端：只有静态表那一条。
    """
    cands: list[tuple[str, ...]] = []
    eff = _quota_map_effective().get(name)
    if eff:
        cands.append(eff)
    legacy = QUOTA_MAP.get(name)
    if legacy and legacy != eff:
        cands.append(legacy)
    return cands

_quota_hold: dict[str, str] = {}     # 后端 -> 打满原因
_quota_last_poll = 0.0
_quota_note = ""                     # 上次的数据告警（同一条只打一次）


def _held_reason(name: str) -> str | None:
    """规则层当前为什么把这个条目按着关；None = 没在按。
    失败率恢复分支据此让路——规则摘的条目不许它复活，否则两边打架打摆子。"""
    if name in _cn_hold:
        return _cn_hold[name]
    if _cn_pool_hold and name in CN_ENTRIES:
        return f"CN 号池空（各号 < {CN_FLOOR:g}）"
    if name in _quota_hold:
        return _quota_hold[name]
    return None


def _apply(name: str, want: bool, why: str, cfg: dict[str, dict]) -> None:
    """把条目运行期 enabled 对齐到 want。幂等：没观测到 / 已达标就不出手。
    开的时候带上配置并发——关的时候并发不动，只有重新开才需要抬回配置值。"""
    have = _observed.get(name)
    if have is None:
        return   # 没载入（配置关着 / 手动标签期）或 stats 没拉到：不碰
    if want and not cfg.get(name, {}).get("enabled", True):
        return   # 配置里就关着的条目：不替它开（配置是唯一真源）
    if have == want:
        return
    body: dict = {"enabled": want}
    if want:
        body["concurrency"] = int(cfg.get(name, {}).get("concurrency") or 0) or BASE_CONC
    print(f"[rule] {name} -> {'ENABLE' if want else 'DISABLE'}: {why}", flush=True)
    _patch(name, **body)


def cn_tick(now: float) -> None:
    global _cn_last_poll, _cn_cleared_at, _cn_pool_hold
    global _cn_exhaust_w, _cn_exhaust_at
    if not CN_ENTRIES:
        return
    if now - _cn_last_poll < CN_INTERVAL:
        return
    _cn_last_poll = now

    # ① 每日 CN_RESET_HOUR（默认 00:00）清一次熄灭标识：hy3 每日额度刷新，从头再试。
    #    重启也等价清零（标识不落盘）。号池空是活条件，不归这里管。
    rp = _cn_reset_point(now)
    if _cn_cleared_at < rp:
        if _cn_hold:
            print(f"[cn] {CN_RESET_HOUR:02d}:00 daily clear: release {sorted(_cn_hold)}", flush=True)
        _cn_hold.clear()
        _cn_exhaust_w = 0
        _cn_exhaust_at = 0.0
        _cn_cleared_at = now

    snap = _cn_read_status()
    if snap is None:
        return   # 读失败沿用旧判定，绝不误动
    credits = snap["credits"]

    # ② 号池空（活条件）：账号级地板由面板挡聊天、攒分照跑、爬回自动回池，
    #    这里只判"池子空了没"——空了两个条目一起关，别拿请求去撞必然失败的上游。
    pool_empty = not [c for c in credits.values() if c >= CN_FLOOR]
    if pool_empty != _cn_pool_hold:
        _cn_pool_hold = pool_empty
        print(f"[cn] pool {'EMPTY' if pool_empty else 'OK'}: min={min(credits.values()):g} "
              f"floor={CN_FLOOR:g} => {'both CN entries off' if pool_empty else 'release'}",
              flush=True)

    # ③ hy3 熄灭信号（只关 hy3 条目；标识到点才清，摘了就没流量、计数会冻结）
    if CN_FREE_BACKEND and CN_FREE_BACKEND not in _cn_hold:
        if snap["hy3_paid"]:
            _cn_hold[CN_FREE_BACKEND] = "signal B: hy3 ledger cost_per_1k > 0"
            print(f"[cn] {_cn_hold[CN_FREE_BACKEND]}", flush=True)
        elif (_cn_exhaust_w >= CN_EXHAUST_FAILS
                and now - _cn_exhaust_at <= CN_INTERVAL * 2):
            _cn_hold[CN_FREE_BACKEND] = (f"signal A: hy3 {_cn_exhaust_w} "
                                         f"consecutive fail windows")
            print(f"[cn] {_cn_hold[CN_FREE_BACKEND]}", flush=True)

    # ④ 对齐期望 vs 网关上报（幂等）
    cfg = _configured_map()
    for name in CN_ENTRIES:
        why = _held_reason(name)
        _apply(name, why is None, why or "CN rules clear", cfg)


def _quota_log(msg: str) -> None:
    """同一条告警只打一次——判定每 60s 一轮，别把日志刷成瀑布。"""
    global _quota_note
    if msg == _quota_note:
        return
    _quota_note = msg
    print(f"[quota] {msg}", flush=True)


def quota_tick(now: float) -> None:
    global _quota_last_poll
    if now - _quota_last_poll < QUOTA_INTERVAL:
        return
    _quota_last_poll = now

    try:
        mtime = os.path.getmtime(QUOTA_FILE)
    except OSError as e:
        _quota_log(f"{QUOTA_FILE} unreadable: {e} => no action")
        return
    age = now - mtime
    if age > QUOTA_STALE_S:
        _quota_log(f"data {age:.0f}s stale (> {QUOTA_STALE_S:g}s) => no action")
        return
    try:
        with open(QUOTA_FILE, "r", encoding="utf-8") as f:
            doc = json.load(f)
    except Exception as e:  # noqa: BLE001
        _quota_log(f"parse failed: {e} => no action")
        return
    _quota_log("data ok")

    cfg = _configured_map()
    names = set(cfg) | set(QUOTA_MAP)
    for backend in sorted(names):
        item: object = None
        for path in _quota_candidates(backend):
            cur: object = doc
            for k in path:
                cur = cur.get(k) if isinstance(cur, dict) else None
            if isinstance(cur, dict) and cur.get("status") == "ok":
                item = cur
                break
        # 出错 / 缺凭证 / stale / 窗口缺失 → 不动作（沿用现状，绝不瞎猜）
        if not isinstance(item, dict) or item.get("status") != "ok":
            continue
        windows = item.get("windows")
        if not isinstance(windows, dict) or not windows:
            continue
        hits = []
        for lane in QUOTA_LANES:
            w = windows.get(lane)
            if not isinstance(w, dict):
                continue
            try:
                pct = float(w.get("percent", 0) or 0)
            except (TypeError, ValueError):
                continue
            if pct >= QUOTA_FULL_PCT:
                hits.append(f"{lane}={pct:g}%")
        if hits:
            why = f"quota window full: {','.join(hits)}"
            _quota_hold[backend] = why
            _apply(backend, False, why, cfg)
        else:
            _quota_hold.pop(backend, None)
            _apply(backend, True, "quota windows below threshold", cfg)


def tick() -> None:
    global _cn_exhaust_w, _cn_exhaust_at
    snap = _get_stats()
    if not snap:
        return
    cfg = _configured_map()
    obs: dict[str, bool] = {}   # 本次成功拉取重建；后端没载入就不在表里 = 规则不碰它
    for b in snap.get("backends", []):
        name = b["name"]
        attempts = int(b.get("attempts", 0))
        failed = int(b.get("failed", 0))
        prev_att, prev_fail = _prev.get(name, (0, 0))
        d_att = attempts - prev_att
        d_fail = failed - prev_fail
        _prev[name] = (attempts, failed)

        enabled = bool(b.get("enabled", True))
        cooling = float(b.get("cooldown_remain_s", 0) or 0) > 0
        tripped = bool(b.get("circuit_open", False))
        obs[name] = enabled

        # 失败类型也取**本窗口增量**。fail_breakdown 是开机以来的累计值，
        # 老代码拿它直接和增量 d_fail 相减是量纲混用（累计只涨不减 →
        # upstream_fail 恒为 0 → 失败率恒 0 → "失败率高就禁用"从未生效过）。
        breakdown = b.get("fail_breakdown") or {}
        prev_bd = _prev_bd.get(name, {})
        d_bd = {k: max(0, int(v) - int(prev_bd.get(k, 0))) for k, v in breakdown.items()}
        _prev_bd[name] = {k: int(v) for k, v in breakdown.items()}

        if d_att <= 0 or d_att < MIN_SAMPLE:
            continue

        # 只统计上游真问题：本窗口失败 - 下游取消(network) - skip类
        d_network = d_bd.get("network", 0)
        d_skip = sum(d_bd.get(k, 0) for k in
                     ("skip_capacity", "skip_disabled", "skip_cooldown", "skip_circuit"))
        upstream_fail = max(0, d_fail - d_network - d_skip)
        rate = upstream_fail / d_att if d_att > 0 else 0
        top = sorted(((k, v) for k, v in d_bd.items() if v > 0), key=lambda kv: -kv[1])[:2]
        tags = ",".join(f"{k}×{v}" for k, v in top)

        # 跟踪连续失败次数
        if upstream_fail > 0:
            _consecutive_fails[name] = _consecutive_fails.get(name, 0) + 1
        else:
            _consecutive_fails[name] = 0

        # CN 免费条目的耗尽信号 A：连续"有上游失败的统计窗口"计数（供 cn_tick）。
        # 样本不足的窗口不推进也不清零（低峰期计数冻结，靠 cn_tick 的
        # 2×CN_INTERVAL 新鲜度闸门防陈旧误触）。
        if name == CN_FREE_BACKEND and d_att >= MIN_SAMPLE:
            if upstream_fail > 0:
                _cn_exhaust_w += 1
                _cn_exhaust_at = time.time()
            else:
                _cn_exhaust_w = 0

        if rate > FAIL_HIGH:
            if b.get("is_fallback"):
                if enabled:
                    # 网关已在冷却/熔断说明它自己也判断该摘了；不重复下发
                    note = " (gateway already cooling/circuit)" if (cooling or tripped) else ""
                    print(f"[sidecar] {name} fail_rate={rate:.2f} upstream_fail={upstream_fail} [{tags}] => DISABLE{note}", flush=True)
                    if not (cooling or tripped):
                        _patch(name, enabled=False)
                # 自动降级/禁用：只对兜底模型生效，首发轮转不动
                if _consecutive_fails[name] >= AUTO_DEGRADE_THRESHOLD and _consecutive_fails[name] < AUTO_DISABLE_THRESHOLD:
                    if int(b.get("concurrency", 0)) != 1:  # 已经降过就别每 5s 重复下发
                        print(f"[sidecar] {name} consecutive_fails={_consecutive_fails[name]} >= {AUTO_DEGRADE_THRESHOLD} => DEGRADE", flush=True)
                        _degrade_backend(name)
                elif _consecutive_fails[name] >= AUTO_DISABLE_THRESHOLD and enabled:
                    print(f"[sidecar] {name} consecutive_fails={_consecutive_fails[name]} >= {AUTO_DISABLE_THRESHOLD} => DISABLE", flush=True)
                    _patch(name, enabled=False)
            else:
                # 首发：失败率只观测不动手（2026-09-24 堵掉的老口子——原代码这里会
                # 真的把首发 enabled=False，而首发不进流量 → 永远不会被 re-enable，
                # 等于永久摘除直到人工发现）。配额打满走网关指数冷却 + 下面的配额窗规则。
                print(f"[sidecar] {name} fail_rate={rate:.2f} upstream_fail={upstream_fail} [{tags}] => PRIMARY observe-only (gateway exponential cooldown)", flush=True)
        elif rate < FAIL_LOW:
            conc = int(b.get("concurrency", 0))
            want = int(cfg.get(name, {}).get("concurrency") or 0)
            # 规则摘的流（号池空 / hy3 熄灭 / 配额窗打满）不许这里复活——恢复归规则的
            # 各自判据管，两边打架会 摘-复活-再摘 打摆子。
            if (not enabled and not cooling and not tripped
                    and _held_reason(name) is None):
                print(f"[sidecar] {name} fail_rate={rate:.2f} => RE-ENABLE (conc={want or BASE_CONC})", flush=True)
                _patch(name, enabled=True, concurrency=(want or BASE_CONC))
                _consecutive_fails[name] = 0
            elif enabled and want and conc < want:
                # 降级恢复：失败率回到低位且运行期并发被降过 → 恢复成配置值
                print(f"[sidecar] {name} fail_rate={rate:.2f} conc={conc} < cfg={want} => RESTORE concurrency", flush=True)
                _patch(name, concurrency=want)
        elif tripped or cooling:
            print(f"[sidecar] {name} fail_rate={rate:.2f} circuit={tripped} cooldown={cooling}s; watching", flush=True)

    _observed.clear()
    _observed.update(obs)


def rule_ticks(now: float) -> None:
    """两条规则各跑一遍；每个 tick 独立 try，一条炸了不影响另一条。"""
    try:
        cn_tick(now)
    except Exception as e:  # noqa: BLE001
        print(f"[cn] tick error: {e}", flush=True)
    try:
        quota_tick(now)
    except Exception as e:  # noqa: BLE001
        print(f"[quota] tick error: {e}", flush=True)


def main() -> None:
    print(f"[sidecar] polling {URL}/api/stats every {INTERVAL}s (base_conc={BASE_CONC})", flush=True)
    print(f"[sidecar] auto-degrade: {AUTO_DEGRADE_THRESHOLD} fails, auto-disable: {AUTO_DISABLE_THRESHOLD} fails", flush=True)
    print(f"[sidecar] CN rules: entries={list(CN_ENTRIES)} floor={CN_FLOOR:g} "
          f"reset={CN_RESET_HOUR:02d}:00 interval={CN_INTERVAL:g}s "
          f"exhaust_fails={CN_EXHAUST_FAILS}", flush=True)
    qmap = ", ".join("%s:%s" % (k, ".".join(v)) for k, v in sorted(_quota_map_effective().items()))
    print(f"[sidecar] quota rules: file={QUOTA_FILE} interval={QUOTA_INTERVAL:g}s "
          f"stale>{QUOTA_STALE_S:g}s full>={QUOTA_FULL_PCT:g}% map={{{qmap}}}", flush=True)
    # 首帧顺序不能反：先 tick() 把 _observed 铺满（规则才下得了手），
    # 再 rule_ticks() 锁存 hold（tick() 的失败率恢复分支才会放手）。
    # 反过来 = 重启后第一帧会把"规则早就摘掉的条目"先复活一次、60s 内又被摘回来，
    # 恰好是用户不要的"闪开闪关 + 白撞一次 429"。
    try:
        tick()
    except Exception as e:  # noqa: BLE001
        print(f"[sidecar] tick error: {e}", flush=True)
    rule_ticks(time.time())
    while True:
        try:
            tick()
        except Exception as e:  # noqa: BLE001
            print(f"[sidecar] tick error: {e}", flush=True)
        rule_ticks(time.time())
        time.sleep(INTERVAL)


if __name__ == "__main__":
    main()
