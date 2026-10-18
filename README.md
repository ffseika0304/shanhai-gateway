# ShanHaiGateway · 山海关

> **两京锁钥无双地,万里长城第一关。**
>
> 山海关是明长城的东端起点,万里长城在这里一头扎进渤海(老龙头),号称天下第一关。
> 关不大,但关内关外所有商旅、文书、军令都得从这座门过;守将手里有一张活地图——
> 哪条道通向哪家、哪条道今天塌了、哪条道设了卡、哪支商队该钉回哪条道走。
>
> 这个网关就是那道关:**每个请求过闸,由它决定走哪条上游道**。
> 关名缩写 **shg**,即二进制/命令行名。

## 一、这是什么

把多个上游模型供应商组织成**一个** OpenAI 兼容端点的多后端 LLM 网关:加权轮转(SWRR)、
熔断与配额类指数冷却、兜底链隔离、`backends.json` 按文件 mtime 热加载;另带控制面——
可插拔登录、多用户下游 key 计费、运维看板。设计目标是直接承载**生产流量**:排队、
冷却、断连判定这些语义都是按线上真实事故校准过的。

旁边挂一个独立的配额守护进程 `py/sidecar.py`(Python 标准库实现,**零第三方依赖**):
每 ~5s 轮询网关 `/api/stats`,按**窗口增量**失败率做慢周期的限流/降级自愈;另读 WB
面板 `/status` 的余额做 CN 账号地板、读配额快照 `quota.json` 做配额窗摘除。
分工:快路径(429/403/5xx/慢流/断连的分级处置 + 配额指数冷却)归 Go 网关,
慢周期决策归 sidecar。

## 二、架构

依赖方向单向:`api → service → core`;`config` / `runtime` 是横向投喂层。

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

每个包为什么要独立划出来——不同的维护者会在不同的时间、因为不同的理由动它:

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

## 三、快速开始

要求 **Go ≥ 1.27**。

```bash
# 构建
make build          # 等价于 go build -o shg ./cmd/shg
```

准备配置(仓库里只有模板,**真实密钥不进仓库**,见「密钥与数据出库语义」):

```bash
cp config/backends.example.json config/backends.json   # 按字段改:凭据全假即可先跑
cp config/keys.example.json    config/keys.json        # 下游 key 总账(sk-user-*)
```

`backends.json` 最省版(凭据全假):

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

最少 env(一个不设也行——LAN 默认 `:18006`,路径走默认):

```bash
export LISTEN=":18006"           # LAN 免账口
export ROUTER_VALIDATE=0         # 跳过启动外网自检(配真 key 时再删掉)
```

启动 + 冒烟:

```bash
./shg &
sleep 1
curl -s http://127.0.0.1:18006/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"demo-model","messages":[{"role":"user","content":"hi"}]}'
# 200 即通。健康检查 GET /health;细粒度流量看板 GET /api/stats
```

LAN 口 `POST /v1/chat/completions` 不强制 key(免账通道);下游 key 计费只有
公网口/用户中心启用。

## 四、运行时布局

数据根:`~/.local/share/shg/`(XDG 规范;改名只改 `runtime/identity.go` 一处,
`AppName = "shg"`)。身份标识 `shg/0.5.0` 走遍 User-Agent、`/healthz` 与日志前缀。

```
~/.local/share/shg/
├── shg.db                # SQLite:attempts 明细 / users / sessions / captures(留存)
│                         #   captures = 输入输出留存快照(旁路,与 attempts 用 rid 关联;
│                         #   model 记 effModel 同口径;ts=请求开始时刻;partial_ratio/
│                         #   usable/truncated 默认值写死兜住完整语义;usage_source 区分
│                         #   upstream/unavailable 避免把 0 当真实用量;见 store.go schemas)
│                         #   A5:req_body/resp_body 落库前经 proxy's scrubCapture 脱敏
│                         #   (有序规则表:具体密钥/内部路径/私有IPv4/手机号/URL query 凭据/
│                         #   Bearer|Authorization|X-Api-Key|Cookie 头/通用密钥前缀 → [mask]),
│                         #   见 proxy/capture.go scrubRules。A6 消费规则也在那里:
│                         #   跨系统关联键=(job_id,时间窗)(SLR 透传 X-Job-Id);同 rid 多条
│                         #   captures 按 ts 排序、usable=true 最后一条为最终交付)
├── quota.json            # 配额采集快照(外部采集脚本写,网关只读)
└── logs/
    ├── shg.log           # 自动轮转的应用日志
    └── attempts.jsonl    # 每条 attempt 一行,与 DB 同源同形

<repo>/config/            # 仓库里只有模板:
├── backends.example.json #   后端基线模板(复制为 backends.json 后改)
└── keys.example.json     #   下游 key 总账模板(复制为 keys.json 后改)
```

### 密钥与数据出库语义(shg 起,替代旧 shadow-config 入库)

**仓库与镜像内零密钥**,这是将来开源/公开的硬前提:

- 真实 `backends.json` / `keys.json` / `backends.blocked.json` / `quota.env` 全部
  **gitignored**,只存在于运行时位置——裸进程部署放 `<repo>/config/`(模板同目录改名),
  docker 部署放挂载卷 `./runtime/config/`(见 README 八)。
- key 的**源头账本**仍是 `~/keys/keys.md`;仓库里任何文件出现真 key 都是 bug,发现即 REDACT。
- 数据(`*.db` / logs)**永不入库**:`shg.db` 的 captures 含请求原文,属在产数据不是配置。
  灾备语义 = 代码 + 运行时配置目录即可复活;数据本身另有留存策略(captures 的 A6-A8)。

### 周边服务:wb-panel / wb2api(WorkBuddy 系)

dashboard 导航链的「WorkBuddy Panel(公网)」是独立项目,不属于本仓库:

- **是什么**:WorkBuddy2API Panel——把腾讯 CodeBuddy 账号包装成 OpenAI 兼容 API 的多账号网关 + Web 运维面板(账号池/熔断/会话粘性/保活)
- **原仓**:<https://github.com/linguo2625469/workbuddy2api-panel>(fork 自 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api),增强分支,源码在本机 `~/scf-dev/seika_llm_router/workbuddy2api-panel/`)
- **接入方式**:
  - 管理面板:本机 `:18011`(0.0.0.0),公网入口 `http://<阿里云VPS>:18011/panel/`(**真实 IP 不入库**;dashboard 导航默认指 RFC5737 占位,自己的浏览器 console 执行 `localStorage.setItem("wbPanelUrl","http://<真实地址>:18011/panel/")` 一次即恢复)
  - API 供源:`http://127.0.0.1:7863/v1`(进程 wb2api)——本网关配置里 `account=workbuddy` 的 `wb_cn_*` / `wb_global_*` 后端全部走这里,账号 key 在 workbuddy 账号段
- **运维纪律**:`wb-panel.service` / wb2api **不随本网关变更重启**,两边升级互不牵连

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
| `SESSION_AFFINITY` | `off` 关闭会话亲和(Options.Affinity 保持 nil,行为与未接亲和逐位一致);`AFFINITY_TTL/AFFINITY_MAX/AFFINITY_CONTENTION_LIMIT` 分别是条目生存期 / LRU 容量 / 满载滞回迁移阈值 | `on` / `6h` / `8192` / `3` |
| `REASONING_EFFORT` | 思考模型统一压低推理预算:客户端**没显式指定** `reasoning_effort` 时注入该默认值(显式指定则尊重、不覆盖);空串 = 不注入。与 SLR(seika_llm_router main.go)同源,SLR 已注入时网关守卫命中"已有"会跳过,串联不双注入 | `low` |

> **为什么 env 前缀还是 `GATEWAY_*` 而不是 `SHG_*`**:sidecar 契约(`py/sidecar.py`
> 读 `GATEWAY_BACKENDS_FILE` / `GATEWAY_QUOTA_FILE`)与既有部署配方都锚在这组名字上,
> 改名收益为零、迁移风险为正。身份在 `identity.go` 的 `AppName`(shg),不在 env 前缀。

`backends.blocked.json` 没有独立 env:它与 `backends.json` 绑在同一目录,必须一起搬走。

sidecar 与网关共用一组文件:`SIDECAR_URL` 指网关(默认同网关 LAN 口);
`ROUTER_BACKENDS_FILE` / `GATEWAY_BACKENDS_FILE` 指定后端文件;
`SIDECAR_QUOTA_FILE` / `GATEWAY_QUOTA_FILE` 指定配额快照——sidecar 与网关
`/quota.json` 必须读同一份磁盘文件;CN 余额的来源仍是 WB 面板 `/status`。
sidecar 的全部 env 见 `py/sidecar.py` 文件头注释。

## 五、运维语义

线上事故沉淀成规则表——改动任何一条之前先读这里的"为什么":

| # | 规则 | 事故来源与理由 |
|----|------|-----------|
| 1 | **客户端断连不冷却** | 一次断连风暴打出 1720×503(10-03):客户端断开被记进后端失败 → 正常后端挨个进冷却 → 全网无后端可用。断连只能归为 `network` 类观测,不吃冷却、不踩熔断 |
| 2 | **模型不存在是后端的锅** | 上游悄悄下架 model 时网关表现为 4xx/黑洞(10-06 起某兜底后端 0 成功率空转 4 天半才被发现)。这类 4xx 不能归给客户端;要落在对应后端头上走冷却/兜底摘除,身份走 `model_expire` / `model_fallback` 打补丁 |
| 3 | **配额类走指数退避,满载走排队** | 配额/权限类(403/429):指数冷却 5m→10m→…→4h 封顶,不反复白撞;**满载**不降级——排队到 60s(`QUEUE_WAIT`),而不是直接扔 503 |
| 4 | **黑名单只写不删,须人工清理** | `backends.blocked.json` 是启动自检判定的"永久失效"清单。没有自动解除路径:自动复白会用失败率当证据,而黑名单的判断依据是探测事实;摘条目必须人工复核后从文件里删 |
| 5 | **strike 只在成功时清零、勿乱改** | 10-01 的 403 自锁风暴:配额没恢复就把 strike 清零 → 后端带着空鼓回了池子 → 立刻再 403 → 冷却重启 → 循环打。指数退避的整条链依赖"连续击中数只靠成功归零",admin 热改不提供 strike 入口(设计如此,不是缺失) |
| 6 | **会话亲和=轮转外一层"优先选择"** | 10-10 生产实测前缀缓存命中率 0%:SWRR 把同一对话轮流甩给不同 provider——前缀缓存在 provider 侧,第二轮落到别家缓存就不在同一地。已建立过粘性的会话后续请求**优先钉回**同一 backend;新会话首落仍由 SWRR 权重控制。**只有最终成功且内容已交付才写表**(防失败会话钉死在坏后端);逐出/保持判定与冷却/熔断同源(`errclass.ReleaseAffinity`),不发明第二层健康层。**亲和键(10-18 起)优先 `X-Job-Id`(任务级,同一 RQ 任务的所有轮次共用一个 id,正是前缀缓存的复用单元),退回 `X-Session-Key`**——10-18 缓存调查:shadow 侧同前缀簇在 ds41 系后端命中 51~56% ≈ 其池内抽取概率,散装轮转就是天花板;键从「会话(没人发)」换成「job(SCF 发)」才接得上。SLR 侧同日上同款钉定(`JOB_AFFINITY`,照搬本表设计) |
| 7 | **按家怪癖进 quirk 层,内核零名字分支** | 内核曾 0 处按 provider 名分支——因为踩过的坑都被**泛化**成了通用规则(无条件删 Content-Length、只改 model 一个键、`session_header` 字段、通用词表),这四条泛化得对。但下一个坑如果不具备可泛化性,揉进通用规则会伤别家、加 `if name==xxx` 会把名字判定撒进内核:两条都是错路。第三条路是把「只在某家出现的问题」写进 `internal/core/quirk/xxx.go`,内核只在三个固定缝上查表(见下节) |
| 8 | **观测契约向 SLR 对齐(迁移前置)** | SLR→gateway 迁移(方案 A 端口接管)的前置工作,2026-10-18 落地两件:① `attempts` 表加 `job_id` 列(含对已存在库的幂等 ALTER 迁移)——按 job 归因/缓存分析的 SLR 分析脚本迁移后原样可用,这是 SLR 有而 gateway 缺的**唯一代码级 gap**;② `/api/stats` 增 SLR 同形 `affinity{hits,misses,puts,pinned_jobs}` 对象——`pinned_jobs` 取裸表长(Len 先于 CountPerBackend,后者顺带清 TTL,顺序反了口径就偏);`sticky_*` 三键是历史键名,看板还读着,保留。**口径注意**:`puts` 计每次真实 Put(含同 backend 刷新),与 SLR 逐位一致 |

### 怪癖层(`internal/core/quirk`):按家差异的落点

内核只在请求生命周期的**三个固定缝**上调它,接口只有三个方法:

| 缝 | 方法 | 时机与职责 | 默认实现的行为 |
|----|------|-----------|----------------|
| ① | `Decorate(req, backend)` | 出口前:通用 header 清理(删 Content-Length / 客户端凭据 / Accept-Encoding、固定 Content-Type / UA)**之后**、发往上游之前。可加该家专属的头/query、按条件裁 body | no-op |
| ② | `Classify(status, body) (Class, bool)` | 分类时:`status+body → errclass.Class`。返回 `(Class, false)` = **不归我管**,调用方走通用判定 —— 让一家只认领自己那几句文案,不抢通用词表 | 只认领 400/404 + 通用"模型下架"词表(词表仍住 `errclass`) |
| ③ | `ParseUsage(body) Usage` | 用量时:`body → Usage`(prompt/completion/total/缓存命中)。`OK=false` = 走 OpenAI 默认 | OpenAI usage + `prompt_tokens_details.cached_tokens` |

**默认 = 通用行为,没怪癖的上游零配置**:`quirk.Get("")`(后端没配 `driver`)
和未注册的 driver 名都回落到包级 `OpenAI` 单例,行为与不接怪癖层逐位一致。
注册表是按名查表 + `RWMutex`,内核零按名分支 —— 怪癖是**数据(配置字段)**,不是代码分支。

**怎么加一个新怪癖**(内核零改动):

1. 新建 `internal/core/quirk/xxx.go`:`type Xxx struct{ OpenAI }`(嵌基准实现),只覆盖有差异的方法;
2. 文件里 `func init() { Register(Xxx{}) }`;重复注册同名会 panic(防两家抢一个名字);
3. `backends.json` 里那条后端加 `"driver": "xxx"`(这是 **config 字段,不是 env**;
   不填 = 通用行为,写错名字也只是退化成通用行为、不会让网关起不来)。

举三个真实形态的例子:某家只回中文欠费文案(缝② 认领那几句 → 拉黑它的 403);
某家的"模型下架"报错体是自造词(缝② 认领 → 计入 fail_streak、连续失败自动熔断);
某家的缓存用量键叫 `prompt_cache_hit_tokens` 而不是 `cached_tokens`(缝③ 解析 →
缓存价计费不掉成 0)。**这批不实现任何具体怪癖**——纯接口预留,落点先立住。

### captures 消费契约(消费方先读这段,别从 Go 源码猜语义)

`captures` 是留存快照(旁路,与 `attempts` 用 rid 关联)。消费方(OpenViking / 面板 /
sidecar / 将来 RAG)判定一条留存**只用下面四条**,这四条是从真流量数据里逼出来的契约:

| # | 判据 | 正确用法 | 反例(别这么判) |
|----|------|----------|------------------|
| 1 | **非完整交付的正信号是 `usable=false`** | 完整=可用(`usable=1`);中途断连/被截断/未交付=不可用(`usable=0`)。断连/失败筛选用它 | ~~`partial_ratio`~~、~~`truncated`~~、~~status~~ 都不是"非完整"的正信号 |
| 2 | **`partial_ratio` 只回答"有没有内容",不承担严重性分级** | `0`=无内容;`>0`=有内容。注意 `partial_ratio=1.0` 是**完整交付的正常形态**(不是"全 partial") | 按 `partial_ratio` 分档推断严重程度是错的——它没有 P50 基线,将来也没有 |
| 3 | **`truncated=true` 是截断,不是失败** | 截断可能出现在完整交付里(正文本就巨大超 tee 上限)。截断≠丢弃,不可用性但仍按 #1 走 `usable` | 把 `truncated` 当失败/丢弃计数 |
| 4 | **同 `rid` 多条按 `ts` 排序,最后一条 `usable=true` 为最终交付** | 重试/partial 会让同一 rid 出现多条,这不是重复数据 | 看到多条就报"留存层写重了" |

一句话记忆:**可用性看 `usable`,内容有无看 `partial_ratio`,截断看 `truncated`,去重看 #4 的排序规则**——`status` 只在有实义 HTTP 码时参考(partial 中途断是 200,别用 status 当完成判据)。

## 六、目录映射

| 目录 | 里面是什么 | 为什么单独划一层 |
|------|-------------|--------------|
| `cmd/shg` | main 装配 + 优雅关闭 | 全项目唯一入口;业务全在 internal,这里零业务 |
| `internal/api` | 控制面 HTTP 门与看板 | 多个 handler 文件并这个层,与业务彻底分离 |
| `internal/service/routing` | 加权轮转 / 候选 / 兜底 / 原子换装 | 独立子包,可单独换实现 |
| `internal/service/proxy` | 数据面代理主流程 | 独立子包 |
| `internal/service/auth` | 登录抽象 + providers/ | 加登录方式只添文件 |
| `internal/service/apikey` | 下游 key 计费 | 独立子包 |
| `internal/core/errclass` | 上游错误分类唯一真源 + 全局默认词表 | core 底座 |
| `internal/core/quirk` | 上游怪癖层(接口预留 + OpenAI 基准实现) | 只依赖 core/errclass + core/upstream,站在 service 之下 |
| `internal/core/upstream` | 后端容量/健康状态机 | core 底座 |
| `internal/core/store` | SQLite 单库 + 单写入口 | core 底座 |
| `internal/config` | 配置加载 / 校验 / 黑名单 / 热加载 | 只依赖 runtime + core/errclass + core/upstream |
| `internal/runtime` | 身份 / 路径 / 轮转 / 清扫 / 保养 | **零项目内依赖**(SQLite 驱动例外,仅 housekeeping 使用) |
| `web/dashboard`,`web/console` | 运维看板 / 公版用户中心 | `go:embed` 进二进制 |
| `py/sidecar.py` | 配额守护(独立 Python 进程,不在 Go 侧) | 只 touch 网关俩 HTTP 接口 + 一份磁盘文件 |
| `deploy/` | Dockerfile + compose | 真实配置挂载卷注入,镜像零密钥(见 README 八) |

## 七、本地开发

```bash
gofmt -l internal/ cmd/ web/          # 必须为空
go vet ./...
go test ./... -race -count=1          # 上一次基线:230 条全绿
make build
```

不撞真上游的开发方法:全部走**假凭据** `sk-xxxxx`。请求会在上游网络错误/401 处失败,
正好顺序踩到错误分类、指数冷却、兜底摘除这些真实路径;单元测试已把路由/熔断/统计逐条
表驱动钉死。要跑不外发流量的 e2e,用 `ROUTER_BACKENDS` 直接注入一份指向
loopback 测试 HTTP 服务的假上游配置,配合 `ROUTER_VALIDATE=0`。

sidecar 依赖说明:见 `py/requirements.txt`——只有 Python 标准库,无 pip 安装项。

## 八、Docker 部署

镜像多阶段构建(构建段 `golang:1.27-alpine`,运行段 `alpine:3.20` + 非 root 用户),
纯 Go SQLite(`modernc.org/sqlite`,CGO_ENABLED=0),web 看板 `go:embed` 进二进制——
**镜像内只有代码,零配置零密钥**:

```bash
# 1. 准备真实配置(进挂载卷,不进 git)
mkdir -p runtime/config runtime/data
cp config/backends.example.json runtime/config/backends.json   # 改成真后端
cp config/keys.example.json    runtime/config/keys.json        # 改成真下游 key

# 2. 起容器(宿主侧默认 :18016,可用 SHG_PORT 覆盖)
docker compose -f deploy/docker-compose.yml up -d --build

# 3. 冒烟
curl -s http://127.0.0.1:18016/health
curl -s http://127.0.0.1:18016/api/stats | head -c 400
```

- 配置热加载照常生效:改 `runtime/config/backends.json`(mtime)无需重启容器
- 数据与日志落 `runtime/data/`(`shg.db` + logs),容器可随意重建
- 依赖仅为:ca-certificates(HTTPS 上游)+ tzdata(日志时区)
- 健康检查走 `GET /health`,compose 30s 间隔;`restart: unless-stopped`

宿主裸跑(不想用 docker 时):`make build` 后按 README 三设 env 启动,路径语义完全一致。
