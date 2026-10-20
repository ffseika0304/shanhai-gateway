// admin.go —— /admin/backend/{name} GET+POST：运行态调参
// （搬自 seika_llm_router main.go 的注册段，逻辑原样）。
//
// ⚠️ **两套机制并存，语义各有归属，别合并**（2026-10-11 钉死）：
//
//  1. admin 热改（本文件）改的是**内存里的 backend 实例状态**
//     （concurrency / weight / enabled）。立即生效；进程重启回落配置；
//     config 热加载触发 Router 重建后同样回落（新实例是按配置建的）。
//     用途：线上现场临时止血（某个上游抽风时临时关掉），不是持久配置。
//
//  2. config 热加载（config.Store.ReloadIfChanged，cmd/gateway 后台
//     goroutine）读的是**文件里的 baseline**（backends.json）。改后
//     重建 Router（原子换装，见 routing/atomic.go），带黑名单过滤。
//     用途：增删后端、改账号 key 池、改模型 —— 改文件的事归它管。
//
//     把这两套合成一套（admin 直接写回 backends.json）是**错误**的方向：
//     运维随手救火一次，就会把临时值固化进基线，事后没人记得它为什么是 2。
//
// 鉴权：本端点没有自己的密码 —— 挂在共用 mux 上；公网口整条链过
// dashGate（不登录摸不到），LAN 口免鉴权定调不变（文档 §九）。
package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

// handleAdminBackend GET/POST /admin/backend/{name}，POST /admin/backend/{name}/clear。
//
// 「/clear」走**独立后缀**而非 query 参数 —— 因为 backend 名字本身可以含
// 任意字符（含点号/斜杠是不合法配置的既有责任），带后缀匹配最省事且与
// 原实现逐字一致。
func (h *Handlers) handleAdminBackend(w http.ResponseWriter, r *http.Request) {
	rt := h.Router.Load()
	name := strings.TrimPrefix(r.URL.Path, "/admin/backend/")

	// {name}/clear：手动清冷却——原实现把这条分支放在普通 GET/POST 之前
	if strings.HasSuffix(name, "/clear") {
		name = strings.TrimSuffix(name, "/clear")
		b := rt.ByName(name)
		if b == nil {
			http.Error(w, `{"error":"unknown backend"}`, http.StatusNotFound)
			return
		}
		b.ClearCooldown()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "name": name})
		return
	}

	// {name}/trip：手动拉爆一个后端（强制熔断打开）——看板「手动拉爆」的落点。
	// 走 core/upstream 已有的 b.Trip(now)：与真实失败触发的熔断同源，恢复仍靠
	// ClearCooldown / 失败计数归零，不发明第二套健康状态。
	if strings.HasSuffix(name, "/trip") {
		name = strings.TrimSuffix(name, "/trip")
		b := rt.ByName(name)
		if b == nil {
			http.Error(w, `{"error":"unknown backend"}`, http.StatusNotFound)
			return
		}
		b.Trip(time.Now())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "name": name, "tripped": true})
		return
	}

	b := rt.ByName(name)
	if b == nil {
		http.Error(w, `{"error":"unknown backend"}`, http.StatusNotFound)
		return
	}

	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"name": b.Name, "concurrency": b.Concurrency, "weight": b.Weight,
			"enabled": b.Enabled, "in_flight": b.InFlight(),
		})
		return
	}

	if r.Method == http.MethodPost {
		var patch struct {
			Concurrency *int     `json:"concurrency"`
			Weight      *float64 `json:"weight"`
			Enabled     *bool    `json:"enabled"`
		}
		if json.NewDecoder(r.Body).Decode(&patch) != nil {
			http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
			return
		}
		// 就地改内存态（见文件头：这是运行态语义，不回写配置文件）。
		if patch.Concurrency != nil {
			if *patch.Concurrency < 1 {
				*patch.Concurrency = 1
			}
			b.Concurrency = *patch.Concurrency
		}
		if patch.Weight != nil {
			b.Weight = *patch.Weight
		}
		if patch.Enabled != nil {
			b.Enabled = *patch.Enabled
		}
		log.Printf("[admin] backend %s -> conc=%d weight=%.2f enabled=%v",
			b.Name, b.Concurrency, b.Weight, b.Enabled)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "ok", "name": b.Name})
		return
	}

	http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
}
