English | [中文](README.md)

<h1 align="center">ShanHaiGateway · Shanhai Pass</h1>

<p align="center">Strings a pile of unreliable upstream model providers into one OpenAI-compatible endpoint.<br>
SWRR weighted rotation · exponential quota cooldown · task-level affinity · shadow capture observability · Docker to production in three minutes</p>

<p align="center"><i>"The lock and key of two capitals, second to none — the first pass of the ten-thousand-li Great Wall."</i></p>

<p align="center">Official site <a href="https://seika.ltd/">seika.ltd</a> · Source, docs and issue tracking live here on GitHub</p>

<p align="center">
  <a href="https://github.com/ffseika0304/shanhai-gateway"><img src="https://img.shields.io/github/stars/ffseika0304/shanhai-gateway?style=flat-square&logo=github" alt="GitHub Stars"></a>
  <a href="https://github.com/ffseika0304/shanhai-gateway/fork"><img src="https://img.shields.io/github/forks/ffseika0304/shanhai-gateway?style=flat-square&logo=github" alt="GitHub Forks"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue?style=flat-square" alt="License: MIT"></a>
  <a href="https://go.dev"><img src="https://img.shields.io/badge/go-%E2%89%A51.27-00ADD8?style=flat-square&logo=go" alt="Go ≥1.27"></a>
  <a href="#quick-start"><img src="https://img.shields.io/badge/docker-image%2038.9MB-2496ED?style=flat-square&logo=docker" alt="Docker"></a>
</p>

---

Every so often a vendor drops a generous free model. People farm it with a pile of accounts, resell access, the vendor notices — and the free tier dies.

I run the same playbook, with two differences: everything I learn is documented here for free, and I don't leech — the paid channels actually get paid (¥100+ a month out of pocket), and commercial lanes, vouchers and free pools share the load. In the October 2026 measurement this pushed 20 billion tokens a month for ≈ ¥170 (~US$24) in cash.

The price is a pile of failure modes. Every pit below was stepped on in production; each pit bought back one rule, and the rules all live in the ops-semantics table.

ShanHaiGateway is the gateway that grew out of those rules: it twists free pools, vouchers and pay-as-you-go plans into one stable upstream pool — catching failures for you and cutting the bill down.

## What problem does it solve

You wire three upstream providers into a rotation, and then this starts happening:

- **One upstream has a bad day and your whole product 503s with it.** You need failure handling smarter than `retry`: route by error class, exponential backoff for quota errors, queue on saturation, and for network errors merely observe without punishing anyone.
- **Prefix-cache hit rate never climbs.** Round-robin deals the second turn of the same conversation to a different provider — and prefix caches live provider-side. Measured: under naive rotation the same-prefix hit rate is 51–56%, exactly the in-pool sampling probability. **Rotation is the ceiling.**
- **An upstream silently delists a model, and a backend sits at 0% success rate for four and a half days before anyone notices.** A 4xx is not necessarily the client's fault; somebody has to claim those failures on the backend's behalf.
- **One disconnect storm prints 1720×503.** Client aborts get booked as backend failures, healthy backends drop into cooldown one by one, and soon no backend in the pool is selectable.
- **Keys scattered everywhere, and the repo can never see daylight.**

Each of the above maps to one rule in the ops-semantics table. Every rule was paid for with a real production incident — none of them are paper models.

## Features

- **SWRR weighted rotation + isolated fallback chain**: per-backend weight/concurrency/timeout; fallback backends live in their own pool and never pollute the main one
- **Error-taxonomy-driven cooldown**: 403/429 exponential backoff 5m→10m→…→4h cap; saturation queues for 60s instead of throwing 503; client disconnects never trigger cooldown
- **Task-level session affinity**: prefers `X-Job-Id`, falls back to `X-Session-Key`, pins every turn of a task back to the same backend — reconnecting the prefix-cache reuse unit (measured hit rate 27% → 51%, peaking at 69%)
- **Quirk layer**: vendor-specific error bodies / usage keys / egress tweaks live in their own package; zero name-based branching in the kernel; vendors without quirks need zero config
- **Multi-user downstream key billing**: `sk-user-*` quota issuance + pluggable login (registry pattern) + a stock user console
- **Shadow capture observability**: single SQLite store for attempts/captures (auto-scrubbed before write), fine-grained `/api/stats` dashboard
- **Hot config reload**: `backends.json` reloads on mtime — tune weights or add backends without restarting
- **Sidecar slow-loop self-healing**: an independent Python process (zero third-party dependencies) making rate-limit/degrade decisions from per-window failure rates, dividing labor with the gateway's fast path
- **Quota-window probe + automatic demotion**: `scripts/quota_probe.py` queries each vendor's 5h/7d/30d quota window per backend name and writes a `quota.json` snapshot every 180s; the sidecar judges every 60s — window exhausted, backend pulled; back under threshold, restored. Each subscription is metered independently; adding one changes no mapping table
- **Docker all the way**: multi-stage build, pure-Go SQLite (zero CGO), non-root runtime, healthcheck, zero secrets in the image

## Measured scale and bill (2026-10, production)

Someone runs 20 billion tokens a month through it in production for ≈ ¥170/month in cash. The same tokens at list price would run about $3,800/month (≈ ¥26,600).

### How the free pool carries 63% of traffic

One core capability of this gateway is blending highly unstable free/granted quotas into a highly available main pool. It does not distinguish main from menial — dispatch is fully random. The result:

- **Paid commercial channels (Volcengine Pro/Plan etc.)** carry only ~37% of real requests;
- **Free model pools from low-cost subscriptions, proxy pools, and cache reads** shoulder ~63% of throughput.

The free pool's 429s and false deaths are isolated by circuit breaking, 600-second two-sided timeouts and failure classification. The free pool errors constantly, yet main-business upstream 429s stay at 0.

**Why a collapsing free pool doesn't touch main business**: gateway and callers both carry a 600-second floor, so a "cut" on either end ends up as a re-send on another lane. When an upstream cuts actively, that lane cools down and the caller's retry lands elsewhere; when the gateway times out first, it re-sends on another lane internally — callers barely notice.

**One more subscription, one more free pool**: every subscription carries its own free model pool, so the paid share keeps falling (measured 43.6% → ~37% after adding one) while total throughput scales up. The bill follows which lane is cheap, not how many tokens you burned. Vendors are swappable route entries in config — never an architectural assumption.

### Real bill breakdown

At a production burn of ~1 billion tokens/day, with route spreading, cache affinity (70% hit rate) and rolling rotation of "multiple voucher accounts + free grant pools", actual monthly cash ≈ ¥170:

| Channel | Count | Monthly tokens | Monthly cash |
|---|---|---|---|
| Volcengine subscription | 4 | 4 × 2B = **8B** | 2 paid in cash **¥99.8**, the other 2 covered by two ¥50 channel vouchers (¥10 left on them) |
| opencode Go | 2 | 2 × 600M paid tokens = **1.2B** | bought at half price, **$5 each** (≈¥70) |
| Free grant pools · credit earning · cache reads | — | the remaining **~63%** | ¥0 |

→ ¥170 for 20B tokens ≈ ¥0.85 per 100M; only 23% of tokens are actually billed at full fresh-token rates. The same volume fully paid at list price ≈ $3,800/month. Code alone (cache affinity + free-tier blending) plus pool-level per-request-size splitting cuts list-price volume to 1/4–1/3; stacking channel strategy brings total cost down 99%.

### How it actually does that

| Capability | What it does | Why this is architecture, not a config trick |
|---|---|---|
| **Internal session affinity** | every turn of the same task (`X-Job-Id`, falling back to `X-Session-Key`) is pinned to the same lane | rotation throws the second turn at another vendor and prefix caching dies. Measured: naive rotation gives a 51–56% same-prefix hit rate — exactly the in-pool sampling probability, rotation is the ceiling; pinned, it goes 27%→51%, peaking at 69% |
| **External session header passthrough** | per-backend `session_header` (e.g. `x-opencode-session`) writes the session value into the vendor's own header | some vendors only honor their own header for caching: without the field, their side never hits. Set the field, change no code (one line in `attempt.go`) |
| **Quirk layer** | vendor-specific error bodies / usage keys / egress tweaks go into a dedicated package (`core/quirk`); the kernel has zero name-based branches | vendors without quirks: zero config; with quirks: one file. Default behavior = generic behavior — not wiring the quirk layer is bit-for-bit identical to before |
| **Independent per-subscription metering + quota-window auto-demotion** | the probe queries each vendor's 5h/7d/30d windows grouped by backend name and writes a `quota.json` snapshot every 180s; the sidecar judges every 60s: exhausted → pull the backend, back under threshold → restore | adding a subscription touches no mapping table. Rotating an expiring subscription = disable one, enable one, with staggered cycles so every one still gets burned clean |
| **Per-request-size splitting (pool config layer, not the kernel)** | small requests go to per-token pools, large ones to per-call pools (22,222-token balance point) | the two billing regimes have different unit costs; picking pools by size beats random dispatch — measured 35% savings. Weights and multi-backend config do this; the kernel only keeps each lane stable |
| **Free/budget lane blending** | free grant pools rotate in the same pool as commercial lanes | free quota is the least stable — but once its collapse can't touch main business (see above), it has earned regular-army status |

> Wiring in a subscription takes two steps; everything else is automatic. ① Add an entry to `accounts[]`: a unique name + `api_keys[]` (for calls) + quota-query credentials (AK/SK, delivered with the deployment config; if missing, the dashboard reports "missing credentials" and takes no action — it never borrows someone else's credentials). ② Add a backend entry to `backends[]`, with `base_url` pointing at that vendor's plan endpoint and `account` pointing at the entry above. It hot-reloads online within 30s; the probe starts querying its quota windows by backend name, and the sidecar starts its "exhausted → pull, restored → release" loop. Swapping an expiring subscription: create the new account + backend, disable the old one (stagger the cycles so each one gets burned clean).

> Disclaimers — please read them together with the numbers.
> ① Promo prices have shelf lives: vouchers and half-price deals can vanish anytime; back at list price it's ≈ ¥340/month — still 98%+ saved.
> ② "Free/budget lanes carry 63%" fluctuates with upstream grants and earn rates (measured range 37–57%, higher if cache reads are counted). No specific quota is promised.
> ③ Free/granted quota is the upstream's gift, not this project's. What this project open-sources, forever, is cache affinity, lane-switching retries, and independent per-subscription metering with automatic demotion.
> ④ The numbers are single-machine production measurements from 2026-10 (daily burn 440M–1.3B tokens), consistent in caliber with the table above; the measurement methodology ships with each release's calibration record.
> ⑤ No vendor lock-in: the main-pool share swung between 11%↔43% over three days (what changed: accounts and weights in config) — routing / cache affinity / fallback logic: not one line changed.

## Two kinds of readers, two entrances

**You just want a working gateway**: three Docker commands stand up an OpenAI-compatible endpoint; swap the `base_url` and go. Weights, cooldown and affinity all ship with sane defaults — you can skip the architecture section.

**You're pinning it into production, or rewriting the router**: the architecture diagram, the package responsibility table, the eight ops rules, the quirk-layer interface and the captures consumption contract are all below. Every magic number has an incident citation for why it is what it is.

---

## Quick start

### Docker (recommended)

```bash
git clone https://github.com/ffseika0304/shanhai-gateway.git && cd shanhai-gateway

# 1. Prepare real config (goes into the mounted volume, never into git)
mkdir -p runtime/config runtime/data
cp config/backends.example.json runtime/config/backends.json   # your backends
cp config/keys.example.json    runtime/config/keys.json        # your downstream keys

# 2. Start the container (host side defaults to :18016; override with SHG_PORT)
docker compose -f deploy/docker-compose.yml up -d --build

# 3. Smoke test
curl -s http://127.0.0.1:18016/health          # {"status":"ok","version":"0.5.1"}
curl -s http://127.0.0.1:18016/api/stats | head -c 400
```

### Bare binary

Requires **Go ≥ 1.27**:

```bash
make build                          # equivalent to: go build -o shg ./cmd/shg

cp config/backends.example.json config/backends.json   # edit the fields; fake credentials boot fine
export LISTEN=":18006"              # LAN no-auth port
export ROUTER_VALIDATE=0            # skip the startup outbound self-check (remove once real keys are in)

./shg &
curl -s http://127.0.0.1:18006/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"demo-model","messages":[{"role":"user","content":"hi"}]}'
# 200 means through. Health check: GET /health; traffic dashboard: GET /api/stats
```

The LAN port's `POST /v1/chat/completions` does not require a key (the free lane); downstream key billing only activates on the public port / user console.

### Two forms, one path semantics

| | Bare | Docker |
|---|---|---|
| Config | `<repo>/config/` (real files gitignored) | `./runtime/config/` → mounted at `/etc/shg` |
| Data/logs | `~/.local/share/shg/` | `./runtime/data/` → mounted at `/var/lib/shg` |
| Identity | `shg/0.5.1` (UA / `/healthz` / log prefix) | same |
| Hot reload | mtime takes effect, no restart | mtime takes effect, no container restart |

---

## Architecture

Dependency direction is one-way: `api → service → core`; `config` / `runtime` are horizontal feeders.

```
        ┌─────────────────────────────────────────────────────┐
        │              cmd/shg — assembly only                │
        │     env → paths → dirs → SQLite → two listeners     │
        └───────────────────────────┬─────────────────────────┘
                                    ▼
┌─────────────────────────────────────────────────────────────────┐
│ internal/api    control-plane HTTP gates: LAN(free)/public(dashGate+key) │
│   usercenter / admin / dashboard / dashgate / public gates       │
└──────────────┬──────────────────────────────┬───────────────────┘
               ▼                              │
┌─────────────────────────────────────────────┼───────────────────┐
│ internal/service      data-plane + control-plane business        │
│   routing (weighted rotation/fallback)   proxy (main flow)       │
│   auth (pluggable login)   apikey (downstream key billing)       │
└──────────────┬──────────────────────────────┼───────────────────┘
               ▼                              │
┌─────────────────────────────────────────────┼───────────────────┐
│ internal/core        errclass (status-code taxonomy)             │
│          upstream (capacity/health state machine) store (SQLite) │
└──────────────┬───────────────────────────────┼───────────────────┘
               │   horizontal feeders (independent of each other and of service) │
┌──────────────┼───────────────────────────┬───┴───────────────┐
│ internal/config   backends.json load/validate/mtime hot-reload/blocklist │
│ internal/runtime   identity/paths/log rotation/sweep/SQLite upkeep │
└─────────────────────────────────────────────────────────────────┘
```

Each package is split out because different maintainers touch it at different times, for different reasons:

| Package | One-line job | Why it must evolve independently |
|------|--------|-----------|
| `api` | Gates and wiring: the LAN free lane's tone, the public login gate, dashboard data assembly | "who may enter" has nothing to do with "how a request is routed"; entrances, ports and frontend changes stay here |
| `service/routing` | Who to pick: weighted rotation, candidate filtering, fallback isolation, atomic swap | the routing algorithm can be replaced wholesale (P2C, least-connections) without dragging core or the config format along |
| `service/proxy` | Send it out: request rewriting, streaming forward, queue timeouts, disconnect detection | data-plane protocol details evolve alone; the routing decision layer must not sense them |
| `service/auth` | Who may enter: pluggable login methods (registry pattern) | adding a login method = one new provider file, zero changes in other layers |
| `service/apikey` | The billing boundary: `sk-user-*` downstream key quota issuance | billing policy changes stay in this layer |
| `core/errclass` | Upstream error status classification (single source of truth) + the global default vocabulary | classification changes ripple into cooldown/circuit/stats/fallback at once — it must be defined in exactly one place |
| `core/quirk` | Upstream quirks: problems only one vendor has (egress tweaks / private error bodies / private usage keys) | per-vendor differences evolve independently; the kernel knows only the interface with zero name branches, and the default implementation = generic behavior |
| `core/upstream` | The backend itself: concurrency slots, circuit breaker, exponential cooldown, per-class failure counts | every magic number was tuned by a production incident; changes deserve separate review |
| `core/store` | One SQLite database: attempts, users, sessions, single write entry | schema evolution and business logic don't drag each other; single-writer is the foundation of concurrency safety |
| `config` | Define config: what counts as a legal backend; dead ones enter the blocklist | config format changes must not force the service layer to follow; blocklist policy is reviewed separately |
| `runtime` | Program identity and disk layout: paths/env/rotation/sweep/upkeep | rename, move, release a version — one package changes, the whole program follows |
| `cmd/shg` | Assembly only, no business | always thin: business sinks, only startup and shutdown order live here |

---

## Configuration

The most minimal `backends.json` (fake credentials; the full field set is in `config/backends.example.json`):

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

### Environment variables

| Variable | Path/behavior it controls | Default |
|-------------|----------|--------|
| `GATEWAY_DATA_DIR` | data root | `~/.local/share/shg` |
| `GATEWAY_CONFIG_DIR` | config directory | `<repo>/config` |
| `GATEWAY_DB_FILE` | main SQLite database | `<dataDir>/shg.db` |
| `GATEWAY_BACKENDS_FILE` | backend baseline file | `<configDir>/backends.json` |
| `GATEWAY_KEYS_FILE` | downstream key ledger | `<configDir>/keys.json` |
| `GATEWAY_QUOTA_FILE` | quota snapshot | `<dataDir>/quota.json` |
| `ROUTER_BACKENDS` | inject JSON config directly (takes priority over files) | — |
| `ROUTER_VALIDATE` | `0` skips the startup self-check | `1` |
| `LISTEN` / `LISTEN_PUBLIC` | LAN free port / public port | `:18006` / not listening |
| `SESSION_AFFINITY` | `off` disables session affinity (bit-identical to not having it); `AFFINITY_TTL/AFFINITY_MAX/AFFINITY_CONTENTION_LIMIT` are entry lifetime / LRU capacity / full-load hysteresis migration threshold | `on` / `6h` / `8192` / `3` |
| `REASONING_EFFORT` | uniformly lowers the reasoning budget for thinking models: injects the default when the client didn't explicitly set `reasoning_effort` (explicit values are respected, never overridden); empty string = inject nothing. If an upstream layer (another gateway/router) already injected it, the guard detects "already present" and skips — chained gateways never double-inject | `low` |

> The env prefix is `GATEWAY_*`, not `SHG_*`: the sidecar contract (`py/sidecar.py` reads `GATEWAY_BACKENDS_FILE` / `GATEWAY_QUOTA_FILE`) is anchored to these names — renaming has zero benefit and positive migration risk. Identity lives in `internal/runtime/identity.go`'s `AppName` (`"shg"`), not in the env prefix.

`backends.blocked.json` has no separate env variable: it is bound to the same directory as `backends.json` and must move together with it.

### Secrets and data leaving the repo

There are no secrets in the repo or the image. That is the floor for self-hosted tools, and what makes open-sourcing this possible at all:

- Real `backends.json` / `keys.json` / `backends.blocked.json` / `quota.env` are all gitignored and live only in runtime locations (bare: `config/`; docker: the mounted `runtime/config/`). The repo only carries `*.example.json` templates.
- Data (`*.db` / logs) never enters git: captures contain request bodies — that is production data, not config. Disaster recovery = code + the runtime config directory, and the service resurrects.
- A real key appearing in any file in this repo is a bug; found means REDACTED on sight.

---

## Ops semantics: the rules table paid for by production incidents

> Lineage note: SLR in this table refers to this project's early in-house front router (seika_llm_router), fully replaced by this gateway; the entries are kept for semantic lineage.

Read the "why" here before changing any of them:

| # | Rule | Incident source and reasoning |
|----|------|-----------|
| 1 | **Client disconnects never cool down** | one disconnect storm printed 1720×503 (Oct 3): client aborts were booked as backend failures → healthy backends cooled down one by one → no backend left in the pool. Disconnects may only be classified as `network` (observed) — no cooldown, no circuit |
| 2 | **"Model not found" is the backend's fault** | when an upstream silently delists a model, the gateway sees 4xx/black-hole (from Oct 6 a fallback backend idled at 0% success rate for 4.5 days before anyone noticed). This kind of 4xx cannot be charged to the client; it must land on the owning backend and walk cooldown/fallback demotion, with `model_expire` / `model_fallback` identities patching it |
| 3 | **Quota errors back off exponentially; saturation queues** | quota/permission class (403/429): exponential cooldown 5m→10m→…→4h cap, no repeated blind retries; saturation does not degrade — it queues up to 60s (`QUEUE_WAIT`) instead of throwing 503 |
| 4 | **The blocklist is write-only; cleanup is manual** | `backends.blocked.json` is the "permanently dead" list decided by startup self-check. There is no automatic unblock path: auto-rewhitening would use failure rate as evidence, but the blocklist's judgment is probe fact; removing an entry requires human review, then deletion from the file |
| 5 | **Strikes reset only on success — don't touch them** | the Oct 1 403 self-lock storm: strikes were zeroed before quota recovered → the backend returned to the pool running on empty → instant 403 again → cooldown restart → loop. The whole exponential-backoff chain depends on "consecutive strikes zeroed only by success"; admin hot-editing deliberately provides no strike entry (by design, not an omission) |
| 6 | **Session affinity = a preference layer outside rotation** | Oct 10 production measurement: prefix-cache hit rate 0% — SWRR deals consecutive turns of one conversation to different providers; prefix caches live provider-side, so the second turn lands where the cache isn't. Sessions that established stickiness get pinned back to the same backend on subsequent requests; a new session's first landing still follows SWRR weights. Only a final success with delivered content writes the table (keeps failed sessions from pinning to a bad backend); eviction/keep decisions share one source with cooldown/circuit (`errclass.ReleaseAffinity`) — no second health layer is invented. The affinity key prefers `X-Job-Id` (task-level: all turns of one task share one id — exactly the prefix-cache reuse unit), falling back to `X-Session-Key` |
| 7 | **Per-vendor quirks go in the quirk layer; zero name branches in the kernel** | the kernel once had zero provider-name branches, because every pit stepped on had generalized into a universal rule (unconditional Content-Length removal, model-only rewrite, the `session_header` field, the common vocabulary) — and those four generalizations were correct. But if the next pit doesn't generalize, folding it into universal rules hurts everyone else, and adding `if name==xxx` sprinkles name checks into the kernel — both wrong roads. The third road: write "problems only one vendor has" into `internal/core/quirk/xxx.go`; the kernel consults it at three fixed seams (next section) |
| 8 | **The observability contract stays SLR-isomorphic** | ① the `attempts` table carries a `job_id` column (idempotent ALTER migration for existing databases) — scripts attributing by job or analyzing caches keep working after migration; ② `/api/stats` exposes the SLR-isomorphic `affinity{hits,misses,puts,pinned_jobs}` object, `pinned_jobs` reads the bare table length (Len before CountPerBackend — the latter clears TTLs along the way; reversed order skews the metric); the three `sticky_*` keys are historical names the dashboard still reads — kept. Caliber note: `puts` counts every real Put (including same-backend refresh), bit-identical to SLR |

### The quirk layer (`internal/core/quirk`): where per-vendor differences land

The kernel calls it at exactly three fixed seams of the request lifecycle; the interface has three methods:

| Seam | Method | Timing and job | Default behavior |
|----|------|-----------|----------------|
| ① | `Decorate(req, backend)` | egress: after generic header cleanup (strip Content-Length / client credentials / Accept-Encoding, pin Content-Type / UA) and before the request goes upstream. May add vendor-specific headers/queries, or trim the body conditionally | no-op |
| ② | `Classify(status, body) (Class, bool)` | classification: `status+body → errclass.Class`. Returning `(Class, false)` = not mine; the caller falls through to generic judgment, letting one vendor claim only its own phrases without grabbing the common vocabulary | claims only 400/404 + the generic "model delisted" vocabulary (the vocabulary still lives in `errclass`) |
| ③ | `ParseUsage(body) Usage` | usage: `body → Usage` (prompt/completion/total/cache hits). `OK=false` = use the OpenAI default | OpenAI usage + `prompt_tokens_details.cached_tokens` |

The default is generic behavior: upstreams without quirks need zero config. `quirk.Get("")` (a backend without `driver`) and unregistered driver names both fall back to the package-level `OpenAI` singleton — bit-for-bit identical to not wiring the quirk layer. The registry is a name lookup + `RWMutex`; the kernel has zero name branches. Quirks are data (config fields), not code branches.

How to add a new quirk (kernel untouched):

1. Create `internal/core/quirk/xxx.go`: `type Xxx struct{ OpenAI }` (embed the baseline implementation), override only the differing methods;
2. In the file, `func init() { Register(Xxx{}) }`; duplicate registration of the same name panics (keeps two vendors from fighting over one name);
3. Add `"driver": "xxx"` to that backend in `backends.json` (this is a config field, not an env var; omitted = generic behavior, and a misspelled name merely degrades to generic behavior — the gateway still boots).

Three real-world shapes: one vendor returns its arrears notices only in Chinese (seam ② claims those phrases → its 403s are treated as vendor-fault); one vendor's "model delisted" body uses an invented word (seam ② claims it → counts into fail_streak, auto-circuits after consecutive failures); one vendor's cache usage key is `prompt_cache_hit_tokens` instead of `cached_tokens` (seam ③ parses it → cache-priced billing doesn't fall to 0). The default distribution implements no concrete quirk — it is pure interface reservation with the landing spot staked out.

### The captures consumption contract (consumers read this first; don't guess semantics from Go source)

`captures` are retained snapshots (a bypass path, associated with `attempts` via `rid`). Judge a retained record by exactly the four clauses below — this contract was squeezed out of real traffic data:

| # | Criterion | Correct use | Counter-example (don't) |
|----|------|----------|------------------|
| 1 | **The positive signal for non-complete delivery is `usable=false`** | complete = usable (`usable=1`); mid-stream disconnect / truncated / undelivered = unusable (`usable=0`). Filter disconnects/failures by it | ~~`partial_ratio`~~, ~~`truncated`~~, ~~status~~ are not positive signals for "incomplete" |
| 2 | **`partial_ratio` only answers "is there content"; it does no severity grading** | `0` = no content; `>0` = content present. Note `partial_ratio=1.0` is the normal shape of complete delivery (not "all partial") | grading severity by `partial_ratio` is wrong; it has no P50 baseline and never will |
| 3 | **`truncated=true` means truncated, not failed** | truncation can appear inside complete delivery (the body is simply huge and overflows the tee cap). Truncated ≠ dropped; usability still follows #1 via `usable` | counting `truncated` as failure/drop |
| 4 | **Multiple rows under one `rid` sort by `ts`; the last `usable=true` row is the final delivery** | retries/partials produce multiple rows per rid — not duplicate data | seeing multiples and reporting "the capture layer double-wrote" |

One-line mnemonic: usability → `usable`; content presence → `partial_ratio`; truncation → `truncated`; dedup → #4's ordering rule. `status` is reference only when it carries a meaningful HTTP code (a mid-partial disconnect is a 200 — don't use status as the completion test).

---

## Related services: wb-panel / wb2api (the WorkBuddy family)

The "WorkBuddy Panel" item in the dashboard nav is a separate project, not part of this repo:

- **What it is**: WorkBuddy2API Panel — a multi-account gateway + web ops panel that wraps Tencent CodeBuddy accounts into an OpenAI-compatible API (account pool / circuit breaking / session stickiness / keep-alive)
- **Upstream repo**: <https://github.com/linguo2625469/workbuddy2api-panel> (forked from [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api), enhanced branch)
- **Wiring**: UI and API share one port (process wb2api, bound to `*:7863`); panel at `http://<host>:7863/panel/`, API source `http://127.0.0.1:7863/v1` (backends with `account=workbuddy` in the config all go through here). The dashboard nav points at same-host `:7863/panel/` by default; if the panel lives on another machine, run `localStorage.setItem("wbPanelUrl","http://<panel-host>:7863/panel/")` once in the browser console to override (the real address is never persisted). History note: the old panel UI listened on `:18011` separately; current builds have merged into 7863 — any document still saying 18011 is outdated
- **Ops discipline**: wb-panel / wb2api do not restart with this gateway; upgrades on either side stay independent

## Troubleshooting

### The gateway won't start, complaining "no backend config"

env points at the wrong directory. `GATEWAY_CONFIG_DIR` must contain `backends.json`; in docker, check that the mounted `runtime/config/` really has files (`docker logs shg` shows how many backends were parsed at startup).

### A backend never gets picked

Check three fields in `GET /api/stats`: `enabled` (disabled in config), `cooldown_remain_s` (in quota-class cooldown; exponential, up to 4h), `circuit_open`. Note that `circuit_open:false` does not mean no cooldown — cooldown reads `cooldown_remain_s`; the two are separate counters.

### Every backend is cooling down and all requests fail

This is exactly the shape rule #1 exists to prevent. Check `fail_breakdown`: lots of `network` failures usually means a client disconnect storm misfiring (old-version semantics) — confirm you are running the current version of this repo; a concentrated burst of 401s means dead keys — cooldown will not heal until you replace them.

### The dashboard has data and I want the raw requests

Query the SQLite `captures` table (read the captures consumption contract above first). Everything is scrubbed before storage (keys / intranet paths / phone numbers / Bearer headers → `[mask]`) — that is a feature, not a flaw.

### The docker container keeps restarting

`docker logs shg` for the exit reason; most likely `runtime/config/` is missing files or has invalid JSON. A failing healthcheck also makes the orchestrator mark it unhealthy, but `restart: unless-stopped` only watches process exit.

## Project structure

```
shanhai-gateway/
├── cmd/shg/                   # main assembly + graceful shutdown (the project's only entry point)
├── internal/
│   ├── api/                   # control-plane HTTP gates and the dashboard
│   ├── service/
│   │   ├── routing/           # weighted rotation / candidates / fallback / atomic swap
│   │   ├── proxy/             # data-plane main proxy flow
│   │   ├── affinity/          # task/session affinity table
│   │   ├── auth/              # login abstraction + providers/
│   │   └── apikey/            # downstream key billing
│   ├── core/
│   │   ├── errclass/          # single source of truth for error classes + vocabulary
│   │   ├── quirk/             # upstream quirk layer (interface reservation + OpenAI baseline)
│   │   ├── upstream/          # backend capacity/health state machine
│   │   └── store/             # single SQLite DB + single write entry
│   ├── config/                # config loading / validation / blocklist / hot reload
│   └── runtime/               # identity / paths / rotation / sweep / upkeep
├── web/dashboard, web/console # ops dashboard / user console (go:embed into the binary)
├── py/sidecar.py              # quota guardian (independent Python process, stdlib only)
├── scripts/                   # quota_probe.py quota-window probe (180s snapshots into quota.json)
├── config/                    # only *.example.json templates; real config is gitignored
├── deploy/                    # Dockerfile + docker-compose.yml
├── API_CONTRACT.md            # api.seika.ltd interface contract
├── Makefile                   # build / test / fmt / vet / docker
└── README.md
```

## Local development

```bash
gofmt -l internal/ cmd/ web/          # must be empty
go vet ./...
go test ./... -race -count=1          # baseline: 12 packages green
make build
```

Developing without touching real upstreams: everything runs on fake `sk-xxxxx` credentials. Requests fail at upstream network errors / 401, which walks you through error classification, exponential cooldown and fallback demotion — the real paths, in order. Unit tests pin routing/circuit/stats table-driven, item by item. For a no-egress e2e, use `ROUTER_BACKENDS` to inject a fake upstream config pointing at a loopback test HTTP server, together with `ROUTER_VALIDATE=0`.

Sidecar dependencies: see `py/requirements.txt` — Python standard library only, nothing to pip install.

## Security and responsibility

ShanHaiGateway is a self-hosted tool; how you deploy it and configure credentials is entirely your call:

- **Zero secrets in the repo and the image**: no keys hardcoded in source, templates, or the Dockerfile; real credentials live only in your own mounted volumes / local directories, never in git history (this repo's history is fresh-start by design)
- **captures are auto-scrubbed**: request/response bodies pass an ordered rule table before storage (keys, intranet paths, phone numbers, auth headers → `[mask]`); but retention is inherently sensitive data — safekeeping the database file is on you
- On the public port, enable the login gate + downstream key billing; the LAN free lane is for trusted segments only. Whether to add HTTPS or IP allowlists depends on your exposure surface

> Disclaimer: this project is provided "as is". The author is not liable for any direct or indirect consequences of use, misconfiguration, failure to harden, upstream credential leaks, data loss, or the like. Deploying means you understand and accept the risks.

## Contributing

[Issues](https://github.com/ffseika0304/shanhai-gateway/issues) and pull requests are welcome. Before changing routing semantics or cooldown parameters, read [Ops semantics](#ops-semantics-the-rules-table-paid-for-by-production-incidents) first. Every rule there has a real incident behind it — don't let the next generation of traffic pay the same tuition twice.

## License

[MIT](LICENSE) — take it, change it, ship it.

## Acknowledgments

- [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) — pure-Go SQLite, keeping the image zero-CGO
- [rs/cors](https://github.com/rs/cors) — CORS middleware
- [workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) and its enhanced branch — the WorkBuddy account-pool ecosystem (see Related services)

---

If it saves you money, a star helps the next person find it.
