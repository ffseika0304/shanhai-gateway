#!/usr/bin/env python3
"""seika_llm_router 配额探针（配额指挥舱数据源）

职责：
  - 定时查询 OpenCode Go 两 key 与火山方舟两个套餐的额度窗口
  - 原子写出 go/static/quota.json，网页看板直接读（网关对 static 目录做整目录文件服务，零重启生效）
  - 只读探针：不碰网关 admin、不碰路由、不调用任何业务接口

移植出处与声明（按用户要求保留原作者声明；本脚本不引入其任何运行时依赖，
接口逻辑按其文档化的实现自行改写为 Python）：
  1) dsh-quota-panel — https://github.com/wenzetan/dsh-quota-panel
     MIT License
     Copyright (c) 2026 dsh-quota-panel contributors
     移植内容：火山方舟 OpenAPI 签名（SigV4 变体：固定 SignedHeaders 顺序、
     裸 HMAC-SHA256 算法串、scope 以 /request 结尾、签名密钥链无 AWS4 前缀）、
     GetAFPUsage / GetCodingPlanUsage 响应解析、OpenCode usage 窗口归一。
     该实现自身注明：算法对照火山官方 openapi demo 与 cc-switch
     （farion1231/cc-switch, src-tauri/src/services/coding_plan.rs）交叉核对。
  2) token-monitor — https://github.com/Javis603/token-monitor
     MIT License
     Copyright (c) 2026 Javis
     参考内容：OpenCode Go /v1/usage 查询方式与三窗口（rolling/weekly/monthly）
     模型、火山 Coding/Agent 双套餐分别查询的口径。

env：
  QUOTA_OUT             输出文件，默认 <repo>/go/static/quota.json
  QUOTA_CONFIG          后端配置，默认 <repo>/config/backends.json（找 opencode key）
  QUOTA_INTERVAL        --loop 模式间隔秒，默认 180
  VOLC_ACCESS_KEY / VOLC_ACCESSKEY   火山引擎 AK（建议只读子账号：ArkReadOnlyAccess）
  VOLC_SECRET_KEY / VOLC_SECRETKEY    火山引擎 SK
  VOLC_REGION           默认 cn-beijing

用法：
  python3 scripts/quota_probe.py          # 跑一次就退出
  python3 scripts/quota_probe.py --loop   # 常驻循环（slr-quota.service 用这个）
"""
from __future__ import annotations

import datetime as dt
import hashlib
import hmac
import json
import os
import sys
import tempfile
import time
import urllib.error
import urllib.request

# ── 基础路径 ────────────────────────────────────────────────
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
CONFIG_PATH = (
    os.getenv("QUOTA_CONFIG")
    or os.getenv("ROUTER_BACKENDS_FILE")  # 与网关/sidecar 同一约定，避免读到两个配置
    or os.path.join(ROOT, "config", "backends.json")
)
OUT_PATH = os.getenv("QUOTA_OUT", os.path.join(ROOT, "go", "static", "quota.json"))
INTERVAL = float(os.getenv("QUOTA_INTERVAL", "180"))
UA = "SeikaRouter-QuotaProbe/1.0"

ATTRIBUTION = [
    "dsh-quota-panel (MIT) — Copyright (c) 2026 dsh-quota-panel contributors",
    "token-monitor (MIT) — Copyright (c) 2026 Javis",
    "volc signing cross-checked vs volcengine official openapi demos & farion1231/cc-switch",
]


def _now_iso() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds")


def _epoch_iso(v) -> str | None:
    """火山纪元值（毫秒或秒，-1 = 无重置）→ ISO；非法/非正 → None。"""
    try:
        n = float(v)
    except (TypeError, ValueError):
        return None
    if n <= 0:
        return None
    if n > 1e12:
        n /= 1000.0
    return dt.datetime.fromtimestamp(n, dt.timezone.utc).isoformat(timespec="seconds")


def _http(url: str, headers: dict, data: bytes | None = None, timeout: float = 10.0) -> tuple[int, bytes]:
    req = urllib.request.Request(url, data=data, headers=headers, method="POST" if data is not None else "GET")
    req.add_header("User-Agent", UA)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


# ── 火山方舟 OpenAPI（SigV4 变体，逐字对齐 dsh-quota-panel 实现） ──
VOLC_HOST = "open.volcengineapi.com"
VOLC_SERVICE = "ark"
VOLC_VERSION = "2024-01-01"
VOLC_CONTENT_TYPE = "application/json; charset=utf-8"
VOLC_SIGNED_HEADERS = "host;x-date;x-content-sha256;content-type"


def _volc_uri_encode(s: str) -> str:
    """RFC3986 仅保留未保留字符（用于规范化查询串）。"""
    out = []
    for b in s.encode("utf-8"):
        c = chr(b)
        if c.isascii() and (c.isalnum() or c in "-_.~"):
            out.append(c)
        else:
            out.append("%%%02X" % b)
    return "".join(out)


def _volc_canonical_query(action: str, region: str) -> str:
    pairs = sorted([("Action", action), ("Region", region), ("Version", VOLC_VERSION)])
    return "&".join(f"{_volc_uri_encode(k)}={_volc_uri_encode(v)}" for k, v in pairs)


def _volc_signed_headers(ak: str, sk: str, region: str, action: str, now: dt.datetime) -> dict:
    x_date = now.strftime("%Y%m%dT%H%M%SZ")
    short_date = x_date[:8]
    payload_hash = hashlib.sha256(b"").hexdigest()
    canonical_query = _volc_canonical_query(action, region)
    # 固定顺序（非字典序）——火山与标准 SigV4 的差异点一
    canonical_headers = (
        f"host:{VOLC_HOST}\n"
        f"x-date:{x_date}\n"
        f"x-content-sha256:{payload_hash}\n"
        f"content-type:{VOLC_CONTENT_TYPE}\n"
    )
    canonical_request = "\n".join(
        ["POST", "/", canonical_query, canonical_headers, VOLC_SIGNED_HEADERS, payload_hash]
    )
    # 裸 HMAC-SHA256、scope 以 /request 结尾、密钥链无 AWS4 前缀——差异点二
    scope = f"{short_date}/{region}/{VOLC_SERVICE}/request"
    string_to_sign = "\n".join(
        ["HMAC-SHA256", x_date, scope, hashlib.sha256(canonical_request.encode("utf-8")).hexdigest()]
    )

    def _hmac(key: bytes, data: str) -> bytes:
        return hmac.new(key, data.encode("utf-8"), hashlib.sha256).digest()

    k_date = _hmac(sk.encode("utf-8"), short_date)
    k_region = _hmac(k_date, region)
    k_service = _hmac(k_region, VOLC_SERVICE)
    k_signing = _hmac(k_service, "request")
    signature = hmac.new(k_signing, string_to_sign.encode("utf-8"), hashlib.sha256).hexdigest()
    return {
        "Host": VOLC_HOST,
        "X-Date": x_date,
        "X-Content-Sha256": payload_hash,
        "Content-Type": VOLC_CONTENT_TYPE,
        "Authorization": (
            f"HMAC-SHA256 Credential={ak}/{scope}, "
            f"SignedHeaders={VOLC_SIGNED_HEADERS}, Signature={signature}"
        ),
    }


def volc_call(action: str, ak: str, sk: str, region: str) -> dict:
    """POST 空体 + 签名查询串调用一个火山动作；业务错误在 200 包里，手动抛。"""
    query = _volc_canonical_query(action, region)
    url = f"https://{VOLC_HOST}/?{query}"
    headers = _volc_signed_headers(ak, sk, region, action, dt.datetime.now(dt.timezone.utc))
    status, raw = _http(url, headers, data=b"")
    try:
        body = json.loads(raw.decode("utf-8"))
    except Exception:  # noqa: BLE001
        raise RuntimeError(f"HTTP {status}: 非 JSON 响应") from None
    err = ((body.get("ResponseMetadata") or {}).get("Error")) or None
    if err:
        raise RuntimeError(f"volcengine {err.get('Code', 'Unknown')}: {err.get('Message', 'unknown error')}")
    if status != 200:
        raise RuntimeError(f"HTTP {status}")
    return body


def _parse_afp(result) -> dict:
    """GetAFPUsage：Result.AFPFiveHour/AFPWeekly/AFPMonthly 各 {Quota,Used,ResetTime}。
    AFPDaily 刻意跳过（官方控制台隐藏、非强制限额）；Quota<=0 视为未订阅该窗。"""
    windows = {}
    result = result or {}
    for key, lane in (("AFPFiveHour", "rolling"), ("AFPWeekly", "weekly"), ("AFPMonthly", "monthly")):
        w = result.get(key)
        if not isinstance(w, dict):
            continue
        try:
            quota = float(w.get("Quota"))
        except (TypeError, ValueError):
            continue
        if quota <= 0:
            continue
        try:
            used = float(w.get("Used"))
        except (TypeError, ValueError):
            used = 0.0
        pct = round(used / quota * 100)
        windows[lane] = {"percent": max(0, min(100, pct)), "resetsAt": _epoch_iso(w.get("ResetTime"))}
    return windows


def _parse_coding(result) -> dict:
    """GetCodingPlanUsage：Result.QuotaUsage[] 各 {Level,Percent,ResetTimestamp(秒,-1=无)}。
    仅百分比、无绝对计数；字段名对照实测响应（2026-06）。"""
    result = result or {}
    arr = result.get("QuotaUsage") or result.get("Usages") or result.get("Details")
    if not isinstance(arr, list):
        return {}
    lane_of = {
        "session": "rolling", "5h": "rolling", "fivehour": "rolling",
        "five_hour": "rolling", "rolling_5h": "rolling",
        "weekly": "weekly", "week": "weekly", "7d": "weekly",
        "monthly": "monthly", "month": "monthly",
    }
    windows = {}
    for item in arr:
        if not isinstance(item, dict):
            continue
        label = str(item.get("Level") or item.get("Type") or item.get("Period") or "").strip()
        lane = lane_of.get(label.lower())
        if not lane:
            continue
        try:
            percent = float(item.get("Percent", item.get("UsedPercent", item.get("UsagePercent"))))
        except (TypeError, ValueError):
            continue
        windows[lane] = {
            "percent": max(0, min(100, round(percent))),
            "resetsAt": _epoch_iso(item.get("ResetTimestamp", item.get("ResetTime"))),
        }
    return windows


def volc_probe(action: str, parse, ak: str, sk: str) -> dict:
    if not ak or not sk:
        return {
            "status": "need_credentials",
            "msg": "缺火山 AK/SK（只读子账号：ArkReadOnlyAccess）；账号条目加 quota_ak / quota_sk，"
                   "或 env VOLC_ACCESS_KEY / VOLC_SECRET_KEY",
            "windows": {},
        }
    region = os.getenv("VOLC_REGION", "cn-beijing")
    last_err = ""
    for i in range(2):  # 瞬时网络错误重试一次；业务错误（ResponseMetadata.Error）立即返回
        try:
            body = volc_call(action, ak, sk, region)
            windows = parse(body.get("Result"))
            if not windows:
                return {"status": "error", "msg": "未解析到额度窗口（可能未订阅该套餐）", "windows": {}}
            return {"status": "ok", "windows": windows}
        except Exception as e:  # noqa: BLE001
            last_err = str(e)
            if i == 0:
                time.sleep(3)
    return {"status": "error", "msg": last_err, "windows": {}}


# ── OpenCode Go /v1/usage（base_url + /usage，Bearer key） ─────────
def opencode_probe(base_url: str, key: str, attempts: int = 2) -> dict:
    """带瞬时错误重试（SSL 握手超时这类抖动常见，见 quota.log；HTTP 错误不重试）。"""
    url = base_url.rstrip("/") + "/usage"
    last_err = ""
    for i in range(attempts):
        try:
            status, raw = _http(url, {"Authorization": "Bearer " + key}, timeout=10)
            body = json.loads(raw.decode("utf-8"))
            if status != 200:
                return {"status": "error", "msg": f"HTTP {status}", "windows": {}}
            usage = body.get("usage") or {}
            windows = {}
            for lane in ("rolling", "weekly", "monthly"):
                w = usage.get(lane)
                if isinstance(w, dict) and isinstance(w.get("percent"), (int, float)):
                    windows[lane] = {"percent": int(w["percent"]), "resetsAt": w.get("resetsAt")}
            if not windows:
                return {"status": "error", "msg": "响应缺 usage 窗口", "windows": {}}
            return {"status": "ok", "windows": windows}
        except Exception as e:  # noqa: BLE001
            last_err = str(e)
            if i < attempts - 1:
                time.sleep(3)
    return {"status": "error", "msg": last_err, "windows": {}}


def opencode_accounts() -> list[tuple[str, str, str]]:
    """配置里所有 opencode 账号 → [(显示名, base_url, key)]；显示名去掉 opencode 前缀。"""
    try:
        with open(CONFIG_PATH, encoding="utf-8") as f:
            cfg = json.load(f)
    except Exception as e:  # noqa: BLE001
        print(f"[quota] 读配置失败 {CONFIG_PATH}: {e}", flush=True)
        return []
    out = []
    for acc in cfg.get("accounts", []):
        base = acc.get("base_url", "")
        if "opencode.ai" not in base:
            continue
        keys = acc.get("api_keys") or []
        if not keys:
            continue
        name = acc.get("name", "opencode")
        short = name[len("opencode"):] if name.startswith("opencode") else name
        out.append((short or name, base, keys[0]))
    return out


# ── 火山方舟：每个订阅（后端）一组窗 ────────────────────────────
# 一个订阅 = backends.json 里一个账号条目 + 一个后端条目：
#   accounts[].api_keys[]   → ark- 推理 key（网关调用用）
#   accounts[].quota_ak/sk  → 火山 AK/SK（配额查询用；缺则回落 env）
#   backends[].base_url     → /api/coding/v3 = Coding Plan；/api/plan/v3 = Agent Plan
# 组键 = **后端名**：sidecar 按后端名取窗，多订阅天然各看各的，不用映射表。
VOLC_PLANS = {
    "/api/coding/v3": ("GetCodingPlanUsage", "Coding Plan"),
    "/api/plan/v3": ("GetAFPUsage", "Agent Plan"),
}
_PARSERS = {"GetCodingPlanUsage": _parse_coding, "GetAFPUsage": _parse_afp}


def volc_backends() -> list[dict]:
    """配置里每个 volc 套餐后端 → {name,label,account,ak,sk,action}。"""
    try:
        with open(CONFIG_PATH, encoding="utf-8") as f:
            cfg = json.load(f)
    except Exception as e:  # noqa: BLE001
        print(f"[quota] 读配置失败 {CONFIG_PATH}: {e}", flush=True)
        return []
    accounts = {a.get("name"): a for a in cfg.get("accounts", [])}
    env_ak = os.getenv("VOLC_ACCESS_KEY") or os.getenv("VOLC_ACCESSKEY") or ""
    env_sk = os.getenv("VOLC_SECRET_KEY") or os.getenv("VOLC_SECRETKEY") or ""
    out = []
    for b in cfg.get("backends", []):
        base = b.get("base_url") or ""
        for path, (action, label) in VOLC_PLANS.items():
            if path not in base:
                continue
            acc = accounts.get(b.get("account") or "") or {}
            # 凭据只认账号自己的（quota_ak/quota_sk）；env 回落要账号显式声明 quota_env，
            # 否则新订阅漏填时会**借用别的账号的凭据**、把别人的窗当成自己的 →
            # sidecar 按错窗摘除。宁可 need_credentials（不动作）也不借。
            if acc.get("quota_ak") and acc.get("quota_sk"):
                ak, sk = acc["quota_ak"], acc["quota_sk"]
            elif acc.get("quota_env"):
                ak, sk = env_ak, env_sk
            else:
                ak = sk = ""
            out.append({
                "name": b.get("name") or "",
                "label": label,
                "account": b.get("account") or "",
                "ak": ak,
                "sk": sk,
                "action": action,
            })
            break
    return out


def _load_prev() -> dict:
    try:
        with open(OUT_PATH, encoding="utf-8") as f:
            p = json.load(f)
        return p if isinstance(p, dict) else {}
    except Exception:  # noqa: BLE001
        return {}


def _apply_stale(doc: dict, prev: dict) -> None:
    """查询失败时沿用上次成功读数，防卡片闪烁（token-monitor 同款语义：transient 保 last-good）。
    状态标 stale，msg 保留真实错误，staleAt = 上次成功数据的生成时刻。"""
    for group in ("opencode", "volc"):
        for name, item in (doc.get(group) or {}).items():
            pitem = (prev.get(group) or {}).get(name)
            if (
                item.get("status") in ("error", "need_credentials")  # 两类"失败"都可沿用：错误 + 手动跑漏载凭证
                and isinstance(pitem, dict)
                and pitem.get("status") in ("ok", "stale")
                and pitem.get("windows")
            ):
                item["windows"] = pitem["windows"]
                item["status"] = "stale"
                item["staleAt"] = pitem.get("staleAt") or prev.get("generated_at")
                item["msg"] = f'{item.get("msg", "")}（沿用上次成功读数）'


def probe_once() -> dict:
    doc: dict = {"generated_at": _now_iso(), "attribution": ATTRIBUTION}
    oc = {}
    for short, base, key in opencode_accounts():
        oc[short] = opencode_probe(base, key)
    if not oc:
        oc["_"] = {"status": "error", "msg": f"配置里没找到 opencode 账号（{CONFIG_PATH}）", "windows": {}}
    doc["opencode"] = oc
    volc = {}
    for spec in volc_backends():
        item = volc_probe(spec["action"], _PARSERS[spec["action"]], spec["ak"], spec["sk"])
        item["label"] = spec["label"]
        item["account"] = spec["account"]
        item["plan"] = "coding" if spec["action"] == "GetCodingPlanUsage" else "agent"
        volc[spec["name"]] = item
    # 旧格式别名（2026-10-20）：组键 coding/agent 保留，给**缓存的旧看板页**用
    # （旧页读 q.volc.coding / q.volc.agent；组键一换，那两张卡直接消失）。
    # 只增不改：老页面读别名、新页面读后端名，两边都活。带 _alias 标记，
    # 新看板据此跳过别名卡（否则同一份数据显示两遍）。
    for alias, plan in (("coding", "coding"), ("agent", "agent")):
        for name, item in volc.items():
            if item.get("plan") == plan:
                item2 = dict(item)
                item2["_alias"] = True
                item2["_alias_of"] = name
                volc[alias] = item2
                break
    doc["volc"] = volc
    _apply_stale(doc, _load_prev())
    return doc


def write_out(doc: dict) -> None:
    os.makedirs(os.path.dirname(OUT_PATH), exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(OUT_PATH), prefix=".quota-", suffix=".tmp")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(doc, f, ensure_ascii=False, indent=1)
        os.replace(tmp, OUT_PATH)
    except Exception:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def run_once() -> None:
    doc = probe_once()
    try:
        write_out(doc)
    except Exception as e:  # noqa: BLE001
        print(f"[quota] 写 {OUT_PATH} 失败: {e}", flush=True)
        return
    parts = []
    for group in ("opencode", "volc"):
        for name, item in doc.get(group, {}).items():
            st = item.get("status")
            w = item.get("windows") or {}
            pct = "/".join(f"{v.get('percent')}%" for v in w.values()) if st == "ok" else item.get("msg", st)
            parts.append(f"{group}.{name}={pct}")
    print(f"[quota] {_now_iso()} {' | '.join(parts)}", flush=True)


def main() -> None:
    loop = "--loop" in sys.argv
    if not loop:
        run_once()
        return
    print(f"[quota] loop mode, interval={INTERVAL}s, out={OUT_PATH}", flush=True)
    while True:
        run_once()
        time.sleep(INTERVAL)


if __name__ == "__main__":
    main()
