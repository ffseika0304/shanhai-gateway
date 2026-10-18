<h1 align="center">ShanHaiGateway · 山海关</h1>

<p align="center"><b>把多个上游模型供应商组织成一个 OpenAI 兼容端点</b><br>
SWRR 加权轮转 · 配额指数冷却 · 任务级亲和 · 影子留存观测 · Docker 三分钟上产线</p>

<p align="center">🥇 每一条路由语义都按线上真实事故校准 —— 排队、冷却、断连判定不是拍脑袋 🥇</p>

<p align="center">
  <a href="https://github.com/ffseika0304/shanhai-gateway"><img src="https://img.shields.io/github/stars/ffseika0304/shanhai-gateway?style=flat-square&logo=github" alt="GitHub Stars"></a>
  <a href="https://github.com/ffseika0304/shanhai-gateway/fork"><img src="https://img.shields.io/github/forks/ffseika0304/shanhai-gateway?style=flat-square&logo=github" alt="GitHub Forks"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue?style=flat-square" alt="License: MIT"></a>
  <a href="https://go.dev"><img src="https://img.shields.io/badge/go-%E2%89%A51.27-00ADD8?style=flat-square&logo=go" alt="Go ≥1.27"></a>
  <a href="#docker-部署"><img src="https://img.shields.io/badge/docker-镜像%2038.9MB-2496ED?style=flat-square&logo=docker" alt="Docker"></a>
</p>

> **两京锁钥无双地，万里长城第一关。**
>
> 山海关是明长城的东端起点，万里长城在这里一头扎进渤海。关不大，但关内关外所有商旅、
> 文书、军令都得从这座门过——守将手里有一张活地图：哪条道通向哪家、哪条道今天塌了、
> 哪条道设了卡、哪支商队该钉回哪条道走。
>
> 这个网关就是那道关：**关外是你的客户端，关内是各家上游，每个请求过闸，由它决定走哪条道。**

---

## 它解决什么问题

你接了三家模型上游做轮询，然后开始遇到这些事：

- **某家上游今天抽风，整个产品跟着 503。** 你需要一个比 `retry` 更聪明的失败处理——按错误类别分流：配额类指数退避，满载排队，网络类只观测不惩罚。
- **前缀缓存命中率永远上不去。** 轮询把同一对话的第二轮甩给另一家 provider——前缀缓存在 provider 侧，换家就凉。实测过：散装轮转下同前缀簇命中率 51~56%，恰好等于池内抽取概率，**轮询就是天花板**。
- **上游悄悄下架某个模型，后端 0 成功率空转了四天半才被发现。** 4xx 不一定是客户端的错，得有人替后端认领这类失败。
- **一次断连风暴打出 1720×503。** 客户端主动断开被记成后端失败，正常后端挨个进冷却，最后全网无后端可用。
- **key 撒得到处都是，仓库永远不敢见人。**

ShanHaiGateway 把这些问题一次关进关里。上面的每一条，都对应本文
[运维语义](#运维语义线上事故沉淀的规则表)里的一条规则——**每条规则都是一次真实事故换来的**，
不是论文里的理论模型。

## ✨ 特性

- 🚦 **SWRR 加权轮转 + 兜底链隔离**：权重/并发/超时逐后端可调，兜底后端与主力池隔离，不互相污染
- ❄️ **错误分类学驱动的冷却**：403/429 指数退避 5m→10m→…→4h 封顶；满载排队 60s 而不是甩 503；客户端断连不吃冷却
- 📌 **任务级会话亲和**：优先 `X-Job-Id`、退回 `X-Session-Key`，同一任务钉回同一后端——前缀缓存的复用单元接上了（实测命中率 27% → 51%，峰值 69%）
- 🧩 **quirk 怪癖层**：某家专属的报错体/用量键/出口定制写进独立包，内核零按名分支，没怪癖的上游零配置
- 👥 **多用户下游 key 计费**：`sk-user-*` 额度核发 + 可插拔登录（注册表模式）+ 公版用户中心
- 🔭 **影子留存观测**：SQLite 单库 attempts/captures（落库前自动脱敏），`/api/stats` 细粒度看板
- 🔥 **配置热加载**：`backends.json` 按 mtime 热加载，改权重/加后端不重启
- 🩺 **sidecar 慢周期自愈**：独立 Python 进程（零第三方依赖），按窗口增量失败率做限流/降级决策，与网关快路径分工
- 🐳 **Docker 一把梭**：多阶段构建、纯 Go SQLite（零 CGO）、非 root 运行、healthcheck、**镜像内零密钥**

## 🎯 两种人，两个入口

**你只想有个能用的网关** —— Docker 三条命令起一个 OpenAI 兼容端点，把 `base_url` 换掉就能跑。
权重、冷却、亲和全部开箱默认，不用读架构章节。

**你要把它钉进产线，或者魔改路由** —— 架构图、包职责表、八条运维规则、怪癖层接口、
captures 消费契约都在下面。每个魔法数字为什么是这个值，都有事故出处。

---

## 🚀 快速开始

### Docker（推荐）

```bash
git clone https://github.com/ffseika0304/shanhai-gateway.git && cd shanhai-gateway

# 1. 准备真实配置（进挂载卷，不进 git）
mkdir -p runtime/config runtime/data
cp config/backends.example.json runtime/config/backends.json   # 改成你的后端
cp config/keys.example.json    runtime/config/keys.json        # 改成你的下游 key

# 2. 起容器（宿主侧默认 :18016，SHG_PORT 可覆盖）
docker compose -f deploy/docker-compose.yml up -d --build

# 3. 冒烟
curl -s http://127.0.0.1:18016/health          # {"status":"ok","version":"0.5.0"}
curl -s http://127.0.0.1:18016/api/stats | head -c 400
```

### 裸跑二进制

要求 **Go ≥ 1.27**：

```bash
make build                          # 等价于 go build -o shg ./cmd/shg

cp config/backends.example.json config/backends.json   # 改字段，凭据全假也能先跑
export LISTEN=":18006"              # LAN 免账口
export ROUTER_VALIDATE=0            # 跳过启动外网自检（配真 key 时删掉）

./shg &
curl -s http://127.0.0.1:18006/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"demo-model","messages":[{"role":"user","content":"hi"}]}'
# 200 即通。健康检查 GET /health；流量看板 GET /api/stats
```

LAN 口 `POST /v1/chat/completions` 不强制 key（免账通道）；下游 key 计费只在公网口/用户中心启用。

### 两种形态，同一套路径语义

| | 裸跑 | Docker |
|---|---|---|
| 配置 | `<repo>/config/`（真实文件 gitignored） | `./runtime/config/` → 挂载 `/etc/shg` |
| 数据/日志 | `~/.local/share/shg/` | `./runtime/data/` → 挂载 `/var/lib/shg` |
| 身份 | `shg/0.5.0`（UA / `/healthz` / 日志前缀） | 同左 |
| 配置热加载 | mtime 生效，无需重启 | mtime 生效，无需重启容器 |

---

## 架构

依赖方向单向：`api → service → core`；`config` / `runtime` 是横向投喂层。

```
        ┌─────────────────────────────────────────────────────┐
        │              cmd/shg —— 只装配                       │
        │     env → 路径 → 目录 → SQLite → 两个 listener        │
        └───────────────────────────┬─────────────────────────┘
                                    ▼
┌─────────────────────────────────────────────────────────────────┐
│ internal/api   控制面 HTTP 门:LAN(免账) / public(dashGate+key)     │
│   usercenter / admin / dashboard / dashgate / public 各门        │
└──────────────┬──────────────────────────────┬───────────────────┘
               ▼                              │
┌─────────────────────────────────────────────┼───────────────────┐
│ internal/service      数据面 + 控制面业务    │                   │
│   routing(加权轮转/兜底)  proxy(代理主流程)   │                   │
│   auth(可插拔登录)  apikey(下游 key 计费)     │                   │
└──────────────┬──────────────────────────────┼───────────────────┘
               ▼                              │
┌─────────────────────────────────────────────┼───────────────────┐
│ internal/core       errclass(返回码分类学)     │                   │
│          upstream(容量/健康状态机) store(SQLite)│                   │
└──────────────┬───────────────────────────────┼───────────────────┘
               │   横向投喂(这两层互不依赖、也不依赖 service)│
┌──────────────┼───────────────────────────┬───┴───────────────┐
│ internal/config  backends.json 的 加载/校验/mtime 热加载/自检黑名单 │
│ internal/runtime 身份/路径/env/日志轮转/清扫/SQLite 保养            │
└─────────────────────────────────────────────────────────────────┘
```

每个包为什么独立划出来——不同的维护者会在不同的时间、因为不同的理由动它：

| 包 | 一句话职责 | 为什么必须独立演进 |
|------|--------|-----------|
| `api` | 门和接线的形状:LAN 免账定调、公网登录门、看板数据拼装 | 『谁能进』与『怎么路由一个请求』无关;换入口/加端口/改前端只动这里 |
| `service/routing` | 选谁:加权轮转、候选筛选、兜底隔离、原子换装 | 路由算法独立换代(换 P2C、最少连接),core 和配置格式不该跟着动 |
| `service/proxy` | 发出去:请求改写、流式转发、排队超时、断连判定 | 数据面协议细节单独演进,路由决策层不该感知 |
| `service/auth` | 谁能进:可插拔登录方式(注册表模式) | 加一种登录方式 = 新增一个 provider 文件,其它层零改动 |
| `service/apikey` | 计费边界:`sk-user-*` 下游 key 的额度核发 | 计费策略的增减只改这一层 |
| `core/errclass` | 上游返回错误的状态分类(唯一真源)+ 全局默认词表 | 分类的增减同时牵动冷却/熔断/统计/兜底四方,必须一处定义 |
| `core/quirk` | 上游怪癖:只在某家出现的问题(出口定制/专属报错体/专属用量键) | 按家差异独立演进;内核只认接口、零按名分支,默认实现=通用行为 |
| `core/upstream` | 后端本体:并发位、熔断、指数冷却、每类失败计数 | 每个魔法数字都是生产事故调出来的;改动值得独立评审 |
| `core/store` | SQLite 单库:attempts、users、sessions,单写入口 | schema 演进与业务逻辑互不拖拽;单写入口是并发安全的地基 |
| `config` | 定配置:什么算合法后端、判死的进黑名单 | 配置格式变化不该逼着 service 层跟着改;黑名单策略单独评审 |
| `runtime` | 程序身份证与落盘位置:路径/env/轮转/保养 | 换名/搬目录/发版本,改这一个包全局生效 |
| `cmd/shg` | 只装配不做业务 | 永远薄一层:业务下沉,这里只写启动与关停顺序 |

---

## 配置

`backends.json` 最省版（凭据全假，字段全集见 `config/backends.example.json`）：

```json
{
  "accounts": [
    { "name": "vendor-a", "base_url": "https://api.example-llm.com/v1",
      "api_keys": ["sk-xxxxx"] }
  ],
  "backends": [
    {
      "name": "vendor-a-main",
      "account": "vendor-a",
      "model": "demo-model",
      "base_url": "https://api.example-llm.com/v1",
      "weight": 1,
      "concurrency": 8,
      "enabled": true,
      "is_primary": true
    }
  ]
}
```

### 环境变量

| 环境变量 | 覆盖的路径/行为 | 默认 |
|-------------|----------|--------|
| `GATEWAY_DATA_DIR` | 数据根 | `~/.local/share/shg` |
| `GATEWAY_CONFIG_DIR` | 配置目录 | `<repo>/config` |
| `GATEWAY_DB_FILE` | SQLite 主库 | `<dataDir>/shg.db` |
| `GATEWAY_BACKENDS_FILE` | 后端基线文件 | `<configDir>/backends.json` |
| `GATEWAY_KEYS_FILE` | 下游 key 总账 | `<configDir>/keys.json` |
| `GATEWAY_QUOTA_FILE` | 配额快照 | `<dataDir>/quota.json` |
| `ROUTER_BACKENDS` | 直接注入 JSON 配置(优先级高于文件) | — |
| `ROUTER_VALIDATE` | `0` 时跳过启动自检 | `1` |
| `LISTEN` / `LISTEN_PUBLIC` | LAN 免账口 / 公网口 | `:18006` / 不监听 |
| `SESSION_AFFINITY` | `off` 关闭会话亲和(行为与未接亲和逐位一致);`AFFINITY_TTL/AFFINITY_MAX/AFFINITY_CONTENTION_LIMIT` 分别是条目生存期 / LRU 容量 / 满载滞回迁移阈值 | `on` / `6h` / `8192` / `3` |
| `REASONING_EFFORT` | 思考模型统一压低推理预算:客户端**没显式指定** `reasoning_effort` 时注入默认值(显式指定则尊重、不覆盖);空串 = 不注入。前置链路(另一层网关/路由器)已注入时,守卫命中"已有"会跳过,串联不双注入 | `low` |

> env 前缀是 `GATEWAY_*` 而不是 `SHG_*`:sidecar 契约(`py/sidecar.py` 读
> `GATEWAY_BACKENDS_FILE` / `GATEWAY_QUOTA_FILE`)锚在这组名字上,改名收益为零、
> 迁移风险为正。身份在 `runtime/identity.go` 的 `AppName`(`"shg"`),不在 env 前缀。

`backends.blocked.json` 没有独立 env:它与 `backends.json` 绑在同一目录,必须一起搬走。

### 密钥与数据出库

**仓库与镜像内零密钥**——这是自托管工具的基本尊严,也是将来开源的硬前提:

- 真实 `backends.json` / `keys.json` / `backends.blocked.json` / `quota.env` 全部 **gitignored**,
  只存在于运行时位置(裸跑:`config/`;docker:挂载卷 `runtime/config/`)。仓库里只有 `*.example.json` 模板。
- 数据(`*.db` / logs)**永不入库**:captures 含请求原文,属在产数据不是配置。
  灾备语义 = 代码 + 运行时配置目录即可复活。
- 仓库里任何文件出现真 key 都是 bug,发现即 REDACT。

---

## 运维语义：线上事故沉淀的规则表

> **沿革说明**:表中 SLR 指本项目早期自研的前置路由器(seika_llm_router),
> 已被本网关整体替代;相关条目作为语义沿革保留。

线上事故沉淀成规则表——改动任何一条之前先读这里的"为什么":

| # | 规则 | 事故来源与理由 |
|----|------|-----------|
| 1 | **客户端断连不冷却** | 一次断连风暴打出 1720×503(10-03):客户端断开被记进后端失败 → 正常后端挨个进冷却 → 全网无后端可用。断连只能归为 `network` 类观测,不吃冷却、不踩熔断 |
| 2 | **模型不存在是后端的锅** | 上游悄悄下架 model 时网关表现为 4xx/黑洞(10-06 起某兜底后端 0 成功率空转 4 天半才被发现)。这类 4xx 不能归给客户端;要落在对应后端头上走冷却/兜底摘除,身份走 `model_expire` / `model_fallback` 打补丁 |
| 3 | **配额类走指数退避,满载走排队** | 配额/权限类(403/429):指数冷却 5m→10m→…→4h 封顶,不反复白撞;**满载**不降级——排队到 60s(`QUEUE_WAIT`),而不是直接扔 503 |
| 4 | **黑名单只写不删,须人工清理** | `backends.blocked.json` 是启动自检判定的"永久失效"清单。没有自动解除路径:自动复白会用失败率当证据,而黑名单的判断依据是探测事实;摘条目必须人工复核后从文件里删 |
| 5 | **strike 只在成功时清零、勿乱改** | 10-01 的 403 自锁风暴:配额没恢复就把 strike 清零 → 后端带着空鼓回了池子 → 立刻再 403 → 冷却重启 → 循环打。指数退避的整条链依赖"连续击中数只靠成功归零",admin 热改不提供 strike 入口(设计如此,不是缺失) |
| 6 | **会话亲和=轮转外一层"优先选择"** | 10-10 生产实测前缀缓存命中率 0%:SWRR 把同一对话轮流甩给不同 provider——前缀缓存在 provider 侧,第二轮落到别家缓存就不在同一地。已建立过粘性的会话后续请求**优先钉回**同一 backend;新会话首落仍由 SWRR 权重控制。**只有最终成功且内容已交付才写表**(防失败会话钉死在坏后端);逐出/保持判定与冷却/熔断同源(`errclass.ReleaseAffinity`),不发明第二层健康层。**亲和键优先 `X-Job-Id`(任务级,同一任务的所有轮次共用一个 id,正是前缀缓存的复用单元),退回 `X-Session-Key`** |
| 7 | **按家怪癖进 quirk 层,内核零名字分支** | 内核曾 0 处按 provider 名分支——因为踩过的坑都被**泛化**成了通用规则(无条件删 Content-Length、只改 model 一个键、`session_header` 字段、通用词表),这四条泛化得对。但下一个坑如果不具备可泛化性,揉进通用规则会伤别家、加 `if name==xxx` 会把名字判定撒进内核:两条都是错路。第三条路是把「只在某家出现的问题」写进 `internal/core/quirk/xxx.go`,内核只在三个固定缝上查表(见下节) |
| 8 | **观测契约对齐 SLR 同形** | ① `attempts` 表含 `job_id` 列(对已存在库幂等 ALTER 迁移)——按 job 归因/缓存分析的脚本迁移后原样可用;② `/api/stats` 提供 SLR 同形 `affinity{hits,misses,puts,pinned_jobs}` 对象——`pinned_jobs` 取裸表长(Len 先于 CountPerBackend,后者顺带清 TTL,顺序反了口径就偏);`sticky_*` 三键是历史键名,看板还读着,保留。**口径注意**:`puts` 计每次真实 Put(含同 backend 刷新),与 SLR 逐位一致 |

### 怪癖层（`internal/core/quirk`）：按家差异的落点

内核只在请求生命周期的**三个固定缝**上调它，接口只有三个方法：

| 缝 | 方法 | 时机与职责 | 默认实现的行为 |
|----|------|-----------|----------------|
| ① | `Decorate(req, backend)` | 出口前：通用 header 清理（删 Content-Length / 客户端凭据 / Accept-Encoding、固定 Content-Type / UA）**之后**、发往上游之前。可加该家专属的头/query、按条件裁 body | no-op |
| ② | `Classify(status, body) (Class, bool)` | 分类时：`status+body → errclass.Class`。返回 `(Class, false)` = **不归我管**，调用方走通用判定 —— 让一家只认领自己那几句文案，不抢通用词表 | 只认领 400/404 + 通用"模型下架"词表（词表仍住 `errclass`） |
| ③ | `ParseUsage(body) Usage` | 用量时：`body → Usage`（prompt/completion/total/缓存命中）。`OK=false` = 走 OpenAI 默认 | OpenAI usage + `prompt_tokens_details.cached_tokens` |

**默认 = 通用行为，没怪癖的上游零配置**：`quirk.Get("")`（后端没配 `driver`）
和未注册的 driver 名都回落到包级 `OpenAI` 单例，行为与不接怪癖层逐位一致。
注册表是按名查表 + `RWMutex`，内核零按名分支 —— 怪癖是**数据（配置字段）**，不是代码分支。

**怎么加一个新怪癖**（内核零改动）：

1. 新建 `internal/core/quirk/xxx.go`：`type Xxx struct{ OpenAI }`（嵌基准实现），只覆盖有差异的方法；
2. 文件里 `func init() { Register(Xxx{}) }`；重复注册同名会 panic（防两家抢一个名字）；
3. `backends.json` 里那条后端加 `"driver": "xxx"`（这是 **config 字段，不是 env**；
   不填 = 通用行为，写错名字也只是退化成通用行为、不会让网关起不来）。

举三个真实形态的例子：某家只回中文欠费文案（缝② 认领那几句 → 拉黑它的 403）；
某家的"模型下架"报错体是自造词（缝② 认领 → 计入 fail_streak、连续失败自动熔断）；
某家的缓存用量键叫 `prompt_cache_hit_tokens` 而不是 `cached_tokens`（缝③ 解析 →
缓存价计费不掉成 0）。**默认发行版不实现任何具体怪癖**——纯接口预留，落点先立住。

### captures 消费契约（消费方先读这段，别从 Go 源码猜语义）

`captures` 是留存快照（旁路，与 `attempts` 用 rid 关联）。消费方判定一条留存**只用下面四条**，
这四条是从真流量数据里逼出来的契约：

| # | 判据 | 正确用法 | 反例（别这么判） |
|----|------|----------|------------------|
| 1 | **非完整交付的正信号是 `usable=false`** | 完整=可用（`usable=1`）；中途断连/被截断/未交付=不可用（`usable=0`）。断连/失败筛选用它 | ~~`partial_ratio`~~、~~`truncated`~~、~~status~~ 都不是"非完整"的正信号 |
| 2 | **`partial_ratio` 只回答"有没有内容"，不承担严重性分级** | `0`=无内容；`>0`=有内容。注意 `partial_ratio=1.0` 是**完整交付的正常形态**（不是"全 partial"） | 按 `partial_ratio` 分档推断严重程度是错的——它没有 P50 基线，将来也没有 |
| 3 | **`truncated=true` 是截断，不是失败** | 截断可能出现在完整交付里（正文本就巨大超 tee 上限）。截断≠丢弃，不可用性但仍按 #1 走 `usable` | 把 `truncated` 当失败/丢弃计数 |
| 4 | **同 `rid` 多条按 `ts` 排序，最后一条 `usable=true` 为最终交付** | 重试/partial 会让同一 rid 出现多条，这不是重复数据 | 看到多条就报"留存层写重了" |

一句话记忆：**可用性看 `usable`，内容有无看 `partial_ratio`，截断看 `truncated`，去重看 #4 的排序规则**——`status` 只在有实义 HTTP 码时参考（partial 中途断是 200，别用 status 当完成判据）。

---

## 周边服务：wb-panel / wb2api（WorkBuddy 系）

dashboard 导航里的「WorkBuddy Panel」是独立项目，不属于本仓库：

- **是什么**：WorkBuddy2API Panel——把腾讯 CodeBuddy 账号包装成 OpenAI 兼容 API 的多账号网关 + Web 运维面板（账号池/熔断/会话粘性/保活）
- **原仓**：<https://github.com/linguo2625469/workbuddy2api-panel>（fork 自 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api)，增强分支）
- **接入方式**：管理面板部署机 `:18011`，公网入口 `http://<VPS>:18011/panel/`（**真实 IP 不入库**；dashboard 导航默认指 RFC5737 占位，部署者在自己浏览器 console 执行 `localStorage.setItem("wbPanelUrl","http://<真实地址>:18011/panel/")` 一次即恢复）；API 供源 `http://127.0.0.1:7863/v1`（进程 wb2api）——配置里 `account=workbuddy` 的后端全部走这里
- **运维纪律**：wb-panel / wb2api **不随本网关变更重启**，两边升级互不牵连

## 🛠️ 故障排查

### 网关起不来，报"没有后端配置"

env 指错目录。`GATEWAY_CONFIG_DIR` 必须包含 `backends.json`；docker 场景检查挂载卷
`runtime/config/` 是否真的有文件（`docker logs shg` 看启动段解析了几个后端）。

### 某个后端一直不被选中

`GET /api/stats` 看三个字段：`enabled`（配置里被关）、`cooldown_remain_s`（配额类冷却中，
指数退避最长 4h）、`circuit_open`。**注意 `circuit_open:false` ≠ 没冷却**——冷却看
`cooldown_remain_s`，两者是两套计数。

### 全部后端都在冷却，请求全挂

这正是规则 #1 要防的形态。检查 `fail_breakdown`：如果大量 `network` 类失败，
多半是客户端断连风暴误伤（老版本语义），确认你在跑本仓库当前版本；如果是 401 集中爆发，
是 key 失效，换 key 前冷却不会自己好。

### 看板/统计有数据，想看原始请求

查 SQLite `captures` 表（先读上面的 captures 消费契约）。落库前已经过 scrub 脱敏
（密钥/内网路径/手机号/Bearer 头 → `[mask]`），这是特性不是缺陷。

### docker 容器反复重启

`docker logs shg` 看退出原因；大概率是 `runtime/config/` 缺文件或 JSON 不合法。
healthcheck 连续失败也会让编排层标记 unhealthy，但 `restart: unless-stopped` 只看进程退出。

## 📁 项目结构

```
shanhai-gateway/
├── cmd/shg/                   # main 装配 + 优雅关闭（全项目唯一入口）
├── internal/
│   ├── api/                   # 控制面 HTTP 门与看板
│   ├── service/
│   │   ├── routing/           # 加权轮转 / 候选 / 兜底 / 原子换装
│   │   ├── proxy/             # 数据面代理主流程
│   │   ├── affinity/          # 任务/会话亲和表
│   │   ├── auth/              # 登录抽象 + providers/
│   │   └── apikey/            # 下游 key 计费
│   ├── core/
│   │   ├── errclass/          # 错误分类唯一真源 + 词表
│   │   ├── quirk/             # 上游怪癖层（接口预留 + OpenAI 基准）
│   │   ├── upstream/          # 后端容量/健康状态机
│   │   └── store/             # SQLite 单库 + 单写入口
│   ├── config/                # 配置加载 / 校验 / 黑名单 / 热加载
│   └── runtime/               # 身份 / 路径 / 轮转 / 清扫 / 保养
├── web/dashboard, web/console # 运维看板 / 用户中心（go:embed 进二进制）
├── py/sidecar.py              # 配额守护（独立 Python 进程，纯标准库）
├── config/                    # 只有 *.example.json 模板；真实配置 gitignored
├── deploy/                    # Dockerfile + docker-compose.yml
├── API_CONTRACT.md            # api.seika.ltd 接口契约
├── Makefile                   # build / test / fmt / vet / docker
└── README.md
```

## 本地开发

```bash
gofmt -l internal/ cmd/ web/          # 必须为空
go vet ./...
go test ./... -race -count=1          # 基线：12 个包全绿
make build
```

不撞真上游的开发方法：全部走**假凭据** `sk-xxxxx`。请求会在上游网络错误/401 处失败，
正好顺序踩到错误分类、指数冷却、兜底摘除这些真实路径；单元测试已把路由/熔断/统计逐条
表驱动钉死。要跑不外发流量的 e2e，用 `ROUTER_BACKENDS` 直接注入一份指向
loopback 测试 HTTP 服务的假上游配置，配合 `ROUTER_VALIDATE=0`。

sidecar 依赖说明：见 `py/requirements.txt`——只有 Python 标准库，无 pip 安装项。

## 🔒 安全与责任

ShanHaiGateway 是**自托管工具**，怎么部署、怎么配凭据，决定权完全在你：

- **仓库与镜像零密钥**：源码、模板、Dockerfile 中不写死任何 key；真实凭据只存在
  你自己的挂载卷/本地目录，不在 git 历史里（本仓库历史就是全新起算的）
- **captures 自动脱敏**：请求/响应原文落库前经有序规则表脱敏（密钥、内网路径、手机号、
  认证头 → `[mask]`）；但留存本质上是敏感数据，**数据库文件的保管责任在你**
- 公网口请启用登录门 + 下游 key 计费，LAN 免账口只应对可信网段；要不要上 HTTPS、
  要不要限 IP，取决于你的部署面

> **免责声明**：本项目按"现状（as-is）"提供。作者不对因使用、误配置、未加固、
> 上游凭据泄露、数据丢失等任何直接或间接后果承担责任。部署即代表你已理解并自行承担相关风险。

## 🤝 贡献

欢迎提 [Issue](https://github.com/ffseika0304/shanhai-gateway/issues) 和 Pull Request。
改路由语义/冷却参数之前，请先读[运维语义](#运维语义线上事故沉淀的规则表)——
每条规则背后都有一次真实事故，别让下一代流量把学费再交一遍。

## 📄 License

[MIT](LICENSE)，随便改，随便用。

## 🙏 致谢

- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) - 纯 Go SQLite，让镜像零 CGO
- [rs/cors](https://github.com/rs/cors) - CORS 中间件
- [workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 及其增强分支 - WorkBuddy 账号池生态（见周边服务）

---

**如果这个项目对你有帮助，给个 Star 支持一下！** ⭐

**多上游、可观测、事故校准——你的流量，值得一道像样的关。** 🏯
